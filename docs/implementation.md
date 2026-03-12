## 구현 설계

### 기술 스택

| 항목 | 선택 | 이유 |
|------|------|------|
| 언어 | Go | 고루틴 기반 동시성, net 라이브러리 성숙도, 단일 바이너리 배포 |
| DB 프로토콜 | PostgreSQL wire protocol | 클라이언트가 일반 PG 드라이버로 접속 가능 |
| 설정 | YAML (`gopkg.in/yaml.v3`) | 스펙 설정 예시와 일치 |
| 캐시 | 인메모리 (`sync.Map` + container/list`) | 외부 의존 없이 LRU 구현 |
| SQL 파서 | `pg_query_go/v5` (cgo) | PostgreSQL 실제 C 파서 바인딩, AST 분류/방화벽/시맨틱 키 |
| 로깅 | `slog` (표준 라이브러리) | 구조화 로깅, 외부 의존 없음 |

---

### 프로젝트 구조

```
pgmux/
├── cmd/
│   └── pgmux/
│       └── main.go              # 진입점: 설정 로드 → 서버 시작
├── internal/
│   ├── config/
│   │   └── config.go            # YAML 파싱, 설정 구조체 정의
│   ├── proxy/
│   │   ├── server.go            # Server 구조체, NewServer, Start, handleConn, Reload
│   │   ├── auth.go              # 인증 핸드셰이크 (relayAuth, frontendAuth)
│   │   ├── query.go             # 메인 쿼리 루프 (relayQueries)
│   │   ├── query_read.go        # 읽기 쿼리 처리 (handleReadQuery, handleReadQueryTraced)
│   │   ├── query_extended.go    # 확장 쿼리 프로토콜 (Prepared Statement 라우팅)
│   │   ├── copy.go              # COPY IN/OUT/BOTH 릴레이
│   │   ├── backend.go           # 백엔드 커넥션 관리 (acquire, reset, fallback)
│   │   ├── lsn.go               # LSN 폴링 (Causal Consistency)
│   │   ├── helpers.go           # 유틸리티 (sendError, parseSize, emitAuditEvent)
│   ├── pool/
│   │   ├── pool.go              # 커넥션 풀 핵심 로직
│   │   └── health.go            # 헬스체크 고루틴
│   ├── router/
│   │   ├── router.go            # Writer/Reader 라우팅 결정 (Causal Consistency)
│   │   ├── parser.go            # 문자열 기반 쿼리 분류
│   │   ├── parser_ast.go        # AST 기반 쿼리 분류 (pg_query_go)
│   │   ├── ast.go               # SQL AST 파싱 + 깊이 우선 노드 순회
│   │   ├── balancer.go          # Reader 라운드로빈 + LSN-aware 라우팅
│   │   ├── lsn.go               # PostgreSQL LSN 타입 파싱/비교
│   │   └── firewall.go          # 쿼리 방화벽 (위험 쿼리 차단)
│   ├── cache/
│   │   ├── cache.go             # LRU 캐시 구현
│   │   ├── invalidator.go       # 쓰기 시 테이블별 캐시 무효화
│   │   └── normalize.go         # 시맨틱 캐시 키 (AST Fingerprint)
│   ├── audit/
│   │   └── audit.go             # 비동기 감사 로그 + Slow Query + Webhook
│   └── dataapi/
│       └── handler.go           # Serverless Data API HTTP 서버
├── deploy/
│   └── helm/
│       └── pgmux/            # Helm Chart (Chart.yaml, values.yaml, templates/)
├── Dockerfile                   # Multi-stage 빌드
├── docs/
│   ├── spec.md
│   └── implementation.md
├── config.yaml                  # 기본 설정 파일
├── go.mod
└── go.sum
```

---

### 1. 커넥션 풀링 구현

#### 핵심 자료구조

```go
type Pool struct {
    mu          sync.Mutex
    idle        []*Conn          // 유휴 커넥션 슬라이스
    numOpen     int              // 현재 열린 총 커넥션 수
    maxOpen     int              // max_connections
    minOpen     int              // min_connections
    maxLifetime time.Duration    // max_lifetime
    idleTimeout time.Duration    // idle_timeout
    connTimeout time.Duration    // connection_timeout
    waitQueue   chan *Conn       // 풀 가득 찬 경우 대기 채널
}

type Conn struct {
    net.Conn
    createdAt  time.Time         // 생성 시각 (max_lifetime 판단용)
    lastUsedAt time.Time         // 마지막 사용 시각 (idle_timeout 판단용)
}
```

#### 커넥션 획득 흐름

```
Acquire() 호출
  │
  ├─ idle 슬라이스에 유효한 커넥션 있음 → 꺼내서 반환
  │    └─ 꺼낼 때 idle_timeout, max_lifetime 초과 여부 확인
  │       초과 시 닫고 다음 커넥션 시도
  │
  ├─ numOpen < maxOpen → 새 커넥션 생성 후 반환
  │
  └─ numOpen >= maxOpen → waitQueue 채널에서 대기
       └─ connection_timeout 초과 시 에러 반환
```

#### 헬스체크 고루틴

```go
// 30초마다 실행
func (p *Pool) healthCheck() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        p.mu.Lock()
        alive := p.idle[:0]
        for _, c := range p.idle {
            if err := c.Ping(); err != nil || c.expired() {
                c.Close()
                p.numOpen--
            } else {
                alive = append(alive, c)
            }
        }
        p.idle = alive
        // min_connections 미만이면 새 커넥션 보충
        for p.numOpen < p.minOpen {
            conn, err := p.newConn()
            if err != nil { break }
            p.idle = append(p.idle, conn)
        }
        p.mu.Unlock()
    }
}
```

---

### 2. R/W 쿼리 라우팅 구현

#### 쿼리 파서

정규식 기반으로 쿼리의 첫 키워드를 추출하여 분류한다. 전문 SQL 파서 대신 경량 방식을 택한다.

```go
var writeKeywords = map[string]bool{
    "INSERT": true, "UPDATE": true, "DELETE": true,
    "CREATE": true, "ALTER": true, "DROP": true,
    "TRUNCATE": true, "GRANT": true, "REVOKE": true,
}

