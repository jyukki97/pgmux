## DB Proxy 작업 워크플로우

---

### 작업 개요

| # | 작업 | 설명 | 상태 |
|---|------|------|------|
| W1 | 프로젝트 초기 세팅 | Go 모듈 초기화, 디렉토리 구조 생성, CI 설정, linter 설정 | 완료 |
| W2 | 설정 시스템 | YAML 설정 파일 파싱, 설정 구조체 정의, validation | 완료 |
| W3 | TCP 프록시 서버 | TCP 리스너, PostgreSQL wire protocol 핸드셰이크, 쿼리 릴레이 | 완료 |
| W4 | 커넥션 풀링 | 풀 생성/관리, 커넥션 획득/반환, 유휴 타임아웃, 최대 수명 관리 | 완료 |
| W5 | 커넥션 헬스체크 | 주기적 ping, 비정상 커넥션 교체, min_connections 유지 | 완료 |
| W6 | 쿼리 파서 | SQL 키워드 기반 R/W 분류, 힌트 주석 파싱, 테이블명 추출 | 완료 |
| W7 | R/W 라우팅 | Writer/Reader 분기, 트랜잭션 세션 추적, read_after_write_delay | 완료 |
| W8 | Reader 로드밸런싱 | 라운드로빈 분산, Reader 장애 감지 및 자동 제외/복구 | 완료 |
| W9 | 쿼리 캐싱 | LRU 캐시 구현, TTL 만료, 캐시 키 해싱 | 완료 |
| W10 | 캐시 무효화 | 쓰기 시 테이블별 캐시 무효화, 테이블-캐시 역인덱스 관리 | 완료 |
| W11 | 테스트 & 벤치마크 | 통합 테스트(docker-compose), 벤치마크, 부하 테스트 | 완료 |
| W12 | 블로그 포스팅 | 개발 과정 및 기술적 내용 블로그 정리 | 완료 |
| W13 | Prometheus 메트릭 | 풀/캐시/라우팅 메트릭 수집, `/metrics` 엔드포인트 | 완료 |
| W14 | Prepared Statement 라우팅 | Extended Query Parse에서 SQL 추출, reader 라우팅 | 완료 |
| W15 | Admin API | HTTP 관리 인터페이스 (stats, health, cache flush) | 완료 |
| W16 | Transaction Pooling | Writer 커넥션 다중화, 트랜잭션 레벨 풀링 | 완료 |
| W17 | SSL/TLS + Front-end Auth | TLS Termination, 프록시 자체 인증 | 완료 |
| W18 | Circuit Breaker & Rate Limiting | 연쇄 장애 방어, 트래픽 제한 | 완료 |
| W19 | Zero-Downtime Reload | SIGHUP 무중단 설정 리로드 | 완료 |
| W20 | LSN 기반 Causal Consistency | Replication Lag 인지형 라우팅, Writer LSN 트래킹 | 완료 |
| W21-22 | AST 파서 + 쿼리 방화벽 | pg_query_go AST 파서 도입, 쿼리 방화벽, Semantic Caching | 완료 |
| W23 | Audit Logging & Slow Query | 쿼리 감사 로그, Slow Query 감지, Webhook 알림 | 진행 중 |
| W24 | Helm Chart | K8s 배포용 Helm Chart, Dockerfile 최적화 | 진행 중 |
| W25 | Serverless Data API | HTTP REST → PG Protocol 변환, JSON 응답 | 미착수 |

---

### 세부 문서

| 문서 | 경로 | 내용 |
|------|------|------|
| 완료된 Task | `docs/tasks-completed.md` | Phase 1-13 Task 목록 및 상세 정의 |
| 초고도화 Task | `docs/tasks-enhancement.md` | Phase 12-13 Task 목록 및 상세 정의 |
| 차기 Task | `docs/tasks-next.md` | Phase 14-16 Task 목록 및 상세 정의 |
| Agent Teams | `docs/agent-teams.md` | Claude Code Agent Teams 활용 가이드 |
| Git 워크플로우 | `docs/git-workflow.md` | 브랜치 전략, 커밋, PR 규칙 |
| 블로그 계획 | `docs/blog-plan.md` | 포스팅 시점, 주제, 템플릿 |
