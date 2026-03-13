## 다음 Phase Task (Phase 24+)

## 고도화 추천 기능

경쟁 제품(PgBouncer, PgCat, Odyssey) 대비 분석 및 오픈소스 생태계 관점에서 추천하는 기능 목록.

---

### 경쟁 제품 대비 현재 위치

| 기능 | PgBouncer | PgCat | Odyssey | **pgmux** |
|------|-----------|-------|---------|-----------|
| Transaction Pooling | O | O | O | **O** |
| R/W Splitting | X | O | O | **O** |
| Query Caching | X | X | X | **O** (차별점) |
| Query Firewall | X | X | X | **O** (차별점) |
| AST Parser | X | X | X | **O** (차별점) |
| Prepared Stmt Mux | X | X | X | **O** (차별점) |
| Query Mirroring | X | X | X | **O** (차별점) |
| CI/CD + Docker | O | O | O | **O** ✅ |
| Multi-DB | O | O | O | **O** ✅ |
| Audit Log | X | X | X | **O** (차별점) |
| Data API (HTTP) | X | X | X | **O** (차별점) |

pgmux는 **캐싱, 방화벽, AST 파서, Audit, Data API, Query Mirroring, Multi-DB** 등 경쟁 제품에 없는 고유 기능이 다수.
경쟁 제품 대비 기능 갭 없이 차별화된 기능 보유.

> **의도적 미지원**: 아래 기능은 프록시의 책임 범위를 벗어나거나 기존 도구로 충분하여 지원하지 않음.
> - **Sharding** — Citus 등 전용 솔루션 영역. 프록시에서 구현 시 분산 트랜잭션/크로스샤드 조인 등 복잡도만 증가
> - **Auto Failover (promote)** — Patroni/Stolon/pg_auto_failover 영역. 프록시에서 promote 결정 시 split-brain 위험. pgmux는 config reload + health check로 Writer 전환 감지/라우팅만 담당
> - **Plugin System** — Go plugin은 빌드 제약이 심하고, Wasm 런타임은 프록시 성능에 악영향. 실제 수요 희박
> - **Query Result Streaming** — 캐싱(버퍼링) 아키텍처와 근본적으로 충돌. 대용량 결과셋은 프록시를 우회하는 것이 적절
> - **DNS Service Discovery** — K8s headless service + config reload로 충분
> - **Read Replica Auto-Discovery** — 클라우드 벤더 SDK 의존을 프록시에 넣는 것은 부적절. K8s operator 또는 config reload로 해결
> - **pgmuxctl CLI** — Admin API + curl/httpie로 충분. CLI 래퍼는 유지보수 부담 대비 가치 낮음

---

### 1. 운영 안정성 (Production Readiness)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| ~~**Graceful Shutdown + Connection Draining**~~ | ~~SIGTERM 수신 시 신규 연결 거부 + 기존 쿼리 완료 대기 후 종료. K8s `terminationGracePeriodSeconds`와 연동 필수~~ | ✅ **기구현** |
| **Connection Warming** | 프록시 시작 시 또는 Reader 추가 시 `min_connections`까지 백그라운드 사전 연결. 콜드 스타트 지연 제거 | 중간 |
| **Online Maintenance Mode** | `POST /admin/maintenance` — 신규 쿼리 거부 + 진행 중 쿼리 완료 대기. DB 마이그레이션/패치 시 유용 | 중간 |

---

### 2. 멀티테넌시 (Multi-Tenancy)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| ~~**Multi-Database Routing**~~ | ~~단일 프록시 인스턴스에서 여러 데이터베이스를 동시 프록시. StartupMessage의 `database` 필드로 분기~~ | ✅ **기구현** |
| ~~**Per-User Connection Limits**~~ | ~~사용자별/데이터베이스별 최대 커넥션 수 제한. `auth.users[].max_connections`~~ | ✅ **기구현** |
| **Per-User Rate Limiting** | 현재 전역 Rate Limit만 존재. 사용자별/IP별 차등 제한 (`rate_limit.per_user`) | 중간 |

---

### 3. 오픈소스 킬러 피처 (Differentiators)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Query Rewriting Rules** | 정규식 또는 AST 기반 쿼리 변환 규칙. 예: `SELECT *` → 특정 컬럼으로 치환, deprecated 테이블명 자동 변환. 무중단 스키마 마이그레이션 지원 | 중간 |
| **Read-Only Mode** | `POST /admin/readonly` — 모든 쓰기 쿼리를 프록시 단에서 즉시 거부. Writer 장애 또는 긴급 유지보수 시 서비스 가용성 유지 | 중간 |

