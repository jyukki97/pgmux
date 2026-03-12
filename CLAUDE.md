# pgmux

PostgreSQL 프록시 — 커넥션 풀링, R/W 쿼리 자동 분산, 반복 쿼리 캐싱.

## 참고 문서

작업 전 반드시 아래 문서를 읽고 맥락을 파악할 것.

| 문서 | 경로 | 내용 |
|------|------|------|
| 스펙 | `docs/spec.md` | 기능 요구사항, 설정 항목, YAML 설정 예시 |
| 구현 설계 | `docs/implementation.md` | 기술 스택, 프로젝트 구조, 자료구조/알고리즘, 요청 처리 흐름 |
| 워크플로우 | `docs/workflow.md` | 작업 개요, 세부 문서 인덱스 |
| 전체 Task | `docs/tasks-completed.md` | 완료된 Phase/Task 목록 및 이슈/PR 번호 |
| 향후 Task | `docs/tasks-next.md` | Phase 20+ 고도화 추천 기능 및 로드맵 |
| Agent Teams | `docs/agent-teams.md` | Claude Code Agent Teams 활용 가이드 |
| Git 워크플로우 | `docs/git-workflow.md` | 브랜치 전략, 커밋, PR 규칙 |
| 블로그 계획 | `docs/blog-plan.md` | 포스팅 시점, 주제, 템플릿 |

## 기술 스택

- 언어: Go
- DB: PostgreSQL (wire protocol 직접 구현)
- 설정: YAML (`gopkg.in/yaml.v3`)
- 캐시: 인메모리 LRU (`container/list`)
- 로깅: `slog` (표준 라이브러리)
- 해시: FNV-1a (`hash/fnv`)
- 인증: MD5, SCRAM-SHA-256 (`golang.org/x/crypto/pbkdf2`)
- SQL 파싱: `pg_query_go/v5` (PostgreSQL C 파서 cgo 바인딩)
- 메트릭: Prometheus (`prometheus/client_golang`)
- 분산 추적: OpenTelemetry (`go.opentelemetry.io/otel`)
- 캐시 무효화: Redis Pub/Sub (`github.com/redis/go-redis/v9`)
- 파일 감시: fsnotify (`github.com/fsnotify/fsnotify`)
- PG 드라이버 (테스트용): `lib/pq`

## 프로젝트 구조

```
cmd/pgmux/main.go              # 진입점 (-debug 플래그)
internal/
  config/
    config.go                  # 설정 파싱 (Backend, Metrics, Admin 포함)
    watcher.go                 # 설정 파일 변경 감시 (fsnotify, ConfigMap symlink swap)
  proxy/
    server.go                  # Server 구조체, NewServer, Start, handleConn, Reload, getters
    auth.go                    # 인증 핸드셰이크 (relayAuth, frontendAuth)
    query.go                   # 메인 쿼리 루프 (relayQueries)
    query_read.go              # 읽기 쿼리 처리 (handleReadQueryTraced)
    query_extended.go          # 확장 쿼리 프로토콜 (Prepared Statement 라우팅)
    copy.go                    # COPY IN/OUT/BOTH 릴레이
    backend.go                 # 백엔드 커넥션 관리 (acquire, reset, fallback)
    lsn.go                     # LSN 폴링 (Causal Consistency)
    helpers.go                 # 유틸리티 (sendError, parseSize, emitAuditEvent 등)
    pgconn.go                  # PG 인증 연결 (MD5, SCRAM-SHA-256)
    synthesizer.go             # Prepared Statement Multiplexing
    cancel.go                  # CancelRequest 프로토콜 처리
  pool/pool.go                 # 커넥션 풀링 (DialFunc, Discard, 헬스체크)
  router/
    router.go                  # Writer/Reader 라우팅 결정 (Causal Consistency)
    parser.go                  # 문자열 기반 쿼리 분류
    parser_ast.go              # AST 기반 쿼리 분류 (pg_query_go)
    ast.go                     # SQL AST 파싱 + 노드 순회
    balancer.go                # 라운드로빈 로드밸런서 + LSN-aware 라우팅
    lsn.go                     # PostgreSQL LSN 타입 파싱/비교
    firewall.go                # 쿼리 방화벽 (위험 쿼리 차단)
  cache/
    cache.go                   # LRU 캐시 + 테이블별 무효화
    invalidator.go             # Redis Pub/Sub 캐시 무효화 전파
    normalize.go               # 시맨틱 캐시 키 (AST Parse+Deparse)
  protocol/
    message.go                 # PG 와이어 프로토콜 메시지 파싱
    literal.go                 # PG 타입별 SQL 리터럴 직렬화 (Injection 방어)
  resilience/
    ratelimit.go               # Token Bucket Rate Limiter
    breaker.go                 # Circuit Breaker (Closed/Open/Half-Open)
  metrics/metrics.go           # Prometheus 메트릭
  telemetry/telemetry.go       # OpenTelemetry 분산 추적
  audit/audit.go               # Audit Logging + Slow Query Tracker
  mirror/
    mirror.go                  # Query Mirroring (Shadow DB 비동기 전송, 워커 풀)
    stats.go                   # 패턴별 P50/P99 레이턴시 비교, 순환 버퍼, 회귀 감지
  dataapi/handler.go           # Serverless Data API (HTTP → PG)
  admin/admin.go               # Admin HTTP API
tests/
  e2e_test.go                  # Docker E2E 테스트
  integration_test.go          # 통합 테스트
  benchmark_test.go            # 벤치마크
```

