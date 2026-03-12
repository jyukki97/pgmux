## pgmux

애플리케이션과 데이터베이스 사이에 위치하여 커넥션 관리, 쿼리 라우팅, 캐싱을 수행하는 프록시 레이어.

---

### 1. 커넥션 풀링

애플리케이션이 DB에 직접 연결하지 않고 프록시가 커넥션을 재사용하여 리소스를 절약한다.

- **풀 관리**
  - 최소/최대 커넥션 수 설정 (`min_connections`, `max_connections`)
  - 유휴 커넥션 타임아웃 — 일정 시간 미사용 시 자동 반환 (`idle_timeout`)
  - 커넥션 최대 수명 — 오래된 커넥션 주기적 갱신 (`max_lifetime`)
- **커넥션 획득**
  - 풀에 여유가 있으면 즉시 할당
  - 풀이 가득 찬 경우 대기 큐에 넣고, 설정된 타임아웃 초과 시 에러 반환 (`connection_timeout`)
- **헬스체크**
  - 주기적으로 커넥션에 ping을 보내 유효성 검증
  - 비정상 커넥션은 풀에서 제거 후 새 커넥션으로 교체

---

### 2. 읽기/쓰기(R/W) 쿼리 자동 분산

쿼리를 파싱하여 Writer(Primary)와 Reader(Replica)로 자동 라우팅한다.

- **쿼리 분류 규칙**
  - `SELECT` → Reader
  - `INSERT`, `UPDATE`, `DELETE`, `CREATE`, `ALTER`, `DROP` → Writer
  - 트랜잭션 내부(`BEGIN` ~ `COMMIT`/`ROLLBACK`) → 모든 쿼리를 Writer로 전송
  - 힌트 주석으로 강제 라우팅 지원 (예: `/* route:writer */ SELECT ...`)
- **Reader 로드밸런싱**
  - 다수의 Replica가 있을 때 라운드로빈 방식으로 분산
  - 특정 Replica 장애 시 자동으로 목록에서 제외하고 나머지로 분산
- **Replication Lag 대응**
  - 타이머 기반: 쓰기 직후 읽기 시 일정 시간(`read_after_write_delay`) 동안 Writer에서 읽기 수행
  - LSN 기반 Causal Consistency: 쓰기 후 Writer의 `pg_current_wal_lsn()`을 세션에 기록하고, Reader의 `pg_last_wal_replay_lsn()`이 해당 LSN에 도달한 경우에만 라우팅
  - 두 방식은 `routing.causal_consistency` 설정으로 전환 (양자택일)

---

### 3. 반복 쿼리 캐싱

동일한 쿼리가 짧은 시간 내에 반복될 때 DB를 거치지 않고 캐시된 결과를 반환한다.

- **캐시 대상**
  - `SELECT` 쿼리만 캐싱 (쓰기 쿼리는 캐싱하지 않음)
  - 쿼리 텍스트 + 파라미터를 해시하여 캐시 키 생성
- **만료 정책**
  - TTL 기반 — 항목별 유효 시간 설정 (`cache_ttl`)
  - 관련 테이블에 쓰기 발생 시 해당 테이블의 캐시 무효화 (write-through invalidation)
- **제한**
  - 최대 캐시 항목 수 제한 (`max_cache_entries`)
  - LRU 방식으로 오래된 항목부터 제거
  - 결과 크기가 임계값을 초과하는 쿼리는 캐싱 제외 (`max_result_size`)

---

---

### 4. Prometheus 메트릭

프록시의 런타임 상태를 Prometheus 형식으로 노출하여 Grafana 등에서 모니터링한다.

- **커넥션 풀 메트릭**
  - 현재 열린 커넥션 수 (writer/reader별) — `pgmux_pool_connections_open`
  - 유휴 커넥션 수 — `pgmux_pool_connections_idle`
  - 커넥션 획득 대기 수 — `pgmux_pool_waiting_total`
  - 커넥션 획득 레이턴시 히스토그램 — `pgmux_pool_acquire_duration_seconds`
- **쿼리 라우팅 메트릭**
  - 쿼리 라우팅 카운터 (writer/reader별) — `pgmux_queries_routed_total{target="writer|reader"}`
  - 쿼리 처리 레이턴시 히스토그램 — `pgmux_query_duration_seconds`
  - Writer fallback 횟수 — `pgmux_reader_fallback_total`
- **캐시 메트릭**
  - 캐시 히트/미스 카운터 — `pgmux_cache_hits_total`, `pgmux_cache_misses_total`
  - 캐시 항목 수 — `pgmux_cache_entries`
  - 캐시 무효화 카운터 — `pgmux_cache_invalidations_total`
