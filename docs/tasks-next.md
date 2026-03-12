## 다음 Phase Task (Phase 20+)

## 고도화 추천 기능 (Phase 20+)

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
| Multi-DB | O | O | O | **미지원** (갭) |
| Auto Failover | X | O | X | **미지원** (갭) |
| Sharding | X | O | X | 미지원 |
| Audit Log | X | X | X | **O** (차별점) |
| Data API (HTTP) | X | X | X | **O** (차별점) |

pgmux는 **캐싱, 방화벽, AST 파서, Audit, Data API, Query Mirroring** 등 경쟁 제품에 없는 고유 기능이 다수.
반면 **Multi-Database**와 **Auto Failover**가 가장 큰 갭 — 오픈소스 채택률의 핵심.

---

### 1. 운영 안정성 (Production Readiness)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Graceful Shutdown + Connection Draining** | SIGTERM 수신 시 신규 연결 거부 + 기존 쿼리 완료 대기 후 종료. K8s `terminationGracePeriodSeconds`와 연동 필수 | **높음** |
| **Adaptive Pool Sizing** | 트래픽 패턴에 따라 `min_connections` ~ `max_connections` 사이에서 풀 크기를 자동 조절. 유휴 시간대 리소스 절약 | 중간 |
| **Connection Warming** | 프록시 시작 시 또는 Reader 추가 시 `min_connections`까지 백그라운드 사전 연결. 콜드 스타트 지연 제거 | 중간 |
| **Online Maintenance Mode** | `POST /admin/maintenance` — 신규 쿼리 거부 + 진행 중 쿼리 완료 대기. DB 마이그레이션/패치 시 유용 | 중간 |

---

### 2. 멀티테넌시 & 접근 제어 (Multi-Tenancy)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Multi-Database Routing** | 단일 프록시 인스턴스에서 여러 데이터베이스를 동시 프록시. StartupMessage의 `database` 필드로 분기. 현재는 단일 `backend.database`만 지원 | **높음** |
| **Per-User Connection Limits** | 사용자별/데이터베이스별 최대 커넥션 수 제한. `auth.users[].max_connections` | 중간 |
| **Per-User Rate Limiting** | 현재 전역 Rate Limit만 존재. 사용자별/IP별 차등 제한 (`rate_limit.per_user`) | 중간 |
| **IP Allow/Deny List** | `pg_hba.conf` 스타일 접근 제어. CIDR 기반 허용/차단 리스트 | 중간 |

---

### 3. 오픈소스 킬러 피처 (Differentiators)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Query Rewriting Rules** | 정규식 또는 AST 기반 쿼리 변환 규칙. 예: `SELECT *` → 특정 컬럼으로 치환, deprecated 테이블명 자동 변환. 무중단 스키마 마이그레이션 지원 | 중간 |
| **Read-Only Mode** | `POST /admin/readonly` — 모든 쓰기 쿼리를 프록시 단에서 즉시 거부. Writer 장애 또는 긴급 유지보수 시 서비스 가용성 유지 | 중간 |
| **Query Tagging & Routing Rules** | `/* app:payment, priority:high */` 같은 태그로 라우팅 규칙 정의. 특정 앱/마이크로서비스의 쿼리를 전용 Reader로 고정 | 중간 |

---

### 4. 관측성 강화 (Observability)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Grafana Dashboard 템플릿** | `deploy/grafana/` 디렉토리에 JSON 대시보드 제공. 커넥션 풀, 캐시 히트율, 쿼리 레이턴시, 방화벽 차단 등 한눈에 확인 | **높음** |
| **Query Digest / Top-N Queries** | 쿼리를 정규화(`$N` 치환)하여 패턴별 실행 횟수, 평균/P99 레이턴시 집계. `GET /admin/queries/top` | 중간 |
| **Connection 추적 대시보드** | `GET /admin/connections` — 현재 활성 세션 목록 (클라이언트 IP, 실행 중 쿼리, 지속 시간). `pg_stat_activity`의 프록시 버전 | 중간 |
| **Structured JSON Logging** | 현재 `slog` 사용 중이지만 출력 포맷 설정 추가 (`log.format: json|text`). 로그 수집기(Loki, ELK)와 연동 용이 | 낮음 |

