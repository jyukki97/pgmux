## 구현 설계

### 기술 스택

| 항목 | 선택 | 이유 |
|------|------|------|
| 언어 | Go | 고루틴 기반 동시성, net 라이브러리 성숙도, 단일 바이너리 배포 |
| DB 프로토콜 | PostgreSQL wire protocol | 클라이언트가 일반 PG 드라이버로 접속 가능 |
| 설정 | YAML (`gopkg.in/yaml.v3`) | 스펙 설정 예시와 일치 |
| 캐시 | 인메모리 (`sync.Map` + container/list`) | 외부 의존 없이 LRU 구현 |
| 로깅 | `slog` (표준 라이브러리) | 구조화 로깅, 외부 의존 없음 |

---

### 프로젝트 구조

```
db-proxy/
├── cmd/
│   └── db-proxy/
│       └── main.go              # 진입점: 설정 로드 → 서버 시작
├── internal/
│   ├── config/
│   │   └── config.go            # YAML 파싱, 설정 구조체 정의
│   ├── proxy/
│   │   └── server.go            # TCP 리스너, 클라이언트 접속 수락
│   ├── pool/
│   │   ├── pool.go              # 커넥션 풀 핵심 로직
│   │   └── health.go            # 헬스체크 고루틴
│   ├── router/
│   │   ├── router.go            # Writer/Reader 라우팅 결정
│   │   ├── parser.go            # 쿼리 파싱 (SELECT/INSERT 등 분류)
│   │   └── balancer.go          # Reader 라운드로빈 로드밸런싱
│   └── cache/
│       ├── cache.go             # LRU 캐시 구현
│       └── invalidator.go       # 쓰기 시 테이블별 캐시 무효화
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
                Name: "dbproxy_queries_routed_total",
                Help: "Total queries routed",
            },
            []string{"target"},
        ),
        QueryDuration: prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "dbproxy_query_duration_seconds",
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
// cmd/db-proxy/main.go
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
