## 차기 Task (Phase 14-16)

---

### Phase 14: Audit Logging & Slow Query Tracker (W23)

**목표**: 모든 쿼리(또는 느린 쿼리만)를 구조화 로그로 기록하고, 임계값 초과 시 Webhook(Slack 등)으로 알림을 전송하는 감사/모니터링 기능.

**현재 상태**: `slog` 로깅 + `dbproxy_query_duration_seconds` 히스토그램이 이미 있으나, 개별 쿼리 단위 감사 로그와 Slow Query 알림은 미구현.

**핵심 아이디어**:
- 쿼리 처리 완료 시점에 duration 측정 → 임계값(`slow_query_threshold`) 초과 시 Slow Query 로그
- `log_all_queries: true`이면 모든 쿼리를 감사 로그로 기록 (user, source IP, query, duration, target)
- Webhook URL이 설정되면 Slow Query 발생 시 비동기 HTTP POST 전송

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T14-1 | Audit 설정 구조체 + Logger 기반 | `feat(audit): Audit Logging 설정 및 구조체` |
| T14-2 | Slow Query Detection + 구조화 로그 | `feat(audit): Slow Query 감지 및 구조화 로그` |
| T14-3 | Webhook 알림 (Slack 등) | `feat(audit): Slow Query Webhook 알림` |
| T14-4 | Audit 메트릭 + Admin API 연동 | `feat(audit): Audit 메트릭 및 Admin API` |
| T14-5 | Audit Logging 테스트 | `test: Audit Logging 단위/통합 테스트` |

#### T14-1: Audit 설정 구조체 + Logger 기반
- **범위**: `config.go`에 `AuditConfig` 추가, `internal/audit/audit.go`에 `AuditLogger` 구조체
- **완료 기준**: YAML 설정 파싱, AuditLogger 생성 + 인터페이스 정의
- **설정 항목**:
  ```yaml
  audit:
    enabled: true
    slow_query_threshold: "500ms"     # 이 이상이면 slow query
    log_all_queries: false            # true면 모든 쿼리 감사 로그
    webhook:
      enabled: false
      url: "https://hooks.slack.com/services/..."
      timeout: "5s"
  ```

#### T14-2: Slow Query Detection + 구조화 로그
- **범위**: `server.go`의 쿼리 처리 완료 시점에서 duration 체크 → AuditLogger에 이벤트 전달
- **완료 기준**: slow_query_threshold 초과 쿼리 → `slog.Warn` 구조화 로그 출력
- **로그 필드**:
  - `event`: "slow_query" 또는 "query"
  - `user`: 백엔드 유저명
  - `source_ip`: 클라이언트 IP
  - `query`: SQL 텍스트 (truncate 가능)
  - `duration_ms`: 실행 시간 (밀리초)
  - `target`: "writer" 또는 "reader"
  - `cached`: 캐시 히트 여부
- **성능 고려**: 로그 이벤트는 채널로 비동기 전달, 메인 쿼리 경로 블로킹 없음

#### T14-3: Webhook 알림 (Slack 등)
- **범위**: Slow Query 발생 시 Webhook URL로 HTTP POST 전송 (비동기 goroutine)
- **완료 기준**: Slow Query 감지 → Webhook 호출 → Slack 메시지 수신
- **핵심 로직**:
  - 전용 goroutine + 버퍼 채널로 비동기 전송 (쿼리 경로 비블로킹)
  - Rate limiting: 동일 쿼리 패턴에 대해 최소 interval (예: 1분) 보장
  - Payload: JSON 포맷 (Slack Incoming Webhook 호환)
- **Payload 예시**:
  ```json
  {
    "text": "[db-proxy] Slow Query Detected",
    "attachments": [{
      "color": "danger",
      "fields": [
        {"title": "Query", "value": "SELECT * FROM large_table", "short": false},
        {"title": "Duration", "value": "2.5s", "short": true},
        {"title": "Target", "value": "reader", "short": true}
      ]
    }]
  }
  ```

#### T14-4: Audit 메트릭 + Admin API 연동
- **범위**: Prometheus 메트릭 추가 + Admin API에서 slow query 통계 조회
- **완료 기준**: `/metrics`에 slow query 카운터 노출, `/admin/stats`에 slow query 통계 포함
- **메트릭**:
  - `dbproxy_slow_queries_total{target="writer|reader"}` — Slow Query 카운터
  - `dbproxy_audit_webhook_sent_total` — Webhook 전송 횟수
  - `dbproxy_audit_webhook_errors_total` — Webhook 전송 실패 횟수

#### T14-5: Audit Logging 테스트
- **범위**: 단위 테스트 (AuditLogger, Webhook 전송) + 통합 테스트 (Slow Query 감지 E2E)
- **완료 기준**: 전체 테스트 통과, Webhook mock 서버 검증

---

### Phase 15: Helm Chart (W24)

**목표**: Kubernetes에 db-proxy를 배포하기 위한 Helm Chart 제공. Operator는 별도 프로젝트로 분리.