func Classify(query string) QueryType {
    // 1) 힌트 주석 확인: /* route:writer */ 또는 /* route:reader */
    if hint := extractHint(query); hint != "" {
        return hint
    }

    // 2) 첫 키워드 추출
    keyword := strings.ToUpper(firstWord(stripComments(query)))

    if writeKeywords[keyword] {
        return Write
    }
    return Read // SELECT, SHOW, EXPLAIN 등
}
```

#### 트랜잭션 추적

클라이언트별 세션 상태를 관리하여 트랜잭션 중에는 모든 쿼리를 Writer로 보낸다.

```go
type Session struct {
    inTransaction  bool           // BEGIN 이후 true, COMMIT/ROLLBACK 이후 false
    lastWriteTime  time.Time      // 마지막 쓰기 시각 (read_after_write_delay용)
    writerConn     *Conn          // 트랜잭션 동안 고정된 Writer 커넥션
}

func (s *Session) Route(query string) *Pool {
    if s.inTransaction {
        return writerPool
    }

    qtype := parser.Classify(query)

    // 쓰기 직후 읽기 → Writer로 전송
    if qtype == Read && time.Since(s.lastWriteTime) < readAfterWriteDelay {
        return writerPool
    }

    if qtype == Write {
        s.lastWriteTime = time.Now()
        return writerPool
    }

    return readerPool
}
```

#### 라운드로빈 로드밸런서

```go
type RoundRobin struct {
    mu       sync.Mutex
    readers  []*Pool            // Reader별 커넥션 풀
    healthy  []bool             // 각 Reader의 정상 여부
    index    uint64             // atomic 카운터
}

func (r *RoundRobin) Next() *Pool {
    r.mu.Lock()
    defer r.mu.Unlock()

    n := len(r.readers)
    for i := 0; i < n; i++ {
        idx := int(atomic.AddUint64(&r.index, 1)) % n
        if r.healthy[idx] {
            return r.readers[idx]
        }
    }
    return nil // 모든 Reader 장애 → 에러 또는 Writer 폴백
}
```

---

### 3. 쿼리 캐시 구현

#### LRU 캐시 구조

Go 표준 라이브러리의 `container/list`로 LRU를 구현한다.

```go
type Cache struct {
    mu         sync.RWMutex
    items      map[uint64]*list.Element  // 해시 → 리스트 노드
    evictList  *list.List                // LRU 순서 관리
    maxEntries int
    ttl        time.Duration
    maxSize    int                       // 결과 바이트 제한
}