---

### 4. 관측성 강화 (Observability)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| ~~**Grafana Dashboard 템플릿**~~ | ~~`deploy/grafana/` 디렉토리에 JSON 대시보드 제공. 커넥션 풀, 캐시 히트율, 쿼리 레이턴시, 방화벽 차단 등 한눈에 확인~~ | ✅ **기구현** |
| ~~**Query Digest / Top-N Queries**~~ | ~~쿼리를 정규화(`$N` 치환)하여 패턴별 실행 횟수, 평균/P99 레이턴시 집계. `GET /admin/queries/top`~~ | ✅ **기구현** |
| **Connection 추적 대시보드** | `GET /admin/connections` — 현재 활성 세션 목록 (클라이언트 IP, 실행 중 쿼리, 지속 시간). `pg_stat_activity`의 프록시 버전 | 중간 |
| **Structured JSON Logging** | 현재 `slog` 사용 중이지만 출력 포맷 설정 추가 (`log.format: json|text`). 로그 수집기(Loki, ELK)와 연동 용이 | 낮음 |

---

### 5. 고가용성 (HA)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Health Check Endpoint (LB용)** | `/healthz` (liveness), `/readyz` (readiness) 분리. 현재 Admin API의 `/admin/health`는 있지만 별도 포트/경로로 LB 전용 엔드포인트 필요 | 중간 |

> Writer Failover는 Patroni/Stolon 등 전용 도구가 담당. pgmux는 config reload + health check로 Writer 전환을 감지하여 라우팅만 전환.

---

### 6. 개발자 경험 & 오픈소스 생태계 (DX & Community)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| ~~**공식 Docker Image (GHCR)**~~ | ~~GitHub Actions CI/CD + `ghcr.io/org/pgmux:latest` 자동 빌드/푸시~~ | ✅ **완료** (Phase 20, PR #170) |
| ~~**벤치마크 Suite & 비교 문서**~~ | ~~PgBouncer, PgCat 대비 성능 벤치마크. `make bench-compare`로 재현 가능한 결과. 오픈소스 선택 시 가장 먼저 보는 자료~~ | ✅ **기구현** |
| **문서 사이트 (GitHub Pages)** | MkDocs 또는 Hugo 기반. 설정 레퍼런스, 아키텍처 가이드, 마이그레이션 가이드(PgBouncer → pgmux) | 중간 |
| **CONTRIBUTING.md + Issue Templates** | 컨트리뷰터 가이드, PR 템플릿, 이슈 템플릿. 오픈소스 커뮤니티 참여 진입장벽 낮추기 | 중간 |
| ~~**GitHub Actions CI**~~ | ~~PR마다 lint + unit test + 벤치마크 리그레션 자동 실행~~ | ✅ **완료** (Phase 20, PR #170) |

---

### 추천 실행 로드맵

```
Phase 20: OSS Release Ready          ✅ 완료 — GitHub Actions CI/CD + Docker Image (GHCR) + lint 정비
Phase 22: Graceful Shutdown          ✅ 기구현 — ShutdownTimeout + wg.Wait + Connection Draining
Phase 23: Multi-Database Routing     ✅ 완료 — DatabaseGroup 추상화, per-DB 풀/밸런서/CB, 하위호환
Phase 24: Query Digest               ✅ 완료 — Top-N 쿼리 패턴 통계, Admin API, Prometheus 메트릭
Phase 25: Grafana Dashboard          ✅ 완료 — JSON 대시보드 템플릿 + Helm ConfigMap + Sidecar 연동
Phase 26: Benchmark Suite             ✅ 완료 — pgbench 기반 Direct/pgmux/PgBouncer 3자 비교
Phase 27: Query Path Optimization    ✅ 완료 — SELECT-only 46%→83%, hot path 최적화
Phase 28: Per-User Connection Limits ✅ 완료 — Per-User/Per-DB 커넥션 제한, Admin API, Prometheus 메트릭
Phase 29: Query Rewriting Rules      ← 무중단 마이그레이션 지원
Phase 29: Multi-Tenancy (Rate Limit) ← Per-User Rate Limiting
Phase 30: 문서 사이트                 ← 오픈소스 생태계
```
