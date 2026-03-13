## 다음 Phase Task (Reprioritized)

현재 구현 범위는 이미 넓다. 다음 단계는 기능 수를 늘리는 것보다
프로덕션 운영 안전성, 관리면 보안, 세션 호환성 공백을 먼저 메우는 것이 맞다.

참고: `Query Timeout`, `Idle Client Timeout`은 이미 구현 완료되어 아래 목록에서 제외한다.

---

### 1. 필수 기능 (Must Have)

프로덕션 투입 전에 우선적으로 보강해야 하는 항목.

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Admin API Auth / RBAC** | `/admin/config`, `/admin/reload`, `/admin/cache/flush` 등 관리 엔드포인트 보호. API Key 또는 Basic Auth, 읽기/변경 권한 분리, 선택적 IP allowlist 또는 mTLS 지원 | 매우 높음 |
| **Health Check Endpoint (LB용)** | `/healthz` (liveness), `/readyz` (readiness) 분리. K8s probe, LB drain, maintenance 연동용 경량 엔드포인트. `/admin/health`와 역할 분리 | 높음 |
| **Online Maintenance Mode** | `POST /admin/maintenance` — 신규 쿼리 거부 + 진행 중 쿼리 drain 대기 + `503 Service Unavailable` 반환. 배포, 마이그레이션, 패치 시 안전한 트래픽 차단 | 높음 |
| **Read-Only Mode** | `POST /admin/readonly` — 모든 쓰기 쿼리를 프록시에서 즉시 거부. Writer 장애, 긴급 점검, 데이터 보호 상황에서 읽기 서비스 유지 | 높음 |
| **Session Compatibility Guard / Session Pinning** | Transaction pooling 환경에서 `LISTEN/UNLISTEN`, 세션 단위 `SET`, temp object, cursor 등 세션 의존 기능을 감지. 안전하게 session pinning 하거나 명시적으로 차단/메트릭 노출 | 높음 |
| **SQL Redaction / Safe Observability** | Audit log, tracing, admin 응답, webhook에서 리터럴/민감정보 마스킹. raw SQL과 normalized SQL 노출 정책 분리 | 높음 |

---

### 2. 있으면 좋은 기능 (Should Have)

운영 편의성과 멀티테넌시, 관측성을 강화하는 항목.

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Active Session Dashboard** | `GET /admin/sessions` — 활성 세션 목록, 실행 중 쿼리, 지속 시간, 클라이언트 IP, 할당 백엔드, 대기 시간 노출. `POST /admin/sessions/{id}/cancel`로 취소 지원 | 중간 |
| **Per-User Rate Limiting** | 현재 전역 Rate Limit만 존재. 사용자별/IP별 차등 제한. `rate_limit.per_user`, `rate_limit.per_ip` 형태 지원 | 중간 |
| **Connection Warming** | 프록시 시작 시 또는 Reader 추가 시 `min_connections`까지 백그라운드 사전 연결. reload 직후와 cold start의 tail latency 완화 | 중간 |
| **Replica Freshness Policy** | LSN 기반 causal consistency 외에 `max_replica_lag_bytes` 또는 `max_replica_lag_ms` 같은 정책 추가. 지나치게 뒤처진 Reader 자동 제외 | 중간 |
| **Config Validation CLI** | `pgmux validate config.yaml` — 설정 파일 문법/의미 검증만 수행 후 종료. CI/CD 배포 전 사전 검증용 | 낮음 |
| **Prometheus Alert Rules / Helm Templates** | Helm 차트에 `PrometheusRule` 템플릿 추가. 연결 고갈, reader fallback 급증, query timeout 증가, mirror drop 증가 등에 대한 기본 알림 제공 | 낮음 |

---

### 3. 더 발전하기 좋은 기능 (Expansion)

차별화 포인트를 강화하거나 제품 방향을 확장하는 항목.

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Query Rewriting Rules** | AST 기반 쿼리 변환 규칙. deprecated 테이블명 치환, 조건 자동 추가, 무중단 스키마 마이그레이션 지원 | 중간 |
| **Policy Engine** | 사용자/DB/테이블 기준으로 라우팅, 캐시, 방화벽, timeout, read-only 정책을 선언적으로 적용 | 중간 |
| **Data API Parameters / Transactions** | Data API에 파라미터 바인딩, 다중 statement transaction, idempotency key 지원. 서버리스/백엔드 통합성 강화 | 중간 |
| **Mirror Correctness Diff** | Query Mirroring을 성능 비교에서 한 단계 확장해 결과셋 샘플 diff 또는 checksum 비교 지원 | 낮음 |
| **PgBouncer Migration Guide / Compat Surface** | PgBouncer 사용자 전환을 돕는 문서와 호환 명령/상태 표면 정리. 운영자 학습 비용 감소 | 낮음 |
| **문서 사이트 (GitHub Pages)** | MkDocs 또는 Hugo 기반. 설정 레퍼런스, 아키텍처 가이드, PgBouncer → pgmux 마이그레이션 가이드 | 낮음 |
| **Issue / PR Templates 보강** | 오픈소스 운영을 위한 이슈 템플릿, 버그 리포트 가이드, 성능 회귀 리포트 템플릿 추가 | 낮음 |

---

### 추천 실행 로드맵

```
Phase 29: Idle Client Timeout (완료) + Admin API Auth
Phase 30: Healthz/Readyz + Maintenance + Read-Only Mode
Phase 31: Session Compatibility Guard + SQL Redaction
Phase 32: Active Session Dashboard
Phase 33: Per-User Rate Limiting + Connection Warming
Phase 34: Replica Freshness Policy + Config Validation CLI
Phase 35: Query Rewriting Rules
Phase 36: Policy Engine / Data API 확장
Phase 37: 문서 사이트 + OSS 운영 템플릿 + Alert Rules
```

원칙:

- 운영 안전장치가 차별화 기능보다 우선이다.
- 관리면 보안이 없는 상태에서 Admin API 확장은 뒤로 미룬다.
- transaction pooling 제품에서는 세션 호환성 명시가 기능 추가보다 중요하다.

---

### 의도적 미지원

아래 기능은 프록시의 책임 범위를 벗어나거나 기존 도구로 충분하여 지원하지 않음.

- **Sharding** — Citus 등 전용 솔루션 영역
- **Auto Failover (promote)** — Patroni/Stolon 영역. pgmux는 config reload + health check로 Writer 전환 감지/라우팅만 담당
- **Plugin System** — Go plugin은 빌드 제약이 크고 Wasm은 성능 비용이 큼
- **Full Query Result Streaming** — 현재 캐싱/버퍼링 아키텍처와 충돌
- **DNS Service Discovery** — K8s headless service + config reload로 충분
- **Read Replica Auto-Discovery** — 클라우드 벤더 SDK 의존보다 operator/config reload 조합이 적절
- **별도 pgbmuxctl CLI** — Admin API가 충분히 안정화되면 `curl`/`httpie`로 대체 가능