type entry struct {
    key       uint64
    result    []byte
    tables    []string                   // 이 쿼리가 참조하는 테이블들
    expiresAt time.Time
}
```

#### 캐시 키 생성

```go
func CacheKey(query string, params []any) uint64 {
    h := xxhash.New()         // 빠른 비암호화 해시
    h.WriteString(query)
    for _, p := range params {
        fmt.Fprint(h, p)
    }
    return h.Sum64()
}
```

#### 쓰기 시 캐시 무효화

쓰기 쿼리에서 대상 테이블을 추출하고, 해당 테이블을 참조하는 캐시를 모두 삭제한다.

```go
type Invalidator struct {
    tableIndex map[string][]*list.Element  // 테이블명 → 캐시 항목들
}

func (inv *Invalidator) OnWrite(query string) {
    tables := extractTables(query)   // "INSERT INTO users ..." → ["users"]
    for _, t := range tables {
        for _, elem := range inv.tableIndex[t] {
            cache.Remove(elem)
        }
        delete(inv.tableIndex, t)
    }
}
```

---

### 전체 요청 처리 흐름

```
클라이언트 TCP 접속
  │
  ▼
[proxy/server.go] PG 핸드셰이크 처리, Session 생성
  │
  ▼
쿼리 수신
  │
  ├─ 쓰기 쿼리 ──────────────────────────────┐
  │                                           ▼
  │                                    Writer Pool에서 커넥션 획득
  │                                           │
  │                                    DB 실행 → 결과 반환
  │                                           │
  │                                    캐시 무효화 (Invalidator)
  │                                           │
  │                                    커넥션 반환
  │
  └─ 읽기 쿼리
       │
       ├─ 캐시 히트 → 캐시 결과 즉시 반환
       │
       └─ 캐시 미스
            │
            ▼
       Reader Pool에서 커넥션 획득 (라운드로빈)
            │
       DB 실행 → 결과 반환
            │
       결과 캐싱 (크기 제한 이내일 때)
            │
       커넥션 반환
```

---

### 구현 순서

단계별로 점진적으로 구현하며, 각 단계가 독립적으로 동작 가능하도록 한다.

| 단계 | 내용 | 완료 기준 |
|------|------|-----------|
| **1단계** | 설정 로드 + TCP 리스너 + PG 프로토콜 프록시 | 클라이언트 → 프록시 → 단일 DB 연결 통과 |
| **2단계** | 커넥션 풀링 | 커넥션 재사용 확인, 풀 가득 찬 경우 대기/타임아웃 동작 |
| **3단계** | 쿼리 파서 + R/W 라우팅 | SELECT는 Reader, INSERT는 Writer로 가는지 확인 |
| **4단계** | 라운드로빈 + 장애 감지 | Reader 여러 대에 분산, 하나 죽여도 나머지로 동작 |
| **5단계** | 쿼리 캐싱 + 무효화 | 동일 SELECT 두 번째는 캐시 히트, 쓰기 후 캐시 미스 |
| **6단계** | Prometheus 메트릭 | `/metrics` 엔드포인트에서 풀/캐시/라우팅 메트릭 수집 |
| **7단계** | Prepared Statement 라우팅 | Extended Query의 SELECT도 reader로 라우팅 |
| **8단계** | Admin API | `/admin/stats`, `/admin/health`, `/admin/cache/flush` 동작 |
| **9단계** | Transaction Pooling | 트랜잭션 단위 커넥션 다중화, DISCARD ALL |
| **10단계** | TLS + Front-end Auth | SSLRequest 핸들링, 프록시 자체 MD5 인증 |
| **11단계** | Circuit Breaker + Rate Limiting | 에러율 기반 자동 트립, Token Bucket |
| **12단계** | Zero-Downtime Reload | SIGHUP + HTTP API, Reader Pool 핫스왑 |
| **13단계** | LSN Causal Consistency | WAL LSN 추적, Reader 폴링, Causal Read |
| **14단계** | AST Parser + Firewall | pg_query_go, AST 분류, 쿼리 방화벽, 시맨틱 캐시 키 |
| **15단계** | Audit Logging | 비동기 감사 로그, Slow Query 감지, Webhook 알림 |
| **16단계** | Helm Chart | Multi-stage Dockerfile, K8s Helm Chart |
| **17단계** | Serverless Data API | HTTP REST → PG Wire Protocol → JSON 응답 |
| **18단계** | OpenTelemetry 분산 추적 | TracerProvider, Span 계측, Data API traceparent 전파 |
| **19단계** | Config File Watch | fsnotify 파일 변경 감지, 자동 리로드 |

---

### 4. Prometheus 메트릭 구현

#### 메트릭 레지스트리

`prometheus/client_golang`을 사용한다. 각 컴포넌트가 자체 메트릭을 등록하고, HTTP 핸들러가 `/metrics`로 노출한다.

```go
// internal/metrics/metrics.go
type Metrics struct {
    // Pool
    PoolOpenConns    *prometheus.GaugeVec     // {role="writer|reader", addr="..."}
    PoolIdleConns    *prometheus.GaugeVec
    PoolAcquireDur   *prometheus.HistogramVec

    // Router
    QueriesRouted    *prometheus.CounterVec   // {target="writer|reader"}
    QueryDuration    *prometheus.HistogramVec // {target="writer|reader"}
    ReaderFallback   prometheus.Counter

    // Cache
    CacheHits        prometheus.Counter
    CacheMisses      prometheus.Counter
    CacheEntries     prometheus.Gauge
    CacheInvalidations prometheus.Counter
}