## README 동기화

- 코드 수정 시 `README.md`에 기술된 내용(기능, 설정 예시, 프로젝트 구조, 메트릭 목록, Admin API 등)과 달라지는 부분이 있으면 **반드시 README.md도 함께 수정**할 것
- 새 기능 추가, 설정 항목 변경, 메트릭 추가/삭제, 엔드포인트 변경 등이 해당됨

## 코딩 컨벤션

- Go 표준 프로젝트 레이아웃 (`cmd/`, `internal/`)
- 외부 의존성 최소화 — 표준 라이브러리 우선
- 에러는 `fmt.Errorf("context: %w", err)` 형태로 래핑
- 테스트 파일은 같은 패키지 내 `_test.go`
- 로깅은 `slog` 사용, `log.Printf` 금지

## Git 규칙

- 브랜치: `feat/{issue번호}-{간단한설명}` (예: `feat/3-connection-pool-struct`)
- 커밋 메시지: conventional commits (예: `feat(pool): add Acquire/Release (#3)`)
- PR → Squash merge → 브랜치 삭제
- 작업 순서: Issue 등록 → 브랜치 → 작업 → PR → 리뷰 → 머지
- **이슈는 반드시 1건 1이슈로 분리** — 버그 수정, 기능 추가 등 성격이 다른 작업을 하나의 이슈/PR에 몰아넣지 말 것. Critical 버그는 즉시 개별 핫픽스 PR, Major 이하는 묶어도 되지만 커밋은 건별로 분리

## 작업 방식

- **Claude Code Agent Teams** 활용 (Phase별 팀 구성은 `docs/workflow.md` 섹션 4 참고)
- Task 단위로 작업 — Task 목록과 상세 정의는 `docs/workflow.md` 섹션 2, 3 참고
- 각 Task의 **범위**와 **완료 기준**을 반드시 확인 후 작업 시작

## 블로그 포스팅

- 블로그 경로: `/Users/nhn/Library/Mobile Documents/com~apple~CloudDocs/project/study-blog/`
- 프로젝트 페이지: `content/projects/pgmux.md`
- 시리즈 포스트: `content/posts/YYYY-MM-DD-pgmux-N-제목.md` (P1~P32 완료)
- Hugo frontmatter 형식, 기존 포스트 참고 (`content/projects/simple-queue-service.md`)
- 포스팅 시점과 주제는 `docs/workflow.md` 섹션 6 참고
