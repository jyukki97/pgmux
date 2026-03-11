## DB Proxy 작업 워크플로우

---

### 1. 필요한 작업 정리

| # | 작업 | 설명 |
|---|------|------|
| W1 | 프로젝트 초기 세팅 | Go 모듈 초기화, 디렉토리 구조 생성, CI 설정, linter 설정 |
| W2 | 설정 시스템 | YAML 설정 파일 파싱, 설정 구조체 정의, validation |
| W3 | TCP 프록시 서버 | TCP 리스너, PostgreSQL wire protocol 핸드셰이크, 쿼리 릴레이 |
| W4 | 커넥션 풀링 | 풀 생성/관리, 커넥션 획득/반환, 유휴 타임아웃, 최대 수명 관리 |
| W5 | 커넥션 헬스체크 | 주기적 ping, 비정상 커넥션 교체, min_connections 유지 |
| W6 | 쿼리 파서 | SQL 키워드 기반 R/W 분류, 힌트 주석 파싱, 테이블명 추출 |
| W7 | R/W 라우팅 | Writer/Reader 분기, 트랜잭션 세션 추적, read_after_write_delay |
| W8 | Reader 로드밸런싱 | 라운드로빈 분산, Reader 장애 감지 및 자동 제외/복구 |
| W9 | 쿼리 캐싱 | LRU 캐시 구현, TTL 만료, 캐시 키 해싱 |
| W10 | 캐시 무효화 | 쓰기 시 테이블별 캐시 무효화, 테이블-캐시 역인덱스 관리 |
| W11 | 테스트 & 벤치마크 | 통합 테스트(docker-compose), 벤치마크, 부하 테스트 |
| W12 | 블로그 포스팅 | 개발 과정 및 기술적 내용 블로그 정리 |
| W13 | Prometheus 메트릭 | 풀/캐시/라우팅 메트릭 수집, `/metrics` 엔드포인트 |
| W14 | Prepared Statement 라우팅 | Extended Query Parse에서 SQL 추출, reader 라우팅 |
| W15 | Admin API | HTTP 관리 인터페이스 (stats, health, cache flush) |

---

### 2. Task 쪼개기

각 작업(W)을 구현 가능한 단위의 Task로 쪼갠다.

#### Phase 1: 프로젝트 기반 (W1 + W2)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T1-1 | Go 모듈 초기화, .gitignore, Makefile 생성 | `chore: 프로젝트 초기 세팅` |
| T1-2 | 설정 구조체 정의 및 YAML 파싱 구현 | `feat: YAML 설정 파싱 구현` |
| T1-3 | 설정 validation (필수값 체크, 범위 검증) | `feat: 설정 validation 추가` |

#### Phase 2: TCP 프록시 (W3)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T2-1 | TCP 리스너 및 클라이언트 접속 수락 | `feat: TCP 리스너 구현` |
| T2-2 | PG wire protocol 핸드셰이크 (Startup, Auth) | `feat: PostgreSQL 핸드셰이크 구현` |
| T2-3 | 쿼리 릴레이 (클라이언트 ↔ DB 양방향 전달) | `feat: 쿼리 릴레이 구현` |

#### Phase 3: 커넥션 풀링 (W4 + W5)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T3-1 | 커넥션 풀 자료구조 및 생성 로직 | `feat: 커넥션 풀 기본 구조 구현` |
| T3-2 | Acquire/Release (획득/반환) 구현 | `feat: 커넥션 획득/반환 로직` |
| T3-3 | 대기 큐 및 connection_timeout 구현 | `feat: 커넥션 대기 큐 및 타임아웃` |
| T3-4 | idle_timeout, max_lifetime 만료 처리 | `feat: 커넥션 만료 처리` |
| T3-5 | 헬스체크 고루틴 구현 | `feat: 커넥션 헬스체크` |