---

### 5. 고가용성 & 자동 장애 대응 (HA)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Auto Failover (Writer)** | Writer 장애 감지 시 설정된 standby를 자동 promote (또는 새 Writer 주소로 전환). Patroni/Stolon 연동 또는 독립 구현 | **높음** |
| **DNS-based Service Discovery** | Reader 목록을 DNS SRV 레코드에서 자동 갱신. K8s headless service, AWS RDS Proxy 스타일. 수동 설정 변경 불필요 | 중간 |
| **Health Check Endpoint (LB용)** | `/healthz` (liveness), `/readyz` (readiness) 분리. 현재 Admin API의 `/admin/health`는 있지만 별도 포트/경로로 LB 전용 엔드포인트 필요 | 중간 |

---

### 6. 개발자 경험 & 오픈소스 생태계 (DX & Community)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **CLI 관리 도구 (`pgmuxctl`)** | `pgmuxctl stats`, `pgmuxctl cache flush`, `pgmuxctl reload` 등. Admin API의 CLI 래퍼. 운영자 UX 대폭 개선 | **높음** |
| **공식 Docker Image (GHCR)** | GitHub Actions CI/CD + `ghcr.io/org/pgmux:latest` 자동 빌드/푸시. Dockerfile은 있지만 배포 파이프라인 미구축 | **높음** |
| **벤치마크 Suite & 비교 문서** | PgBouncer, PgCat 대비 성능 벤치마크. `make bench-compare`로 재현 가능한 결과. 오픈소스 선택 시 가장 먼저 보는 자료 | **높음** |
| **문서 사이트 (GitHub Pages)** | MkDocs 또는 Hugo 기반. 설정 레퍼런스, 아키텍처 가이드, 마이그레이션 가이드(PgBouncer → pgmux) | 중간 |
| **CONTRIBUTING.md + Issue Templates** | 컨트리뷰터 가이드, PR 템플릿, 이슈 템플릿. 오픈소스 커뮤니티 참여 진입장벽 낮추기 | 중간 |
| **GitHub Actions CI** | PR마다 lint + unit test + E2E test + 벤치마크 리그레션 자동 실행 | 중간 |

---

### 7. 고급 기능 (Advanced / Long-term)

| 기능 | 설명 | 우선순위 |
|------|------|----------|
| **Read Replica Auto-Discovery** | AWS RDS API / K8s API를 통해 Reader 목록 자동 갱신. 수동 설정 제거 | 낮음 |
| **Sharding (Horizontal Partitioning)** | 테이블별 샤딩 키 기반 라우팅. `shard_key: user_id`. 장기 로드맵 | 낮음 |
| **Query Result Streaming** | 대용량 SELECT 결과를 스트리밍 방식으로 전달. 현재는 전체 버퍼링 후 캐시/전달 | 낮음 |
| **Plugin System** | 쿼리 처리 파이프라인에 사용자 정의 미들웨어 삽입. Go plugin 또는 Wasm | 낮음 |

---

### 추천 실행 로드맵

```
Phase 20: OSS Release Ready          ← GitHub Actions CI + Docker Image + 벤치마크
Phase 22: Multi-Database Routing     ← 단일 인스턴스 다중 DB (가장 요청 많을 기능)
Phase 23: Grafana + Query Digest     ← 관측성 강화
Phase 24: Auto Failover + DNS SD     ← 고가용성
Phase 25: pgmuxctl CLI               ← 운영자 UX
Phase 26: Query Rewriting Rules      ← 무중단 마이그레이션 지원
Phase 27: Multi-Tenancy              ← Per-User Limits + IP ACL
Phase 28: Advanced (Sharding 등)     ← 장기 로드맵
```
