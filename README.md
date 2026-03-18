**한국어** | [English](README_en.md)

# pgmux

[![CI](https://github.com/jyukki97/pgmux/actions/workflows/ci.yml/badge.svg)](https://github.com/jyukki97/pgmux/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/jyukki97/pgmux?style=flat-square)](https://github.com/jyukki97/pgmux/releases/latest)
[![Go Version](https://img.shields.io/github/go-mod/go-version/jyukki97/pgmux?style=flat-square)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat-square)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/jyukki97/pgmux)](https://goreportcard.com/report/github.com/jyukki97/pgmux)
[![Docker Image](https://img.shields.io/badge/GHCR-pgmux-blue?style=flat-square&logo=docker)](https://github.com/jyukki97/pgmux/pkgs/container/pgmux)

Go로 작성한 경량 PostgreSQL 프록시. 애플리케이션과 데이터베이스 사이에서 커넥션 풀링, 읽기/쓰기 쿼리 자동 분산, 반복 쿼리 캐싱을 수행합니다. 기존 PostgreSQL 드라이버를 그대로 사용할 수 있습니다.

## 주요 기능

- **트랜잭션 레벨 커넥션 풀링** — Writer/Reader 모두 커넥션 풀에서 관리합니다. 트랜잭션 시작 시 커넥션을 획득하고 종료 시 반환하여, 수천 클라이언트가 소수의 백엔드 커넥션을 공유합니다. 커넥션 반환 시 `DISCARD ALL`로 세션 상태를 초기화합니다.
- **읽기/쓰기 자동 라우팅** — `SELECT`는 Reader(Replica)로, 쓰기 쿼리(`INSERT`, `UPDATE`, `DELETE`, `MERGE`, `CALL`, `COMMENT`, DDL)는 Writer(Primary)로 자동 분산합니다. `EXPLAIN ANALYZE`가 쓰기 쿼리를 포함하면 Writer로 라우팅합니다. Reader 간 라운드로빈 로드밸런싱을 지원합니다.
- **쿼리 캐싱** — 반복되는 `SELECT` 쿼리 결과를 인메모리 LRU 캐시에 저장합니다. TTL 만료 및 쓰기 시 테이블 단위 자동 무효화를 지원하며, Redis Pub/Sub을 통해 다중 프록시 인스턴스 간 캐시 무효화를 전파합니다. **참고:** `CALL` (Stored Procedure)은 Writer로 라우팅되지만, 프로시저 내부에서 수정하는 테이블을 정적으로 알 수 없어 캐시 무효화가 자동으로 수행되지 않습니다. 프로시저가 데이터를 변경하는 경우 Admin API(`/admin/cache/flush`)를 통한 수동 무효화를 권장합니다.
- **Prepared Statement 라우팅** — Extended Query Protocol을 지원하여 `SELECT` Prepared Statement도 Reader로 라우팅합니다.
- **Prepared Statement Multiplexing** — `multiplex` 모드에서는 Prepared Statement의 Parse/Bind를 프록시가 인터셉트하여 파라미터를 안전하게 바인딩한 Simple Query로 합성합니다. Transaction Pooling 환경에서도 Prepared Statement를 사용할 수 있는 킬러 피처입니다 (PgBouncer에서는 불가능). SQL Injection 방어를 위한 타입별 리터럴 직렬화를 내장하고 있습니다.
- **Replication Lag 대응** — `read_after_write_delay` 타이머 기반 또는 LSN 기반 Causal Consistency를 선택할 수 있습니다. LSN 모드에서는 쓰기 후 Writer의 WAL LSN을 추적하여, 해당 LSN에 도달한 Reader에서만 읽기를 수행합니다.
- **AST 기반 쿼리 분류** — pg_query_go(PostgreSQL 실제 파서)를 활용하여 CTE, 서브쿼리, DDL 등 복잡한 쿼리도 정확하게 읽기/쓰기를 분류합니다. 기존 문자열 파서와 설정으로 전환 가능합니다.
- **쿼리 방화벽(Firewall)** — AST 분석으로 조건 없는 DELETE/UPDATE, DROP TABLE, TRUNCATE 등 위험 쿼리를 사전 차단합니다.
- **시맨틱 캐시 키** — 공백, 대소문자가 달라도 구조적으로 동일한 쿼리는 같은 캐시 키를 생성하여 캐시 히트율을 높입니다. 리터럴 값이 다르면 별도의 캐시 엔트리를 유지합니다.
- **힌트 기반 라우팅** — SQL 주석으로 라우팅을 강제할 수 있습니다: `/* route:writer */ SELECT ...`
- **트랜잭션 인식** — `BEGIN` ~ `COMMIT`/`ROLLBACK`/`ABORT` 내부의 모든 쿼리는 Writer로 전송됩니다.
- **Prometheus 메트릭** — 풀, 라우팅, 캐시 메트릭을 `/metrics` 엔드포인트로 노출합니다.
- **Admin API** — HTTP를 통해 런타임 통계 조회, 헬스체크, 캐시 플러시를 수행할 수 있습니다. Bearer API Key 인증과 RBAC(admin/viewer 역할 분리), 선택적 IP allowlist를 지원하며, 설정 변경 시 hot-reload로 즉시 반영됩니다.
- **Serverless Data API** — `POST /v1/query`로 HTTP를 통해 SQL을 실행하고 JSON 응답을 받습니다. Lambda/Edge 함수에서 TCP 커넥션 비용 없이 풀링된 커넥션을 재활용합니다. API Key 인증, 방화벽, 캐싱이 투명하게 적용됩니다. `COPY` 문은 HTTP 특성상 지원되지 않습니다.
- **Audit Logging & Slow Query Tracker** — 모든 쿼리 또는 느린 쿼리만 선별하여 구조화 감사 로그를 기록합니다. 임계값 초과 시 Slack 등 Webhook으로 알림을 전송하며, 동일 쿼리 중복 알림을 자동으로 억제합니다.
- **Session Compatibility Guard** — Transaction pooling 환경에서 세션 의존 기능(LISTEN/UNLISTEN, 세션 SET, DECLARE CURSOR, CREATE TEMP, PREPARE, advisory lock 등)을 감지하여 block/warn/pin/allow 모드로 제어합니다. `SET LOCAL`, `SET TRANSACTION`, `SET CONSTRAINTS`는 트랜잭션 범위이므로 감지에서 제외됩니다. Pin 모드에서는 해당 세션의 모든 쿼리를 Writer에 고정하고 백엔드 커넥션을 세션 수명 동안 유지합니다.
- **SQL Redaction / Safe Observability** — Audit log, OpenTelemetry span, slog, webhook 등 모든 외부 노출 경로에서 SQL 리터럴을 자동 마스킹합니다. `observability.sql_redaction` 설정으로 `none`(원본 노출), `literals`(리터럴을 `$1`, `$2`로 치환, 기본값), `full`(쿼리 fingerprint만 노출) 세 가지 정책을 선택할 수 있습니다. `pg_query` 파서 기반으로 정확한 리터럴 제거를 수행하며, 파싱 실패 시 regex fallback을 적용합니다. 내부 라우팅·캐싱에는 원본 SQL을 사용하므로 기능에 영향이 없습니다.
- **Query Mirroring** — 프로덕션 쿼리를 Shadow DB에 비동기로 미러링하여 지연 시간을 비교합니다. 쿼리 패턴별 P50/P99 레이턴시 비교, 자동 성능 회귀 감지, 테이블 필터, read_only/all 모드를 지원합니다. 프로덕션 트래픽에 영향 없이 DB 마이그레이션·인덱스 변경의 성능 영향을 사전 검증할 수 있습니다.
- **Query Digest / Top-N Queries** — 쿼리를 정규화(`$N` 치환)하여 패턴별 실행 횟수, 평균/P50/P99 레이턴시를 집계합니다. `GET /admin/queries/top`으로 가장 많이 실행된 쿼리 패턴을 확인하고, `POST /admin/queries/reset`으로 통계를 초기화할 수 있습니다. `pg_stat_statements`의 프록시 버전입니다.
- **Query Timeout** — 프록시 레벨 쿼리 타임아웃을 지원합니다. 설정된 시간을 초과하면 백엔드에 `CancelRequest`를 전송하여 쿼리를 취소합니다. `pool.query_timeout: 30s`로 전역 설정하거나, `/* timeout:5s */ SELECT ...` 힌트로 쿼리별 오버라이드가 가능합니다.
- **Idle Client Timeout** — 유휴 클라이언트를 자동으로 연결 해제합니다. `proxy.client_idle_timeout: 5m` 설정 시, 5분간 아무 쿼리도 보내지 않는 클라이언트에 FATAL(57P01)을 전송하고 연결을 종료합니다. 트랜잭션 진행 중에는 타임아웃이 적용되지 않으며, 설정 변경 시 hot-reload로 즉시 반영됩니다.
- **Online Maintenance Mode** — Admin API로 유지보수 모드를 즉시 활성화/비활성화합니다. 유지보수 모드에서는 신규 연결과 트랜잭션 외 쿼리를 거부하고, 진행 중인 트랜잭션은 완료될 때까지 허용합니다(drain). `/readyz`가 자동으로 503을 반환하여 LB/K8s가 트래픽을 차단합니다. 배포, 마이그레이션, 패치 시 안전한 트래픽 차단에 사용합니다.
- **Read-Only Mode** — `POST /admin/readonly`로 모든 쓰기 쿼리를 프록시 레벨에서 즉시 거부합니다. Writer 장애, 긴급 점검, 데이터 보호 상황에서 읽기 서비스를 유지하면서 데이터 변경을 차단합니다. `DELETE /admin/readonly`로 해제합니다.
- **Per-User / Per-Database Connection Limits** — 사용자별·데이터베이스별 최대 커넥션 수를 제한합니다. 멀티테넌트 환경에서 특정 사용자가 풀을 독점하는 것을 방지하며, PostgreSQL 표준 에러 코드(53300, `too_many_connections`)로 거부합니다. 설정 변경 시 hot-reload로 즉시 반영되며, `GET /admin/connections`로 현재 커넥션 현황을 조회할 수 있습니다.
- **Multi-Database Routing** — 단일 프록시 인스턴스에서 여러 PostgreSQL 데이터베이스를 동시에 라우팅합니다. 클라이언트의 `StartupMessage.database` 필드로 DB 그룹을 자동 분기하며, 각 DB별로 독립적인 Writer/Reader 풀, 밸런서, Circuit Breaker를 유지합니다.
- **OpenTelemetry 분산 추적** — 쿼리 파싱, 캐시 조회, 커넥션 풀 획득, 백엔드 실행까지 각 단계를 스팬으로 추적합니다. OTLP gRPC 또는 stdout 익스포터를 지원하며, Data API의 `traceparent` 헤더를 통한 컨텍스트 전파로 애플리케이션에서 DB까지 엔드투엔드 트레이싱이 가능합니다.
- **PostgreSQL Wire Protocol 직접 구현** — PG 프로토콜을 직접 처리(MD5 & SCRAM-SHA-256 인증)하므로, 어떤 표준 PG 드라이버든 수정 없이 연결할 수 있습니다.

## 아키텍처

```mermaid
graph LR
    App["App<br/>(pgx, JDBC, ...)"] -- "PG Wire Protocol" --> AUTH
    HTTP["HTTP Client"] -- "HTTP" --> DAPI

    subgraph pgmux
        DAPI["Data API :8080"] --> AUTH
        AUTH["Auth & Firewall"] --> RTR["R/W Router"]
        RTR --> CACHE["Cache (LRU)"]
        RTR --> POOL["Connection Pool"]
        CACHE -. miss .-> POOL
        ADM["Admin :9091"]
        MET["Metrics :9090"]
        AUDIT["Audit Logger"]
        OTEL["OTel Tracing"]
        MIRR["Query Mirror"]
    end

    POOL --> W[("Primary<br/>(Writer)")]
    POOL --> R1[("Replica 1<br/>(Reader)")]
    POOL --> R2[("Replica 2<br/>(Reader)")]
    MIRR -.-> S[("Shadow DB")]
```

## 성능 벤치마크

pgbench (PostgreSQL 표준 벤치마크 도구)로 측정한 Direct DB / pgmux / PgBouncer 3자 비교 결과입니다. 3회 반복 평균, 웜업 후 측정.

**SELECT-only (읽기 전용 워크로드)**

| Target | c=1 | c=10 | c=50 | c=100 |
|--------|-----|------|------|-------|
| Direct | 2,447 | 16,724 | 25,483 | 25,488 |
| **pgmux** | **2,467** | **14,482** | **21,069** | **20,137** |
| PgBouncer | 2,178 | 13,812 | 23,665 | 21,778 |

**TPC-B (혼합 읽기/쓰기 워크로드)**

| Target | c=1 | c=10 | c=50 | c=100 |
|--------|-----|------|------|-------|
| Direct | 413 | 2,282 | 3,306 | 3,156 |
| **pgmux** | **337** | **1,906** | **2,606** | **2,578** |
| PgBouncer | 370 | 2,070 | 2,757 | 2,745 |

> **c=1에서 pgmux(2,467)가 Direct(2,447)와 PgBouncer(2,178)보다 빠릅니다** — 커넥션 풀링이 오버헤드 없이 동작합니다. 고동시성 SELECT에서는 PgBouncer의 C 이벤트루프가 유리하지만(Go 고루틴 스케줄링 오버헤드), pgmux는 캐싱, 방화벽, 미러링, Prepared Statement Multiplexing 등 PgBouncer에 없는 기능을 제공합니다.
>
> 재현: `make bench-compare`

## 빠른 시작

### 사전 요구사항

- Go 1.25+
- PostgreSQL 16+ (Primary + Replica)
- Docker & Docker Compose (로컬 개발용)

### 빌드

```bash
make build
```

### Docker로 로컬 실행

Primary 1대 + Replica 2대 PostgreSQL 인스턴스를 실행합니다:

```bash
make docker-up
```

`config.yaml`을 Docker 인스턴스에 맞게 수정한 뒤 프록시를 실행합니다:

```bash
make run
```

### 접속

프록시는 완전히 투명하게 동작하므로, 어떤 PostgreSQL 클라이언트든 그대로 사용할 수 있습니다:

```bash
psql -h 127.0.0.1 -p 5432 -U postgres -d testdb
```

## 설정

프로젝트 루트에 `config.yaml`을 작성합니다:

```yaml
proxy:
  listen: "0.0.0.0:5432"
  shutdown_timeout: 30s              # Graceful shutdown 타임아웃 (기본: 30s)
  client_idle_timeout: 0             # 유휴 클라이언트 타임아웃 (0 = 무제한, 예: 5m)

pool:
  min_connections: 5
  max_connections: 50
  idle_timeout: 10m
  max_lifetime: 1h
  connection_timeout: 5s
  query_timeout: 0              # 쿼리 타임아웃 (0 = 무제한). 쿼리별 힌트: /* timeout:5s */
  reset_query: "DISCARD ALL"    # 커넥션 반환 시 세션 리셋 쿼리
  prepared_statement_mode: "proxy" # "proxy" (기본, 패스스루) | "multiplex" (Simple Query로 합성)

routing:
  read_after_write_delay: 500ms  # 타이머 기반 (causal_consistency와 양자택일)
  causal_consistency: false       # true: LSN 기반 Causal Consistency (read_after_write_delay 무시)
  ast_parser: false               # true: pg_query_go AST 파서 사용 (정확도↑, 성능 약간↓)

firewall:
  enabled: true
  block_delete_without_where: true
  block_update_without_where: true
  block_drop_table: false
  block_truncate: false

session_compatibility:
  enabled: true
  mode: "warn"                     # "block" | "warn" | "pin" | "allow"

observability:
  sql_redaction: "literals"        # "none" | "literals" | "full"

circuit_breaker:
  enabled: false
  error_threshold: 0.5           # 에러율 (0.0-1.0) — 이 비율 초과 시 차단
  open_duration: 10s              # Open 상태 유지 시간
  half_open_max: 3                # Half-Open 상태에서 허용할 최대 요청 수
  window_size: 10                 # 에러율 계산 윈도우 크기

rate_limit:
  enabled: false
  rate: 1000                      # 초당 허용 쿼리 수
  burst: 100                      # 최대 버스트 크기

cache:
  enabled: true
  cache_ttl: 10s
  max_cache_entries: 10000
  max_result_size: "1MB"
  invalidation:
    mode: "pubsub"          # "local" (기본값) 또는 "pubsub" (Redis)
    redis_addr: "localhost:6379"
    channel: "pgmux:invalidate"

audit:
  enabled: true
  slow_query_threshold: 500ms    # 이 이상이면 slow query로 기록
  log_all_queries: false          # true면 모든 쿼리 감사 로그
  webhook:
    enabled: false
    url: "https://hooks.slack.com/services/..."
    timeout: 5s

mirror:
  enabled: false
  host: "shadow-db.internal"
  port: 5432
  # user, password, database: 미설정 시 backend 값 사용
  mode: "read_only"               # "read_only" (기본) | "all"
  tables: []                       # 빈 배열 = 모든 테이블
  compare: true                    # 패턴별 P50/P99 레이턴시 비교
  workers: 4                       # 미러링 워커 수
  buffer_size: 10000               # 비동기 큐 크기 (초과 시 드롭)

digest:
  enabled: true
  max_patterns: 1000               # 추적할 최대 고유 쿼리 패턴 수
  samples_per_pattern: 1000        # 패턴별 P50/P99 계산용 샘플 수

databases:
  mydb:
    writer:
      host: "primary.db.internal"
      port: 5432
    readers:
      - host: "replica-1.db.internal"
        port: 5432
      - host: "replica-2.db.internal"
        port: 5432
    backend:
      user: "postgres"
      password: "postgres"
      database: "mydb"
  # otherdb:
  #   writer:
  #     host: "primary-2.db.internal"
  #     port: 5432
  #   backend:
  #     user: "admin"
  #     password: "secret"
  #     database: "otherdb"

backend:                          # 공유 기본값 — databases에서 미지정 시 사용
  user: "postgres"
  password: "postgres"

connection_limits:
  enabled: true
  default_max_connections_per_user: 100     # 사용자별 기본 최대 커넥션 (0 = 무제한)
  default_max_connections_per_database: 200  # DB별 기본 최대 커넥션 (0 = 무제한)

auth:
  enabled: true
  users:
    - username: "app_user"
      password: "secret"
      max_connections: 50          # 사용자별 오버라이드 (기본값 대신 적용)
    - username: "admin_user"
      password: "secret"
      max_connections: 0           # 0 = 무제한

metrics:
  enabled: true
  listen: "0.0.0.0:9090"

admin:
  enabled: true
  listen: "0.0.0.0:9091"
  auth:
    enabled: true
    api_keys:
      - key: "your-admin-api-key"
        role: "admin"              # 전체 엔드포인트 접근 (GET + POST)
      - key: "your-viewer-api-key"
        role: "viewer"             # GET 엔드포인트만 접근
    ip_allowlist:                  # 선택사항 — 허용 IP/CIDR (비어있으면 모든 IP 허용)
      - "10.0.0.0/8"
      - "172.16.0.0/12"
    trusted_proxies:               # 선택사항 — X-Forwarded-For를 신뢰할 리버스 프록시 IP/CIDR (비어있으면 XFF 무시)
      - "10.0.0.1"
      - "172.16.0.0/12"

data_api:
  enabled: false
  listen: "0.0.0.0:8080"
  api_keys:
    - "your-secret-key"

tls:
  enabled: false
  cert_file: "/path/to/server.crt"
  key_file: "/path/to/server.key"

config:
  watch: false                   # true: fsnotify로 설정 파일 변경 감시 (hot-reload)

telemetry:
  enabled: false
  exporter: "otlp"            # "otlp" (gRPC) 또는 "stdout"
  endpoint: "localhost:4317"   # OTLP Collector gRPC 엔드포인트
  service_name: "pgmux"
  sample_ratio: 1.0            # 0.0 ~ 1.0 (샘플링 비율)
```

## Makefile 명령어

| 명령어 | 설명 |
|--------|------|
| `make build` | `bin/pgmux` 바이너리 빌드 |
| `make run` | 빌드 후 실행 |
| `make test` | 전체 단위 테스트 실행 |
| `make test-integration` | E2E 통합 테스트 실행 |
| `make test-coverage` | 테스트 커버리지 리포트 생성 |
| `make bench` | 컴포넌트 벤치마크 실행 |
| `make bench-compare` | Direct/pgmux/PgBouncer 3자 비교 벤치마크 |
| `make lint` | golangci-lint 실행 |
| `make docker-up` | 로컬 PostgreSQL Primary + Replica 실행 |
| `make docker-down` | Docker 컨테이너 정리 |
| `make clean` | 빌드 산출물 삭제 |

## 프로젝트 구조

```
cmd/pgmux/main.go              # 진입점
internal/
  config/config.go                # YAML 설정 파싱
  config/watcher.go               # 설정 파일 변경 감시 (fsnotify, ConfigMap symlink swap)
  proxy/server.go                 # Server 구조체, NewServer, Start, handleConn, Reload
  proxy/dbgroup.go                # DatabaseGroup (per-DB 풀, 밸런서, CB 캡슐화)
  proxy/auth.go                   # 인증 핸드셰이크 (relayAuth, frontendAuth)
  proxy/query.go                  # 메인 쿼리 루프 (relayQueries)
  proxy/query_read.go             # 읽기 쿼리 처리 (캐시 + fallback)
  proxy/query_extended.go         # 확장 쿼리 프로토콜 (Prepared Statement 라우팅)
  proxy/copy.go                   # COPY IN/OUT/BOTH 릴레이
  proxy/backend.go                # 백엔드 커넥션 관리 (acquire, reset, fallback)
  proxy/lsn.go                    # LSN 폴링 (Causal Consistency)
  proxy/helpers.go                # 유틸리티 (sendError, parseSize, emitAuditEvent)
  proxy/connlimit.go              # Per-User/Per-DB 커넥션 제한 (ConnTracker)
  proxy/pgconn.go                 # PG 인증 (MD5, SCRAM-SHA-256)
  proxy/synthesizer.go            # Prepared Statement → Simple Query 합성 (Multiplexing)
  proxy/cancel.go                 # CancelRequest 프로토콜 처리
  pool/pool.go                    # 커넥션 풀 + 헬스체크
  router/router.go                # 쓰기/읽기 라우팅 결정 (Causal Consistency)
  router/parser.go                # 문자열 기반 쿼리 분류
  router/parser_ast.go            # AST 기반 쿼리 분류 (pg_query_go)
  router/ast.go                   # SQL AST 파싱 + 노드 순회
  router/balancer.go              # 라운드로빈 로드밸런서 + LSN-aware 라우팅
  router/lsn.go                   # PostgreSQL LSN 타입 파싱/비교
  router/firewall.go              # 쿼리 방화벽 (위험 쿼리 차단)
  router/session_compat.go        # Session Compatibility Guard (세션 의존 기능 감지/차단/핀)
  redact/redact.go                # SQL Redaction (리터럴 마스킹, fingerprint)
  audit/audit.go                  # Audit Logging + Slow Query Tracker
  dataapi/handler.go              # Serverless Data API (HTTP → PG)
  cache/cache.go                  # LRU 캐시 + 테이블별 무효화
  cache/invalidator.go            # Redis Pub/Sub 캐시 무효화 전파
  cache/normalize.go              # 시맨틱 캐시 키 (AST Parse+Deparse)
  protocol/message.go             # PG 와이어 프로토콜 메시지 파싱
  protocol/literal.go             # PG 타입별 SQL 리터럴 직렬화 (Injection 방어)
  resilience/ratelimit.go         # Token Bucket Rate Limiter
  resilience/breaker.go           # Circuit Breaker (Closed/Open/Half-Open)
  metrics/metrics.go              # Prometheus 메트릭
  telemetry/telemetry.go          # OpenTelemetry 분산 추적
  mirror/mirror.go                # Query Mirroring (Shadow DB 비동기 전송)
  mirror/stats.go                 # 패턴별 P50/P99 레이턴시 비교 통계
  digest/digest.go                # Query Digest (Top-N 쿼리 패턴 통계)
  admin/admin.go                  # Admin HTTP API
tests/
  e2e_test.go                     # Docker 기반 E2E 테스트
  integration_test.go             # 통합 테스트
  benchmark_test.go               # 벤치마크
Dockerfile                        # Multi-stage 빌드
deploy/grafana/                   # Grafana 대시보드 JSON 템플릿
deploy/helm/pgmux/             # Kubernetes Helm Chart
```

## Health Check (LB / K8s Probe)

인증 없이 접근 가능한 경량 헬스체크 엔드포인트입니다. 로드밸런서, K8s liveness/readiness probe 연동에 사용합니다.

| 엔드포인트 | 메서드 | 인증 | 설명 |
|-----------|--------|------|------|
| `/healthz` | GET | 불필요 | Liveness — 프로세스 생존 확인. 항상 `200 {"status":"ok"}` |
| `/readyz` | GET | 불필요 | Readiness — 유지보수 모드이거나 Writer 백엔드 TCP 연결 불가 시 `503 {"status":"not_ready","reason":"..."}`, 정상 시 `200 {"status":"ready"}` |

**K8s probe 설정 예시:**

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 9091
  initialDelaySeconds: 5
  periodSeconds: 10
readinessProbe:
  httpGet:
    path: /readyz
    port: 9091
  initialDelaySeconds: 5
  periodSeconds: 5
```

> `/admin/health`는 상세 진단용 (Writer + Reader별 개별 상태)이며 인증이 필요합니다. probe에는 `/healthz`, `/readyz`를 사용하세요.

## Admin API

`admin.auth.enabled: true`로 인증을 활성화하면 모든 엔드포인트에 Bearer API Key가 필요합니다:

```bash
curl -H "Authorization: Bearer your-admin-api-key" http://localhost:9091/admin/stats
```

| 엔드포인트 | 메서드 | 필요 역할 | 설명 |
|-----------|--------|----------|------|
| `/admin/stats` | GET | viewer | 풀, 캐시, 라우팅 통계 |
| `/admin/health` | GET | viewer | 백엔드 헬스 상태 (Writer + Reader별 상세 진단) |
| `/admin/config` | GET | viewer | 현재 적용된 설정 (비밀번호/API Key 마스킹) |
| `/admin/cache/flush` | POST | admin | 전체 캐시 플러시 |
| `/admin/cache/flush/{table}` | POST | admin | 특정 테이블 캐시 무효화 |
| `/admin/reload` | POST | admin | 설정 핫 리로드 |
| `/admin/mirror/stats` | GET | viewer | 쿼리 미러링 통계 (패턴별 P50/P99, 회귀 감지) |
| `/admin/queries/top` | GET | viewer | Query Digest Top-N (패턴별 실행 횟수, 평균/P50/P99 레이턴시) |
| `/admin/queries/reset` | POST | admin | Query Digest 통계 초기화 |
| `/admin/connections` | GET | viewer | 사용자별/DB별 활성 커넥션 수 및 제한 현황 |
| `/admin/maintenance` | GET | viewer | 유지보수 모드 상태 조회 (`enabled`, `entered_at`) |
| `/admin/maintenance` | POST | admin | 유지보수 모드 진입 (신규 연결/쿼리 거부, 진행 중 트랜잭션 drain) |
| `/admin/maintenance` | DELETE | admin | 유지보수 모드 해제 |
| `/admin/readonly` | GET | viewer | Read-only 모드 상태 조회 |
| `/admin/readonly` | POST | admin | Read-only 모드 활성화 (쓰기 쿼리 거부) |
| `/admin/readonly` | DELETE | admin | Read-only 모드 해제 |

## 메트릭

`metrics.enabled`가 `true`일 때, 설정된 주소에서 Prometheus 메트릭을 수집할 수 있습니다:

- `pgmux_pool_connections_open` — 역할별 열린 커넥션 수
- `pgmux_pool_connections_idle` — 유휴 커넥션 수
- `pgmux_pool_acquires_total` — 커넥션 획득 횟수
- `pgmux_pool_acquire_duration_seconds` — 커넥션 획득 레이턴시
- `pgmux_queries_routed_total` — 라우팅된 쿼리 수 (writer/reader)
- `pgmux_query_duration_seconds` — 쿼리 처리 레이턴시
- `pgmux_reader_fallback_total` — Reader 장애 시 Writer 폴백 횟수
- `pgmux_cache_hits_total` / `pgmux_cache_misses_total` — 캐시 히트/미스
- `pgmux_cache_entries` — 현재 캐시 항목 수
- `pgmux_cache_invalidations_total` — 캐시 무효화 횟수
- `pgmux_reader_lsn_lag_bytes` — Reader별 WAL replay LSN (Causal Consistency 활성 시)
- `pgmux_firewall_blocked_total` — 방화벽 차단 횟수 (rule별)
- `pgmux_slow_queries_total` — Slow Query 감지 횟수 (target별)
- `pgmux_audit_webhook_sent_total` — Audit Webhook 전송 횟수
- `pgmux_audit_webhook_errors_total` — Audit Webhook 실패 횟수
- `pgmux_digest_patterns` — 현재 Query Digest 고유 패턴 수
- `pgmux_query_timeout_total` — 쿼리 타임아웃으로 취소된 쿼리 수 (target별)
- `pgmux_client_idle_timeout_total` — 유휴 타임아웃으로 종료된 클라이언트 연결 수
- `pgmux_connection_limit_rejected_total` — 커넥션 제한 초과로 거부된 연결 수
- `pgmux_active_connections_by_user` — 사용자별 현재 활성 커넥션 수
- `pgmux_active_connections_by_database` — DB별 현재 활성 커넥션 수
- `pgmux_maintenance_mode` — 유지보수 모드 활성 여부 (1 = 활성, 0 = 비활성)
- `pgmux_maintenance_rejected_total` — 유지보수 모드로 인해 거부된 연결/쿼리 수
- `pgmux_readonly_mode` — Read-only 모드 활성 여부 (1 = active, 0 = inactive)
- `pgmux_readonly_rejected_total` — Read-only 모드에서 거부된 쓰기 쿼리 수
- `pgmux_session_dependency_detected_total` — 세션 의존 기능 감지 횟수 (feature별)
- `pgmux_session_dependency_blocked_total` — 세션 의존 기능으로 차단된 쿼리 수 (feature별)
- `pgmux_session_pinned_total` — Writer에 고정된 세션 수 (feature별)

## Data API (HTTP)

TCP 커넥션 없이 HTTP로 SQL 쿼리를 실행할 수 있습니다:

```bash
curl -X POST http://localhost:8080/v1/query \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT id, name FROM users LIMIT 2"}'
```

응답:
```json
{
  "columns": ["id", "name"],
  "types": ["int4", "text"],
  "rows": [[1, "Alice"], [2, "Bob"]],
  "row_count": 2,
  "command": "SELECT 2"
}
```

R/W 라우팅, 캐싱, 방화벽, Rate Limiting이 투명하게 적용됩니다. `COPY` 문은 스트리밍 특성상 HTTP API에서 지원되지 않습니다.

## Grafana Dashboard

`deploy/grafana/pgmux-overview.json`에 Prometheus 데이터소스 기반 Grafana 대시보드 템플릿을 제공합니다.

### 대시보드 구성

| 섹션 | 패널 | 주요 메트릭 |
|------|------|------------|
| **Overview** | Total QPS, Avg Latency, Cache Hit Rate, Open Connections | 운영 핵심 지표 한눈에 |
| **Query Routing** | QPS by Target, Duration P50/P99, Reader Fallback | Writer/Reader 부하 분산 확인 |
| **Connection Pool** | Open/Idle Connections, Acquire Duration, Acquires/sec | 풀 포화도 모니터링 |
| **Cache** | Hits/Misses, Hit Rate Gauge, Entries & Invalidations | 캐시 효율 추적 |
| **Security** | Firewall Blocked, Rate Limited | 차단/제한 현황 |
| **Replication** | LSN Lag (bytes) per Reader | Replica 지연 감시 |
| **Audit** | Slow Queries, Webhook Sent/Errors | 느린 쿼리 추세 |
| **Query Digest** | Unique Patterns | 쿼리 다양성 추이 |

### 설치 방법

**Grafana UI에서 Import:**

1. Grafana → Dashboards → Import
2. `deploy/grafana/pgmux-overview.json` 파일 업로드
3. Prometheus 데이터소스 선택 → Import

**Helm Chart (Grafana Sidecar):**

```bash
helm install pgmux deploy/helm/pgmux/ \
  --set grafanaDashboard.enabled=true
```

`grafana_dashboard: "1"` 라벨이 적용된 ConfigMap이 생성되어 Grafana sidecar가 자동으로 대시보드를 로드합니다.

## Kubernetes 배포 (Helm)

### Docker 이미지

공식 이미지는 GitHub Container Registry에서 제공됩니다:

```bash
docker pull ghcr.io/jyukki97/pgmux:1.0.0    # 특정 버전
docker pull ghcr.io/jyukki97/pgmux:latest    # 최신
```

직접 빌드하려면:

```bash
make docker-build
```

### Helm Chart 설치

```bash
# values.yaml에서 databases 주소를 실제 DB로 수정한 뒤:
helm install pgmux deploy/helm/pgmux/ \
  --set image.repository=ghcr.io/jyukki97/pgmux \
  --set image.tag=1.0.0 \
  --set config.databases.mydb.writer.host=primary.db.internal \
  --set config.databases.mydb.backend.password=mypassword
```

### 주요 values

| 키 | 기본값 | 설명 |
|---|---|---|
| `replicaCount` | 2 | 프록시 Pod 수 |
| `config.*` | (config.yaml 전체) | pgmux 설정 |
| `autoscaling.enabled` | false | HPA 활성화 |
| `podDisruptionBudget.enabled` | true | PDB 활성화 |
| `serviceMonitor.enabled` | false | Prometheus Operator ServiceMonitor |
| `grafanaDashboard.enabled` | false | Grafana sidecar용 대시보드 ConfigMap |

## 기여

기여를 환영합니다. 가이드라인은 [CONTRIBUTING.md](CONTRIBUTING.md)를 참고해주세요.

## 라이선스

이 프로젝트는 [MIT 라이선스](LICENSE)를 따릅니다.