#### Phase 4: 쿼리 라우팅 (W6 + W7 + W8)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T4-1 | SQL 키워드 기반 R/W 분류 파서 | `feat: 쿼리 R/W 분류 파서` |
| T4-2 | 힌트 주석 파싱 (`/* route:writer */`) | `feat: 힌트 주석 기반 라우팅` |
| T4-3 | Writer/Reader 풀 분리 및 라우팅 로직 | `feat: Writer/Reader 라우팅` |
| T4-4 | 트랜잭션 세션 추적 (BEGIN~COMMIT) | `feat: 트랜잭션 세션 추적` |
| T4-5 | read_after_write_delay 구현 | `feat: 쓰기 후 읽기 지연 라우팅` |
| T4-6 | 라운드로빈 로드밸런서 구현 | `feat: Reader 라운드로빈 로드밸런싱` |
| T4-7 | Reader 장애 감지 및 자동 제외/복구 | `feat: Reader 장애 감지 및 자동 복구` |

#### Phase 5: 쿼리 캐싱 (W9 + W10)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T5-1 | LRU 캐시 자료구조 구현 | `feat: LRU 캐시 구현` |
| T5-2 | 캐시 키 해싱 (쿼리 + 파라미터) | `feat: 쿼리 캐시 키 생성` |
| T5-3 | TTL 만료 및 max_entries 제한 | `feat: 캐시 TTL 및 용량 제한` |
| T5-4 | 쓰기 쿼리에서 테이블명 추출 | `feat: 쿼리 테이블명 추출` |
| T5-5 | 테이블별 캐시 무효화 구현 | `feat: 테이블별 캐시 무효화` |

#### Phase 6: 테스트 & 마무리 (W11)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T6-1 | docker-compose로 PG Primary + Replica 구성 | `test: docker-compose 테스트 환경 구성` |
| T6-2 | 통합 테스트 작성 (R/W 분산, 캐시 동작) | `test: 통합 테스트 작성` |
| T6-3 | 벤치마크 및 부하 테스트 | `test: 벤치마크 및 부하 테스트` |

#### Phase 7: 고도화 (W13 + W14 + W15)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T7-1 | Prometheus 메트릭 인프라 (레지스트리, HTTP 엔드포인트) | `feat: Prometheus 메트릭 인프라 구현` |
| T7-2 | 풀/캐시/라우팅 메트릭 계측 | `feat: 풀/캐시/라우팅 메트릭 추가` |
| T7-3 | Parse 메시지 SQL 추출 | `feat: Extended Query Parse 메시지 SQL 추출` |
| T7-4 | Prepared Statement reader 라우팅 | `feat: Prepared Statement reader 라우팅` |
| T7-5 | Admin HTTP 서버 + Stats/Health 엔드포인트 | `feat: Admin API 서버 구현` |
| T7-6 | Cache flush 엔드포인트 | `feat: Admin cache flush API` |
| T7-7 | 고도화 테스트 및 블로그 포스팅 | `test: 고도화 기능 테스트 및 문서화` |

---

### 3. Task 상세 정의

#### T1-1: 프로젝트 초기 세팅
- **범위**: `go mod init`, 디렉토리 구조(`cmd/`, `internal/`), `.gitignore`, `Makefile`(build/test/lint), `golangci-lint` 설정
- **완료 기준**: `make build`로 빈 바이너리 빌드 성공
- **참고 파일**: `cmd/db-proxy/main.go` (빈 main 함수)

#### T1-2: YAML 설정 파싱 구현
- **범위**: `internal/config/config.go`에 설정 구조체 정의, `config.yaml` 로드
- **완료 기준**: `config.Load("config.yaml")` 호출 시 구조체에 값이 정상 바인딩, 단위 테스트 통과
- **참고 파일**: `docs/spec.md` 설정 예시 참조

#### T1-3: 설정 validation 추가
- **범위**: 필수값 누락 체크, 숫자 범위 검증 (min < max 등), 기본값 설정
- **완료 기준**: 잘못된 설정 파일로 로드 시 명확한 에러 메시지 반환