- **엔드포인트**
  - `GET /metrics` — Prometheus scrape 엔드포인트
  - 설정의 `metrics.listen` 포트에서 HTTP로 제공

---

### 5. Prepared Statement 라우팅

Extended Query Protocol의 `Parse` 메시지에서 SQL을 추출하여 reader 라우팅을 지원한다. 현재는 Extended Query가 무조건 writer로 가지만, SELECT의 Prepared Statement도 reader로 보낼 수 있어야 한다.

- **Parse 메시지 SQL 추출**
  - `Parse` 메시지 포맷: statement name (string) + query (string) + param count + param OIDs
  - 쿼리 텍스트를 추출하여 `Classify()` 적용
- **세션별 Statement 맵**
  - `map[string]Route` — statement name → 라우팅 결과 캐싱
  - `Bind`/`Execute` 시 해당 statement의 라우팅 결과를 참조
  - unnamed statement (`""`)은 매번 분류
- **라우팅 규칙**
  - SELECT Prepared Statement → reader (RoundRobin)
  - 쓰기 Prepared Statement → writer
  - 트랜잭션 내부 → 전체 writer (기존 Session 로직 유지)
- **제한**
  - reader로 라우팅된 Extended Query의 응답은 캐싱 지원
  - `DEALLOCATE` 시 statement 맵에서 제거

---

### 6. Admin API

프록시의 런타임 상태를 조회하고 동적으로 제어하는 HTTP 관리 인터페이스.

- **상태 조회**
  - `GET /admin/stats` — 풀, 캐시, 라우팅 통계 JSON
  - `GET /admin/health` — 백엔드 헬스 상태 (writer + reader별)
  - `GET /admin/config` — 현재 적용된 설정 (비밀번호 마스킹)
  - `GET /admin/mirror/stats` — 쿼리 미러링 통계 (패턴별 P50/P99, 회귀 감지)
- **동적 제어**
  - `POST /admin/cache/flush` — 전체 캐시 즉시 비우기
  - `POST /admin/cache/flush/{table}` — 특정 테이블 캐시만 무효화
  - `POST /admin/reload` — 설정 핫 리로드
- **보안**
  - 별도 포트에서 제공 (외부 노출 방지)
  - 향후 Bearer token 인증 추가 가능

---

### 7. AST 기반 쿼리 파싱

pg_query_go(PostgreSQL 실제 C 파서 바인딩)를 활용하여 정확한 쿼리 분석을 수행한다.

- **AST 기반 쿼리 분류** (`routing.ast_parser: true`)
  - CTE 내부 write 감지: `WITH x AS (UPDATE ...) SELECT ...` → Write로 분류
  - DDL 20+ 노드 타입 (CreateStmt, AlterTableStmt, IndexStmt, ViewStmt 등) 정확 분류
  - 파싱 실패 시 문자열 파서로 자동 fallback
- **AST 기반 테이블 추출**
  - CTE 내부의 write 대상 테이블도 정확히 추출
  - 캐시 무효화의 정확도 향상
- **시맨틱 캐시 키**
  - pg_query Parse+Deparse로 공백/대소문자 정규화, 리터럴 값은 보존하여 캐시 충돌 방지
  - `NormalizeQuery()` — `$N` 플레이스홀더 치환 (로깅/디버깅용)

---

### 8. 쿼리 방화벽 (Query Firewall)

AST 분석으로 위험한 쿼리를 프록시 단에서 사전 차단한다.

- **차단 규칙**
  - `block_delete_without_where` — WHERE 없는 DELETE 차단
  - `block_update_without_where` — WHERE 없는 UPDATE 차단
  - `block_drop_table` — DROP 문 차단
  - `block_truncate` — TRUNCATE 문 차단
- **동작 방식**
  - AST의 WhereClause == nil 로 정확 판단 (문자열 검색과 다름)
  - 차단 시 PG 에러 메시지 반환 + Prometheus 메트릭 기록
  - 파싱 불가 시 fail-open (허용)

---

### 설정 예시