func New() *Metrics {
    m := &Metrics{
        QueriesRouted: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "pgmux_queries_routed_total",
                Help: "Total queries routed",
            },
            []string{"target"},
        ),
        QueryDuration: prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "pgmux_query_duration_seconds",
                Help:    "Query processing duration",
                Buckets: prometheus.DefBuckets,
            },
            []string{"target"},
        ),
        // ...
    }
    prometheus.MustRegister(m.QueriesRouted, m.QueryDuration, ...)
    return m
}
```

#### 서버에서 메트릭 기록

```go
// proxy/server.go — relayQueries 내부
start := time.Now()
if route == RouteWriter {
    s.handleWriteQuery(...)
    s.metrics.QueriesRouted.WithLabelValues("writer").Inc()
} else {
    s.handleReadQuery(...)
    s.metrics.QueriesRouted.WithLabelValues("reader").Inc()
}
s.metrics.QueryDuration.WithLabelValues(routeName(route)).Observe(time.Since(start).Seconds())
```

#### HTTP 엔드포인트

```go
// cmd/pgmux/main.go
if cfg.Metrics.Enabled {
    go func() {
        mux := http.NewServeMux()
        mux.Handle("/metrics", promhttp.Handler())
        http.ListenAndServe(cfg.Metrics.Listen, mux)
    }()
}
```

---

### 5. Prepared Statement 라우팅 구현

#### Parse 메시지 파싱

PG Extended Query Protocol의 `Parse` 메시지 포맷:

```
'P' + int32(length) + string(statement_name) + string(query) + int16(num_params) + int32[](param_oids)
```

쿼리 텍스트를 추출하여 라우팅에 활용한다.

```go
// internal/protocol/message.go
func ExtractParseQuery(payload []byte) (stmtName, query string) {
    // statement name: null-terminated string
    nameEnd := indexOf(payload, 0)
    stmtName = string(payload[:nameEnd])
    rest := payload[nameEnd+1:]
    // query: null-terminated string
    queryEnd := indexOf(rest, 0)
    query = string(rest[:queryEnd])
    return stmtName, query
}
```

#### 세션별 Statement 라우팅 맵

```go
// proxy/server.go — clientSession에 추가
type extendedQueryState struct {
    stmtRoutes map[string]router.Route  // statement name → route
    pendingRoute router.Route           // 현재 Parse~Sync 사이의 라우팅 결과
}

// Parse 메시지 수신 시:
stmtName, query := protocol.ExtractParseQuery(msg.Payload)
route := session.Route(query)
eqState.stmtRoutes[stmtName] = route
eqState.pendingRoute = route

// Bind/Execute/Describe는 pendingRoute에 따라 writer 또는 reader로 전달
// Sync에서 relayUntilReady
```

#### 라우팅 흐름

```
Parse(P) → SQL 추출 → Classify → Route 결정 → 대상 백엔드로 전달
Bind(B)  → pendingRoute의 백엔드로 전달
Execute(E) → pendingRoute의 백엔드로 전달
Sync(S) → 대상 백엔드로 전달 → relayUntilReady
```

---

### 6. Admin API 구현

#### HTTP 핸들러

```go
// internal/admin/admin.go
type Handler struct {
    pools    map[string]*pool.Pool
    cache    *cache.Cache
    balancer *router.RoundRobin
    cfg      *config.Config
}