#### T2-1: TCP 리스너 구현
- **범위**: `internal/proxy/server.go`에 `net.Listen` + accept loop, graceful shutdown (signal 핸들링)
- **완료 기준**: `psql -h localhost -p 5432` 접속 시 TCP 연결 수립 확인 (프로토콜 처리 없이 연결만)

#### T2-2: PostgreSQL 핸드셰이크 구현
- **범위**: PG wire protocol의 StartupMessage 파싱, AuthenticationOk 응답, ReadyForQuery 전송
- **완료 기준**: `psql`로 접속 시 프록시가 백엔드 DB로 인증을 중계하여 정상 접속
- **참고**: [PG 프로토콜 문서](https://www.postgresql.org/docs/current/protocol-flow.html)

#### T2-3: 쿼리 릴레이 구현
- **범위**: Query 메시지를 백엔드로 전달, 응답을 클라이언트로 전달 (양방향 바이트 릴레이)
- **완료 기준**: 프록시를 통해 `SELECT 1`, `CREATE TABLE`, `INSERT` 등 정상 실행

#### T3-1: 커넥션 풀 기본 구조 구현
- **범위**: `internal/pool/pool.go`에 Pool 구조체, `NewPool()`, 설정값 적용
- **완료 기준**: Pool 생성 시 min_connections만큼 커넥션 사전 생성

#### T3-2: 커넥션 획득/반환 로직
- **범위**: `Acquire()` — idle에서 꺼내기 or 새 생성, `Release()` — idle로 반환
- **완료 기준**: 반복 Acquire/Release 시 커넥션 재사용 확인 (numOpen 증가 없음)

#### T3-3: 커넥션 대기 큐 및 타임아웃
- **범위**: maxOpen 도달 시 채널 대기, connection_timeout 초과 시 에러
- **완료 기준**: 동시 goroutine에서 maxOpen+1개 Acquire 시 마지막 하나가 타임아웃 에러

#### T3-4: 커넥션 만료 처리
- **범위**: Acquire 시 idle_timeout/max_lifetime 초과 커넥션 폐기 후 재생성
- **완료 기준**: idle_timeout=1s 설정 후 2초 대기 → Acquire 시 새 커넥션 생성 확인

#### T3-5: 커넥션 헬스체크
- **범위**: `internal/pool/health.go`에 주기적 ping 고루틴, 실패 시 교체
- **완료 기준**: DB 강제 종료 후 헬스체크가 비정상 커넥션 제거 확인

#### T4-1: 쿼리 R/W 분류 파서
- **범위**: `internal/router/parser.go`에 `Classify(query) QueryType`
- **완료 기준**: SELECT→Read, INSERT/UPDATE/DELETE→Write, SHOW/EXPLAIN→Read 단위 테스트

#### T4-2: 힌트 주석 기반 라우팅
- **범위**: `/* route:writer */`, `/* route:reader */` 주석 파싱
- **완료 기준**: `/* route:writer */ SELECT ...` → Write 반환 단위 테스트

#### T4-3: Writer/Reader 라우팅
- **범위**: `internal/router/router.go`에 Writer풀/Reader풀 분기 로직
- **완료 기준**: SELECT는 Reader 풀, INSERT는 Writer 풀에서 커넥션 획득 확인

#### T4-4: 트랜잭션 세션 추적
- **범위**: Session 구조체에 inTransaction 상태 관리, BEGIN~COMMIT 사이 Writer 고정
- **완료 기준**: BEGIN → SELECT → COMMIT 시퀀스에서 SELECT도 Writer로 라우팅

#### T4-5: 쓰기 후 읽기 지연 라우팅
- **범위**: lastWriteTime 기록, read_after_write_delay 이내 읽기 시 Writer로 전송
- **완료 기준**: INSERT 직후 SELECT가 Writer로 라우팅, delay 경과 후엔 Reader로 복귀

#### T4-6: Reader 라운드로빈 로드밸런싱
- **범위**: `internal/router/balancer.go`에 RoundRobin 구현
- **완료 기준**: Reader 2대 설정 시 SELECT가 교대로 분산되는지 확인

#### T4-7: Reader 장애 감지 및 자동 복구
- **범위**: Reader ping 실패 시 healthy=false, 주기적 재검사로 복구
- **완료 기준**: Reader 1대 중단 → 나머지로 분산, 재시작 → 다시 포함

#### T5-1: LRU 캐시 구현
- **범위**: `internal/cache/cache.go`에 Cache 구조체, Get/Set/Remove
- **완료 기준**: max_entries 초과 시 가장 오래된 항목 제거, 최근 접근 항목 유지

#### T5-2: 쿼리 캐시 키 생성
- **범위**: xxhash로 쿼리 텍스트 + 파라미터 해싱
- **완료 기준**: 동일 쿼리+파라미터 → 동일 키, 파라미터 다르면 다른 키

#### T5-3: 캐시 TTL 및 용량 제한
- **범위**: TTL 만료 체크, max_result_size 초과 시 캐싱 스킵
- **완료 기준**: TTL 경과 후 Get → miss, 큰 결과 Set 시 저장 안 됨

#### T5-4: 쿼리 테이블명 추출
- **범위**: `INSERT INTO users` → `["users"]`, `UPDATE orders SET` → `["orders"]`
- **완료 기준**: 다양한 쓰기 쿼리 패턴에서 테이블명 정확 추출 단위 테스트

#### T5-5: 테이블별 캐시 무효화
- **범위**: `internal/cache/invalidator.go`에 테이블→캐시 역인덱스, OnWrite 시 삭제
- **완료 기준**: users 테이블 SELECT 캐싱 → INSERT INTO users → 해당 캐시 삭제 확인

#### T6-1: docker-compose 테스트 환경 구성
- **범위**: PG Primary 1대 + Replica 2대, 프록시 컨테이너, 네트워크 구성
- **완료 기준**: `docker-compose up`으로 전체 환경 구동, replication 동작 확인

#### T6-2: 통합 테스트 작성
- **범위**: Go test로 프록시 경유 R/W 분산 검증, 캐시 히트/미스 검증
- **완료 기준**: `make test-integration` 통과

#### T6-3: 벤치마크 및 부하 테스트
- **범위**: Go benchmark, pgbench를 통한 직접연결 대비 프록시 경유 성능 비교
- **완료 기준**: 벤치마크 결과 문서화, 직접연결 대비 오버헤드 10% 이내

#### T7-1: Prometheus 메트릭 인프라 구현
- **범위**: `internal/metrics/metrics.go`에 Metrics 구조체, Prometheus 레지스트리 등록, `/metrics` HTTP 엔드포인트
- **완료 기준**: `curl localhost:9090/metrics` 응답 확인, `dbproxy_` 접두사 메트릭 노출
- **의존성**: `prometheus/client_golang` 패키지 추가

#### T7-2: 풀/캐시/라우팅 메트릭 추가
- **범위**: server.go에서 쿼리 처리 시 메트릭 기록 — 라우팅 카운터, 쿼리 레이턴시, 캐시 hit/miss, 풀 상태
- **완료 기준**: 프록시 경유 쿼리 실행 후 `/metrics`에서 카운터 증가 확인
- **참고 메트릭**: `dbproxy_queries_routed_total`, `dbproxy_query_duration_seconds`, `dbproxy_cache_hits_total`, `dbproxy_pool_connections_open`

#### T7-3: Parse 메시지 SQL 추출
- **범위**: `internal/protocol/message.go`에 `ExtractParseQuery(payload) (stmtName, query string)` 함수
- **완료 기준**: Parse 메시지 바이트에서 statement name과 SQL 텍스트 정확 추출 단위 테스트
- **참고**: PG 프로토콜 Parse 메시지 포맷 — string(stmt) + string(query) + int16(params) + int32[](oids)

#### T7-4: Prepared Statement reader 라우팅
- **범위**: server.go에 `extendedQueryState` 추가, Parse 시 route 결정 → Bind/Execute는 같은 대상으로 전달
- **완료 기준**: `SELECT` Prepared Statement가 reader로 라우팅되는 E2E 테스트 통과
- **제한**: 트랜잭션 내부는 기존처럼 전체 writer

#### T7-5: Admin HTTP 서버 + Stats/Health 엔드포인트
- **범위**: `internal/admin/admin.go`에 Handler, `GET /admin/stats`, `GET /admin/health`, `GET /admin/config`
- **완료 기준**: JSON 형식의 풀/캐시/헬스 정보 반환, 비밀번호 마스킹 확인

#### T7-6: Admin cache flush API
- **범위**: `POST /admin/cache/flush` (전체), `POST /admin/cache/flush/{table}` (테이블별)
- **완료 기준**: flush 후 캐시 entries가 0(전체) 또는 해당 테이블 캐시만 삭제 확인
- **참고**: Cache에 `Flush()` 메서드 추가 필요

#### T7-7: 고도화 테스트 및 블로그 포스팅
- **범위**: Docker 환경 E2E 테스트 보강, 블로그 6편 작성 (고도화 내용)
- **완료 기준**: 전체 테스트 통과, 블로그 포스트 완성

---

### 4. 작업 도구: Claude Code Agent Teams

모든 코딩 작업은 **Claude Code Agent Teams**(멀티 에이전트 오케스트레이션)를 활용한다.

#### Agent Teams란?

여러 Claude Code 에이전트가 팀으로 협업하는 실험적 기능이다.

- **Team Lead**: 작업을 조율하고 Task를 분배하는 리더 에이전트
- **Teammate**: 각자 독립적인 컨텍스트에서 병렬로 작업하는 에이전트
- Teammate끼리 직접 소통 가능 (일반 서브에이전트와 차별점)
- 공유 Task 리스트로 의존성 자동 관리

#### 활성화

`settings.json`에 아래 설정 추가:

```json
{
  "env": {
    "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1"
  }
}
```

또는 환경 변수: `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`

#### Phase별 Agent Teams 활용 전략

| Phase | Team 구성 | 역할 분담 |
|-------|-----------|-----------|
| Phase 1 (기반) | Lead + 1 Teammate | Lead: 모듈 초기화 / Teammate: 설정 파싱+validation |
| Phase 2 (TCP) | Lead + 1 Teammate | Lead: TCP 리스너+릴레이 / Teammate: PG 프로토콜 핸드셰이크 |
| Phase 3 (풀링) | Lead + 2 Teammates | Lead: 풀 구조+획득반환 / T1: 대기큐+만료처리 / T2: 헬스체크 |
| Phase 4 (라우팅) | Lead + 3 Teammates | Lead: 라우팅 코어 / T1: 파서+힌트 / T2: 트랜잭션+delay / T3: 로드밸런서+장애감지 |
| Phase 5 (캐싱) | Lead + 2 Teammates | Lead: LRU 캐시 / T1: 키해싱+TTL / T2: 테이블추출+무효화 |
| Phase 6 (테스트) | Lead + 2 Teammates | Lead: docker-compose / T1: 통합테스트 / T2: 벤치마크 |

#### 프롬프트 예시

```
# Phase 3 작업 시작
"Agent team을 만들어서 커넥션 풀링을 구현해줘.

Lead: internal/pool/pool.go에 Pool 구조체와 Acquire/Release 구현
Teammate 1: 대기 큐(connection_timeout)와 만료 처리(idle_timeout, max_lifetime) 구현
Teammate 2: internal/pool/health.go에 헬스체크 고루틴 구현

docs/implementation.md의 커넥션 풀링 섹션 참고.
각자 단위 테스트도 같이 작성해줘."

# 코드 리뷰 시
"Agent team으로 이 PR을 리뷰해줘.
- Reviewer 1: 동시성 이슈 (race condition, deadlock)
- Reviewer 2: 성능 (불필요한 할당, 락 경합)
- Reviewer 3: 테스트 커버리지"
```

#### 주의 사항

- **토큰 비용**: 팀 규모에 비례하여 비용 증가 — 단순 작업은 단일 에이전트로 충분
- **실험적 기능**: 세션 재개, 종료 처리에 제한이 있을 수 있음
- **디스플레이 모드**: 기본은 in-process (단일 터미널), tmux/iTerm2에서는 split-pane 모드 사용 가능

---

### 5. Git 워크플로우

모든 Task는 아래 순서로 진행한다.

```
Issue 등록 → 브랜치 생성 → 작업 → PR 생성 → 리뷰 → PR 머지
```

#### 상세 흐름

```
1) GitHub Issue 등록
   - 제목: Task 테이블의 "예상 이슈 제목" 사용
   - 본문: Task 상세 정의의 범위/완료 기준 복사
   - 라벨: phase별 라벨 (phase-1, phase-2, ...)
   - 예시:
     Title: feat: 커넥션 풀 기본 구조 구현
     Body:
       ## 범위
       - internal/pool/pool.go에 Pool 구조체, NewPool(), 설정값 적용
       ## 완료 기준
       - Pool 생성 시 min_connections만큼 커넥션 사전 생성

2) 브랜치 생성
   - 네이밍: feat/{issue번호}-{간단한설명}
   - 예시: feat/3-connection-pool-struct

3) 작업 (Claude Code 활용)
   - 커밋 메시지: conventional commits 형식
   - 예시: "feat(pool): add Pool struct and NewPool constructor (#3)"

4) PR 생성
   - 제목: 이슈 제목과 동일
   - 본문:
     - ## 변경 사항 (무엇을 했는지)
     - ## 테스트 (어떻게 검증했는지)
     - closes #{이슈번호}
   - Claude Code로 PR 리뷰 코멘트 생성

5) PR 머지
   - Squash merge 사용 (커밋 히스토리 깔끔하게)
   - 머지 후 브랜치 삭제
```

#### 브랜치 전략

```
main
 ├── feat/1-project-setup
 ├── feat/2-config-parsing
 ├── feat/3-connection-pool-struct
 ├── ...
 └── feat/21-benchmark
```

---

### 6. 블로그 포스팅 계획

개발이 어느정도 진행되면 블로그에 정리한다.

- **블로그 루트**: `/Users/nhn/Library/Mobile Documents/com~apple~CloudDocs/project/study-blog/`
- **형식**: Hugo (기존 블로그와 동일한 frontmatter 형식)

#### 6-1. 프로젝트 페이지 등록

먼저 `content/projects/`에 프로젝트 페이지를 등록한다. (simple-queue-service와 동일한 형식)

- **경로**: `content/projects/db-proxy.md`

```markdown
---
title: "DB Proxy"
date: YYYY-MM-DD
draft: false
description: "PostgreSQL 프록시 직접 구현하기 - 커넥션 풀링, R/W 분산, 쿼리 캐싱"
icon: "🗄️"
status: "진행중"
tech: ["Go", "PostgreSQL", "Wire Protocol"]
links:
  - name: "GitHub Repository"
    url: "https://github.com/{user}/db-proxy"
    icon: "🔗"
  - name: "개발 블로그 시리즈"
    url: "/posts?project=db-proxy"
    icon: "📝"
duration: "YYYY.MM.DD ~ 진행중"
---

# DB Proxy 프로젝트

> "내 손으로 만드는 데이터베이스 프록시"

애플리케이션과 DB 사이에 위치하여 커넥션 풀링, 읽기/쓰기 자동 분산,
반복 쿼리 캐싱을 수행하는 프록시를 직접 구현하는 프로젝트입니다.

## 🎯 프로젝트 배경 및 목표

**🔍 기술적 호기심**
- PostgreSQL wire protocol을 직접 다뤄보고 싶다
- 커넥션 풀링, 쿼리 라우팅이 내부적으로 어떻게 동작하는지 이해하고 싶다

**📚 학습 목표**
- Go 동시성 프로그래밍 (goroutine, channel, sync)
- PostgreSQL wire protocol 이해
- 커넥션 풀링 설계
- LRU 캐시 구현

## 🚀 핵심 구현 기능
- 커넥션 풀링 (min/max, idle timeout, health check)
- R/W 쿼리 자동 분산 (Writer/Reader 라우팅, 트랜잭션 추적)
- 반복 쿼리 캐싱 (LRU, TTL, 테이블별 무효화)

## 🛠️ 기술 스택
- Go, PostgreSQL Wire Protocol, YAML, xxhash

## 📝 개발 블로그 시리즈
프로젝트 개발 과정에서 배운 내용을 기록하고 있습니다.

## 🔗 관련 링크
- **GitHub Repository**: [프로젝트 소스코드](https://github.com/{user}/db-proxy)
- **개발 블로그**: [개발 블로그 시리즈](/posts?project=db-proxy)
```

#### 6-2. 시리즈 포스트

Phase 완료 시점마다 `content/posts/`에 시리즈 포스트를 작성한다.

- **경로**: `content/posts/YYYY-MM-DD-db-proxy-N-제목.md`

| # | 시점 | 제목 (안) | 주요 내용 |
|---|------|-----------|-----------|
| P1 | Phase 2 완료 후 | "Go로 PostgreSQL 프록시 만들기 (1) - PG Wire Protocol 이해" | PG 프로토콜 구조, 핸드셰이크 과정, 바이트 레벨 분석, 구현 시 삽질 |
| P2 | Phase 3 완료 후 | "Go로 PostgreSQL 프록시 만들기 (2) - 커넥션 풀링 직접 구현" | 풀링이 왜 필요한지, 자료구조 설계, 동시성 처리(mutex vs channel), 벤치마크 |
| P3 | Phase 4 완료 후 | "Go로 PostgreSQL 프록시 만들기 (3) - 읽기/쓰기 자동 분산" | 쿼리 파싱 전략, 트랜잭션 추적, replication lag 대응, 장애 감지 |
| P4 | Phase 5 완료 후 | "Go로 PostgreSQL 프록시 만들기 (4) - 쿼리 캐싱과 무효화" | LRU 구현, 캐시 키 설계, 테이블별 무효화 전략, 캐시 히트율 분석 |
| P5 | Phase 6 완료 후 | "Go로 PostgreSQL 프록시 만들기 (5) - 통합, E2E 테스트, 회고" | 컴포넌트 통합, SCRAM-SHA-256, E2E 테스트, 벤치마크 |
| P6 | Phase 7 완료 후 | "Go로 PostgreSQL 프록시 만들기 (6) - 메트릭, Admin API, Prepared Statement" | Prometheus 메트릭, Admin API, Extended Query 라우팅 |

#### 포스트 템플릿

```markdown
---
title: "Go로 PostgreSQL 프록시 만들기 (N) - 제목"
date: YYYY-MM-DD
draft: false
tags: ["Go", "PostgreSQL", "Database", "Proxy", "해당주제태그"]
categories: ["Database"]
description: "한줄 설명"
---

## 들어가며
> 왜 이걸 만들게 되었는지 / 이번 글에서 다룰 내용

## 본문
(구현 과정, 코드, 설명)

## 결과
(동작 확인, 벤치마크 등)

## 마무리
(배운 점, 다음 글 예고)
```