```yaml
proxy:
  listen: "0.0.0.0:5432"
  shutdown_timeout: 30s              # Graceful shutdown 타임아웃 (기본: 30s)

writer:
  host: "primary.db.internal"
  port: 5432

readers:                              # 선택사항 — 생략 시 모든 쿼리가 writer로 라우팅
  - host: "replica-1.db.internal"
    port: 5432
  - host: "replica-2.db.internal"
    port: 5432

pool:
  min_connections: 5
  max_connections: 50
  idle_timeout: "10m"
  max_lifetime: "1h"
  connection_timeout: "5s"
  reset_query: "DISCARD ALL"
  prepared_statement_mode: "proxy"    # "proxy" (기본, 패스스루) | "multiplex" (Simple Query로 합성)

routing:
  read_after_write_delay: "500ms"
  causal_consistency: false
  ast_parser: false

firewall:
  enabled: true
  block_delete_without_where: true
  block_update_without_where: true
  block_drop_table: false
  block_truncate: false

cache:
  enabled: true
  cache_ttl: "10s"
  max_cache_entries: 10000
  max_result_size: "1MB"
  invalidation:
    mode: "pubsub"                    # "local" (기본값) 또는 "pubsub" (Redis)
    redis_addr: "localhost:6379"
    channel: "pgmux:invalidate"

audit:
  enabled: true
  slow_query_threshold: "500ms"
  log_all_queries: false
  webhook:
    enabled: false
    url: "https://hooks.slack.com/services/..."
    timeout: "5s"

mirror:
  enabled: false
  host: "shadow-db.internal"
  port: 5432
  # user, password, database: 미설정 시 backend 값 사용
  mode: "read_only"                    # "read_only" (기본) | "all"
  tables: []                            # 빈 배열 = 모든 테이블
  compare: true                         # 패턴별 P50/P99 레이턴시 비교
  workers: 4                            # 미러링 워커 수
  buffer_size: 10000                    # 비동기 큐 크기 (초과 시 드롭)

backend:
  user: "postgres"
  password: "postgres"
  database: "testdb"

metrics:
  enabled: true
  listen: "0.0.0.0:9090"

admin:
  enabled: true
  listen: "0.0.0.0:9091"

data_api:
  enabled: true
  listen: "0.0.0.0:8080"
  api_keys:
    - "your-api-key-here"

telemetry:
  enabled: false
  exporter: "otlp"                    # "otlp" (gRPC) 또는 "stdout"
  endpoint: "localhost:4317"
  service_name: "pgmux"
  sample_ratio: 1.0

config:
  watch: true                         # 설정 파일 변경 감지 자동 리로드
```
### 9. Audit Logging & Slow Query Tracker

쿼리 감사 로그를 기록하고, 느린 쿼리를 감지하여 알림을 전송한다.

- **Slow Query 감지**
  - 쿼리 실행 시간이 `slow_query_threshold`를 초과하면 경고 로그 기록
  - 구조화 로그 필드: event, user, source_ip, query, duration_ms, target, cached
- **감사 로그**
  - `log_all_queries: true`이면 모든 쿼리를 감사 로그로 기록
  - 비동기 채널 기반 — 쿼리 처리 경로 비블로킹
- **Webhook 알림**
  - Slow Query 발생 시 Webhook URL로 HTTP POST (Slack Incoming Webhook 호환)
  - 동일 쿼리 패턴 중복 알림 방지 (최소 interval)
  - 전용 goroutine + 버퍼 채널로 비동기 전송
- **메트릭**
  - `pgmux_slow_queries_total{target}` — Slow Query 카운터
  - `pgmux_audit_webhook_sent_total` — Webhook 전송 횟수
  - `pgmux_audit_webhook_errors_total` — Webhook 실패 횟수

---

### 10. Serverless Data API

HTTP REST API를 통해 SQL 쿼리를 실행할 수 있는 엔드포인트를 제공한다. Lambda/Edge 함수에서 TCP 커넥션 비용 없이 DB에 접근할 수 있다.

- **엔드포인트**
  - `POST /v1/query` — SQL 전송 → JSON 응답
  - 별도 포트에서 HTTP 서버 운영 (`data_api.listen`)
- **PG Wire Protocol → JSON 변환**
  - RowDescription OID 기반 타입 매핑 (int4→number, text→string, bool→boolean 등)
  - DataRow 메시지를 파싱하여 JSON 배열로 변환
  - NULL 값, 타입 변환 정확 처리
- **기존 기능 투명 적용**
  - R/W 라우팅: SELECT는 Reader, INSERT/UPDATE/DELETE는 Writer
  - 쿼리 캐싱: Reader 쿼리 결과 캐시 + 쓰기 시 무효화
  - 방화벽: AST 기반 위험 쿼리 차단 (DELETE without WHERE 등)
  - Rate Limiting: Token Bucket 제한
- **인증**
  - `Authorization: Bearer <api_key>` 헤더 검증
  - API Key 목록은 YAML 설정으로 관리
  - 키 미설정 시 인증 비활성화