func (h *Handler) Register(mux *http.ServeMux) {
    mux.HandleFunc("GET /admin/stats", h.handleStats)
    mux.HandleFunc("GET /admin/health", h.handleHealth)
    mux.HandleFunc("GET /admin/config", h.handleConfig)
    mux.HandleFunc("POST /admin/cache/flush", h.handleCacheFlush)
    mux.HandleFunc("POST /admin/cache/flush/{table}", h.handleCacheFlushTable)
}
```

#### Stats 응답 예시

```json
{
  "pool": {
    "writer": {"open": 5, "idle": 3},
    "readers": {
      "replica-1:5432": {"open": 8, "idle": 4},
      "replica-2:5432": {"open": 7, "idle": 5}
    }
  },
  "cache": {
    "entries": 1523,
    "hit_rate": 0.847
  },
  "routing": {
    "writer_queries": 12450,
    "reader_queries": 89230,
    "fallback_count": 3
  }
}
```

---

### 7. LSN 기반 Causal Consistency 구현

#### LSN 타입

PostgreSQL LSN(Log Sequence Number) "X/XXXXXXXX" 형식을 uint64로 파싱하여 O(1) 비교를 가능하게 한다.

```go
type LSN uint64

func ParseLSN(s string) (LSN, error) {
    parts := strings.SplitN(s, "/", 2)
    hi, _ := strconv.ParseUint(parts[0], 16, 32)
    lo, _ := strconv.ParseUint(parts[1], 16, 32)
    return LSN(hi<<32 | lo), nil
}
```

#### Causal Read 흐름

```
쓰기 쿼리 → Writer 실행 → pg_current_wal_lsn() 조회 → Session에 LSN 저장
                                                            │
읽기 쿼리 → Session.lastWriteLSN 확인 ───────────────────────┤
                                                            │
Reader 선택: NextWithLSN(minLSN) → replay_lsn >= minLSN인 Reader만 ──→ DB 실행
                                    │
                    (적합한 Reader 없으면 Writer fallback)
```

#### LSN 폴링

`startLSNPolling()` 고루틴이 1초마다 각 Reader에 `SELECT pg_last_wal_replay_lsn()`를 조회하고, Balancer의 `SetReplayLSN()`으로 갱신한다. Prometheus `pgmux_reader_lsn_lag_bytes` 메트릭도 함께 업데이트한다.

---

### 8. AST 기반 쿼리 파서 구현

#### pg_query_go 통합

`pg_query_go/v5`는 PostgreSQL의 실제 C 파서를 cgo로 바인딩한 라이브러리다. 문자열 파서로 처리 불가능한 복잡한 쿼리를 정확하게 분석한다.

```go
func ClassifyAST(query string) QueryType {
    // 1. 힌트 체크
    // 2. pg_query.Parse() → AST
    // 3. isWriteNode() — 20+ DDL/DML 노드 타입 검사
    // 4. CTE 내부 write 감지
    // 5. 파싱 실패 시 문자열 파서 fallback
}
```

#### AST 노드 순회

`WalkNodes()` — 깊이 우선 순회. SelectStmt, InsertStmt, UpdateStmt, DeleteStmt, JoinExpr, SubLink, CommonTableExpr, BoolExpr 등 주요 노드 타입을 재귀적으로 탐색한다.

---

### 9. 쿼리 방화벽 구현

AST 분석으로 위험 쿼리를 프록시 단에서 차단한다. `WhereClause == nil`로 조건 유무를 정확히 판단하며, 파싱 불가 시 fail-open 전략을 따른다.

```go
func CheckFirewall(query string, cfg FirewallConfig) FirewallResult {
    // AST 파싱 → 각 statement 검사
    // DELETE: WhereClause == nil → 차단
    // UPDATE: WhereClause == nil → 차단
    // DROP: 무조건 차단
    // TRUNCATE: 무조건 차단
}
```

---

### 10. 시맨틱 캐시 키 구현

pg_query의 Parse+Deparse를 활용하여 구조적으로 동일한 쿼리에 같은 캐시 키를 생성한다.
Deparse는 공백/대소문자를 정규화하면서 리터럴 값은 보존하므로, 다른 파라미터의 쿼리는 서로 다른 캐시 키를 갖는다.

```go
func SemanticCacheKey(query string) uint64 {
    tree, _ := pg_query.Parse(query)
    deparsed, _ := pg_query.Deparse(tree)
    h := fnv.New64a()
    h.Write([]byte(deparsed))
    return h.Sum64() // 공백, 대소문자 무관 / 리터럴 값 보존
}
```

---

### 11. Audit Logging 구현

#### 비동기 이벤트 채널

쿼리 처리 경로를 블로킹하지 않기 위해 버퍼 채널 기반의 비동기 처리 패턴을 사용한다.

```go
type Logger struct {
    cfg     Config
    eventCh chan Event        // 버퍼 채널 (1024)
    // ...
}

