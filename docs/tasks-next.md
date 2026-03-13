## 다음 Phase Task (Phase 29+)

오픈소스 생태계 및 프로덕션 운영 관점에서 필요한 기능 목록.

---

### 1. 프로덕션 필수 (Production Safety)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| ~~**Query Timeout**~~ | ~~프록시 레벨 쿼리 타임아웃. 설정 시간 초과 시 백엔드에 `CancelRequest` 전송 후 클라이언트에 에러 반환. PgBouncer `query_timeout` 대응. `pool.query_timeout: 30s`~~ | ✅ **기구현** |
| **Idle Client Timeout** | 유휴 클라이언트 자동 연결 해제. 커넥션 누수 방지. PgBouncer `client_idle_timeout` 대응. `proxy.client_idle_timeout: 5m` | 높음 |
| **Connection Warming** | 프록시 시작 시 또는 Reader 추가 시 `min_connections`까지 백그라운드 사전 연결. 콜드 스타트 지연 제거 | 중간 |

---

### 2. 운영 도구 (Operational)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Online Maintenance Mode** | `POST /admin/maintenance` — 신규 쿼리 거부 + 진행 중 쿼리 완료 대기 + `503 Service Unavailable` 반환. DB 마이그레이션/패치 시 유용. PgBouncer `PAUSE`/`RESUME` 대응 | 중간 |
| **Read-Only Mode** | `POST /admin/readonly` — 모든 쓰기 쿼리를 프록시 단에서 즉시 거부. Writer 장애 또는 긴급 유지보수 시 서비스 가용성 유지 | 중간 |
| **Health Check Endpoint (LB용)** | `/healthz` (liveness), `/readyz` (readiness) 분리. K8s probe 전용 경로. 현재 `/admin/health`는 인증/관리용이므로 LB용 경량 엔드포인트 별도 필요 | 중간 |
| **Config Validation CLI** | `pgmux validate config.yaml` — 설정 파일 문법/의미 검증만 수행 후 종료. CI/CD 파이프라인에서 배포 전 사전 검증용 | 낮음 |

---

### 3. 멀티테넌시 (Multi-Tenancy)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Per-User Rate Limiting** | 현재 전역 Rate Limit만 존재. 사용자별/IP별 차등 제한. `rate_limit.per_user: {default: 100, users: {batch_user: 10}}` | 중간 |

---

### 4. 킬러 피처 (Differentiators)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Query Rewriting Rules** | AST 기반 쿼리 변환 규칙. `SELECT *` → 컬럼 명시, deprecated 테이블명 자동 치환, 조건 자동 추가 등. 무중단 스키마 마이그레이션 지원 | 중간 |

---

### 5. 관측성 (Observability)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Active Session Dashboard** | `GET /admin/sessions` — 현재 활성 세션 목록 (클라이언트 IP, 실행 중 쿼리, 지속 시간, 할당된 백엔드). `pg_stat_activity`의 프록시 버전. `POST /admin/sessions/{id}/cancel`로 특정 세션 강제 종료 | 중간 |

---

### 6. 오픈소스 생태계 (DX & Community)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **문서 사이트 (GitHub Pages)** | MkDocs 또는 Hugo 기반. 설정 레퍼런스, 아키텍처 가이드, PgBouncer → pgmux 마이그레이션 가이드 | 중간 |
| **CONTRIBUTING.md + Issue Templates** | 컨트리뷰터 가이드, PR 템플릿, 이슈 템플릿. 오픈소스 커뮤니티 진입장벽 낮추기 | 낮음 |

---

### 추천 실행 로드맵

```
Phase 29: Query Timeout                        ✅ 완료 — CancelRequest 기반, 힌트 오버라이드, Prometheus 메트릭
Phase 29: Idle Client Timeout                  ← 프로덕션 안전장치
Phase 30: Query Rewriting Rules                ← 무중단 마이그레이션 지원
Phase 31: Per-User Rate Limiting               ← 멀티테넌시 강화
Phase 32: Online Maintenance + Read-Only Mode  ← 운영 도구
Phase 33: Active Session Dashboard             ← 관측성
Phase 34: Health Check Endpoint + Config CLI   ← K8s/CI 연동
Phase 35: 문서 사이트                            ← 오픈소스 생태계
```

---

### 의도적 미지원

아래 기능은 프록시의 책임 범위를 벗어나거나 기존 도구로 충분하여 지원하지 않음.

- **Sharding** — Citus 등 전용 솔루션 영역
- **Auto Failover (promote)** — Patroni/Stolon 영역. pgmux는 config reload + health check로 Writer 전환 감지/라우팅만 담당
- **Plugin System** — Go plugin은 빌드 제약, Wasm은 성능 악영향. 실제 수요 희박
- **Query Result Streaming** — 캐싱(버퍼링) 아키텍처와 근본 충돌
- **DNS Service Discovery** — K8s headless service + config reload로 충분
- **Read Replica Auto-Discovery** — 클라우드 벤더 SDK 의존 부적절. K8s operator 또는 config reload로 해결
- **pgmuxctl CLI** — Admin API + curl/httpie로 충분