---

### 11. Helm Chart

Kubernetes에 pgmux를 배포하기 위한 Helm Chart를 제공한다.

- **Dockerfile**: Multi-stage 빌드 (golang:1.25-bookworm → debian:bookworm-slim)
- **Helm Chart 구조**: `deploy/helm/pgmux/`
  - Deployment (readiness/liveness probe, ConfigMap 마운트)
  - Service (PG 5432, Metrics 9090, Admin 9091)
  - ConfigMap (config.yaml 주입)
  - HPA (CPU 기반 오토스케일링)
  - PDB (최소 가용 Pod 보장)
  - ServiceMonitor (Prometheus Operator 연동)

---

### 12. OpenTelemetry 분산 추적

프록시 내부 처리 단계별 지연 시간을 Span으로 기록하여 분산 추적을 지원한다.

- **TracerProvider**
  - OTLP gRPC 또는 stdout exporter 지원
  - Resource: `service.name`, `service.version`
  - Sampler: ratio-based (0.0 ~ 1.0)
  - W3C TraceContext + Baggage propagator
- **Simple Query Span 구조**
  - `pgmux.query` (root) → `pgmux.parse` → `pgmux.cache.lookup` → `pgmux.pool.acquire` → `pgmux.backend.exec` → `pgmux.cache.store`
  - Attributes: `db.system`, `db.statement`, `db.operation`, `pgmux.route`, `pgmux.cached`
- **Extended Query Span 구조**
  - `pgmux.extended_query` (root) → `pgmux.pool.acquire` → `pgmux.backend.exec`
- **Data API Trace 전파**
  - HTTP `traceparent` 헤더를 파싱하여 부모 span context로 사용
- **설정**
  - `telemetry.enabled: false`일 때 noop tracer (성능 영향 없음)

```yaml
telemetry:
  enabled: false
  exporter: "otlp"          # otlp | stdout
  endpoint: "localhost:4317"
  service_name: "pgmux"
  sample_ratio: 1.0
```

---

### 13. 설정 파일 자동 리로드

`fsnotify`로 설정 파일 변경을 감지하여 무중단 리로드를 자동으로 트리거한다.

- **FileWatcher**
  - 부모 디렉토리 감시로 K8s ConfigMap symlink swap 지원
  - 1초 디바운싱으로 연속 이벤트 병합
  - 기존 SIGHUP 리로드 경로 재사용 (Phase 11)
- **설정**
  - `config.watch: true`일 때 활성화 (기본값: `false`)

```yaml
config:
  watch: true
```

---

### 14. Query Mirroring

프로덕션 쿼리를 Shadow DB에 비동기로 미러링하여 레이턴시를 비교한다.

- **비동기 전송 (Fire-and-Forget)**
  - 워커 풀 + 버퍼 채널 기반 비동기 처리
  - 프로덕션 쿼리 경로에 블로킹 없음 (버퍼 초과 시 드롭)
  - 전용 커넥션 풀로 Shadow DB 커넥션 관리
- **모드**
  - `read_only` (기본): SELECT만 미러링
  - `all`: INSERT/UPDATE/DELETE도 미러링
- **테이블 필터**
  - `tables: ["users", "orders"]` — 지정 테이블 관련 쿼리만 미러링
  - 빈 배열이면 모든 테이블 통과
- **레이턴시 비교 (`compare: true`)**
  - `pg_query.Normalize()`로 쿼리 정규화 → 패턴별 그룹핑
  - 순환 버퍼(1,000 샘플)로 메모리 효율적 통계 수집
  - 패턴별 Primary P50/P99, Mirror P50/P99 계산
  - **회귀 감지**: Mirror P50 > Primary P50 × 2이면 regression 플래그
- **Admin API**: `GET /admin/mirror/stats`

---

### 15. 향후 고도화 아이디어 (Future Enhancements)
- **Multi-Database Routing**: 단일 프록시 인스턴스에서 여러 데이터베이스를 동시 프록시 (StartupMessage의 `database` 필드로 분기).
- **Auto Failover (Writer)**: Writer 장애 감지 시 standby 자동 promote 또는 새 Writer 주소로 전환.
- **Multi-Tenant Routing**: 테넌트별 백엔드 라우팅 (단일 백엔드 아키텍처 변경 필요).
- **K8s Operator (CRD)**: Helm 이상의 자동화가 필요할 때 별도 프로젝트로 구현.

> 상세 로드맵은 `docs/tasks-next.md` 참고.