**현재 상태**: Dockerfile 있음. Helm Chart 미작성.

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T15-1 | Dockerfile 최적화 (multi-stage) | `chore(docker): Dockerfile multi-stage 빌드 최적화` |
| T15-2 | Helm Chart 스캐폴딩 | `feat(deploy): Helm Chart 기본 구조` |
| T15-3 | Helm values + templates 완성 | `feat(deploy): Helm Chart values 및 templates` |
| T15-4 | Helm Chart 문서화 | `docs: Helm Chart 배포 가이드` |

#### T15-1: Dockerfile 최적화
- **범위**: Multi-stage build (빌드 스테이지 → scratch/distroless 런타임), cgo 빌드 환경 포함 (pg_query_go)
- **완료 기준**: 최종 이미지 사이즈 50MB 이하, `docker build` 성공

#### T15-2: Helm Chart 스캐폴딩
- **범위**: `deploy/helm/db-proxy/` 디렉토리에 `Chart.yaml`, `values.yaml`, `templates/` 기본 구조
- **완료 기준**: `helm template` 실행 시 유효한 K8s 매니페스트 생성

#### T15-3: Helm values + templates 완성
- **범위**: Deployment, Service, ConfigMap (config.yaml), HPA, PDB, ServiceMonitor (Prometheus Operator)
- **완료 기준**: `helm install` → Pod Running, 프록시 접속 가능
- **values.yaml 핵심**:
  ```yaml
  replicaCount: 2
  image:
    repository: db-proxy
    tag: latest
  config:
    writer: { host: "...", port: 5432 }
    readers: [{ host: "...", port: 5432 }]
    pool: { min_connections: 5, max_connections: 50 }
  resources:
    requests: { cpu: 100m, memory: 128Mi }
    limits: { cpu: 500m, memory: 256Mi }
  ```

#### T15-4: Helm Chart 문서화
- **범위**: README에 Helm 배포 가이드 추가
- **완료 기준**: `helm install` 원커맨드 배포 예시 문서화

---

### Phase 16: Serverless Data API (W25)

**목표**: HTTP REST API를 통해 SQL 쿼리를 실행할 수 있는 엔드포인트 제공. Lambda/Edge 함수에서 TCP 커넥션 비용 없이 DB 접근 가능.

**현재 상태**: Admin API HTTP 서버 (`internal/admin/admin.go`) 존재. 기존 Router, Cache, Firewall, Rate Limiter 모두 재활용 가능.

**핵심 아이디어**:
- `POST /v1/query` — SQL + params → 내부 풀링된 커넥션으로 실행 → JSON 응답
- 기존 모든 기능 (R/W 라우팅, 캐싱, 방화벽, Rate Limiting) 투명하게 적용
- 인증: API Key 또는 JWT (Bearer token)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T16-1 | Data API 설정 및 HTTP 서버 | `feat(api): Serverless Data API 설정 및 HTTP 서버` |
| T16-2 | PG 결과 → JSON 변환기 | `feat(api): PG wire protocol 결과 JSON 변환` |
| T16-3 | API 인증 (API Key / JWT) | `feat(api): Data API 인증` |
| T16-4 | 기존 기능 통합 (Cache, Firewall, Router) | `feat(api): Data API 기존 기능 통합` |
| T16-5 | Data API 테스트 및 문서화 | `test: Data API E2E 테스트` |

#### T16-1: Data API 설정 및 HTTP 서버
- **범위**: `config.go`에 `DataAPIConfig` 추가, 별도 포트에서 HTTP 서버 제공
- **설정**:
  ```yaml
  data_api:
    enabled: true
    listen: "0.0.0.0:8080"
    auth:
      type: "api_key"         # "api_key" 또는 "jwt"
      api_keys: ["key1", "key2"]
  ```

#### T16-2: PG 결과 → JSON 변환기
- **범위**: PG DataRow 메시지를 파싱하여 JSON 배열로 변환
- **완료 기준**: `SELECT id, name FROM users` → `{"columns":["id","name"],"rows":[[1,"Alice"],[2,"Bob"]]}`
- **핵심 난이도**: RowDescription의 OID로 타입 매핑 (int4→number, text→string, bool→boolean, timestamp→string)

#### T16-3: API 인증
- **범위**: `Authorization: Bearer <key>` 헤더 검증, 미인증 시 401
- **완료 기준**: 유효한 API Key → 쿼리 실행, 무효 → 401 Unauthorized

#### T16-4: 기존 기능 통합
- **범위**: HTTP 요청에서 추출한 SQL을 기존 Router.Classify → Pool.Acquire → Firewall.Check → Cache.Get/Set 파이프라인에 주입
- **완료 기준**: HTTP API로 SELECT → 캐시 히트, DELETE without WHERE → 방화벽 차단

#### T16-5: Data API 테스트 및 문서화
- **범위**: HTTP 클라이언트 E2E 테스트, README에 API 사용법 추가
- **완료 기준**: 전체 테스트 통과, curl 예시 문서화

---

### 기각/보류 기획

| 기획 | 판단 | 사유 |
|------|------|------|
| K8s Operator (CRD) | 보류 | Helm Chart로 충분, Operator는 별도 프로젝트 규모 |
| Multi-Tenant Routing | 후순위 | 단일 백엔드 아키텍처 전면 변경 필요, Phase 16 이후 검토 |
| Distributed State (Gossip) | 기각 | Redis Pub/Sub으로 충분, 인메모리 캐시 레이턴시 장점 상실, 과도한 복잡도 |
