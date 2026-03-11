# DB Proxy

PostgreSQL 프록시 — 커넥션 풀링, R/W 쿼리 자동 분산, 반복 쿼리 캐싱.

## 참고 문서

작업 전 반드시 아래 문서를 읽고 맥락을 파악할 것.

| 문서 | 경로 | 내용 |
|------|------|------|
| 스펙 | `docs/spec.md` | 기능 요구사항, 설정 항목, YAML 설정 예시 |
| 구현 설계 | `docs/implementation.md` | 기술 스택, 프로젝트 구조, 자료구조/알고리즘, 요청 처리 흐름 |
| 워크플로우 | `docs/workflow.md` | Task 목록, Task 상세 정의, Agent Teams 활용, Git 플로우, 블로그 계획 |

## 기술 스택

- 언어: Go
- DB: PostgreSQL (wire protocol 직접 구현)
- 설정: YAML (`gopkg.in/yaml.v3`)
- 캐시: 인메모리 LRU (`container/list`)
- 로깅: `slog` (표준 라이브러리)
- 해시: FNV-1a (`hash/fnv`)
- 인증: MD5, SCRAM-SHA-256 (`golang.org/x/crypto/pbkdf2`)
- 메트릭: Prometheus (`prometheus/client_golang`) — Phase 7
- PG 드라이버 (테스트용): `lib/pq`

## 프로젝트 구조

```
cmd/db-proxy/main.go           # 진입점 (-debug 플래그)
internal/
  config/config.go             # 설정 파싱 (Backend, Metrics, Admin 포함)
  proxy/server.go              # TCP 리스너 + Pool/Router/Cache 통합
  proxy/pgconn.go              # PG 인증 연결 (MD5, SCRAM-SHA-256)
  pool/pool.go                 # 커넥션 풀링 (DialFunc, Discard)
  router/router.go, parser.go, balancer.go  # 쿼리 라우팅
  cache/cache.go               # LRU 캐시 + 테이블별 무효화
  metrics/metrics.go           # Prometheus 메트릭 (Phase 7)
  admin/admin.go               # Admin API (Phase 7)
tests/
  e2e_test.go                  # Docker E2E 테스트
```

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
- 프로젝트 페이지: `content/projects/db-proxy.md`
- 시리즈 포스트: `content/posts/YYYY-MM-DD-db-proxy-N-제목.md` (P1~P6 완료)
- Hugo frontmatter 형식, 기존 포스트 참고 (`content/projects/simple-queue-service.md`)
- 포스팅 시점과 주제는 `docs/workflow.md` 섹션 6 참고