func (l *Logger) Log(e Event) {
    select {
    case l.eventCh <- e:  // 논블로킹
    default:              // 채널 가득 차면 드롭
    }
}
```

전용 goroutine이 채널에서 이벤트를 소비하며, Slow Query 감지(`duration > threshold`), 구조화 로그(`slog.Warn/Info`), Webhook 알림을 처리한다.

#### Webhook Rate Limiting

동일 쿼리 패턴에 대한 중복 알림을 방지하기 위해 쿼리 앞 50자를 키로 사용하여 최소 1분 간격을 보장한다.

```go
func (l *Logger) shouldSendWebhook(query string) bool {
    key := truncateQuery(query, 50)
    if last, ok := l.lastWebhook[key]; ok {
        if time.Since(last) < l.webhookInterval {
            return false
        }
    }
    l.lastWebhook[key] = time.Now()
    return true
}
```

---

### 12. Serverless Data API 구현

#### HTTP → PG Wire Protocol 변환

`POST /v1/query`로 받은 SQL을 커넥션 풀에서 획득한 PG 커넥션으로 Simple Query Protocol을 사용하여 실행하고, 응답 메시지를 JSON으로 변환한다.

```
HTTP Request → Pool.Acquire → WriteMessage(Query) → ReadMessage loop → JSON Response
```

#### RowDescription OID 타입 매핑

PG RowDescription 메시지의 type OID를 JSON 타입으로 변환한다.

```go
func convertValue(val string, oid uint32) any {
    switch oid {
    case 16:          return val == "t"        // bool
    case 20, 21, 23:  return parseInt64(val)   // int8/int2/int4
    case 700, 701:    return parseFloat64(val) // float4/float8
    default:          return val               // text, timestamp 등
    }
}
```

#### 기존 기능 통합

Data API는 기존 모든 컴포넌트를 재사용한다:
- `router.Classify()` / `router.ClassifyAST()` — R/W 분류
- `cache.Cache` — 읽기 결과 캐싱 + 쓰기 시 무효화
- `router.CheckFirewall()` — 위험 쿼리 차단
- `resilience.RateLimiter` — 요청 제한

---

### 13. OpenTelemetry 분산 추적 구현

#### TracerProvider 초기화

`go.opentelemetry.io/otel`을 사용한다. `telemetry.Init(cfg)` 호출 시 exporter, sampler, resource를 설정하고, `enabled: false`이면 noop tracer로 동작한다.

```go
// internal/telemetry/telemetry.go
func Init(cfg config.TelemetryConfig) (shutdown func(context.Context) error, err error) {
    if !cfg.Enabled {
        return func(context.Context) error { return nil }, nil
    }
    // Resource(service.name, service.version) + Exporter(otlp/stdout) + Sampler(ratio)
    tp := sdktrace.NewTracerProvider(...)
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{}, propagation.Baggage{},
    ))
    return tp.Shutdown, nil
}
```

#### Span 계측 위치

Simple Query(`MsgQuery`) 처리 경로와 Extended Query(`MsgSync`) 처리 경로에 각각 Span을 삽입한다.
Data API는 HTTP `traceparent` 헤더를 `otel.GetTextMapPropagator().Extract()`로 파싱하여 부모 Span으로 연결한다.

---

### 14. Config File Watch 구현

#### fsnotify 기반 파일 감시

`github.com/fsnotify/fsnotify`로 설정 파일의 부모 디렉토리를 감시한다. K8s ConfigMap은 symlink swap 방식이므로 `CREATE` 이벤트도 포함하여 감지한다.

```go
// internal/config/watcher.go
type FileWatcher struct {
    path     string
    fileName string
    onChange func()
    watcher  *fsnotify.Watcher
}

func (fw *FileWatcher) Start(ctx context.Context) error {
    dir := filepath.Dir(fw.path)
    fw.watcher.Add(dir)
    // 이벤트 수신 → 디바운싱(1초) → onChange 콜백
}
```

`cmd/pgmux/main.go`에서 `config.watch: true`일 때 FileWatcher를 시작하며, 콜백은 기존 `reloadConfig()` 함수를 재사용한다.
