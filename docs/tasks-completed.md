## 완료된 Task (Phase 1-16)

모든 Task 완료됨.

---

### Phase 1: 프로젝트 기반 (W1 + W2)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T1-1 | Go 모듈 초기화, .gitignore, Makefile 생성 | `chore: 프로젝트 초기 세팅` |
| T1-2 | 설정 구조체 정의 및 YAML 파싱 구현 | `feat: YAML 설정 파싱 구현` |
| T1-3 | 설정 validation (필수값 체크, 범위 검증) | `feat: 설정 validation 추가` |

### Phase 2: TCP 프록시 (W3)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T2-1 | TCP 리스너 및 클라이언트 접속 수락 | `feat: TCP 리스너 구현` |
| T2-2 | PG wire protocol 핸드셰이크 (Startup, Auth) | `feat: PostgreSQL 핸드셰이크 구현` |
| T2-3 | 쿼리 릴레이 (클라이언트 ↔ DB 양방향 전달) | `feat: 쿼리 릴레이 구현` |

### Phase 3: 커넥션 풀링 (W4 + W5)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T3-1 | 커넥션 풀 자료구조 및 생성 로직 | `feat: 커넥션 풀 기본 구조 구현` |
| T3-2 | Acquire/Release (획득/반환) 구현 | `feat: 커넥션 획득/반환 로직` |
| T3-3 | 대기 큐 및 connection_timeout 구현 | `feat: 커넥션 대기 큐 및 타임아웃` |
| T3-4 | idle_timeout, max_lifetime 만료 처리 | `feat: 커넥션 만료 처리` |
| T3-5 | 헬스체크 고루틴 구현 | `feat: 커넥션 헬스체크` |

### Phase 4: 쿼리 라우팅 (W6 + W7 + W8)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T4-1 | SQL 키워드 기반 R/W 분류 파서 | `feat: 쿼리 R/W 분류 파서` |
| T4-2 | 힌트 주석 파싱 (`/* route:writer */`) | `feat: 힌트 주석 기반 라우팅` |
| T4-3 | Writer/Reader 풀 분리 및 라우팅 로직 | `feat: Writer/Reader 라우팅` |
| T4-4 | 트랜잭션 세션 추적 (BEGIN~COMMIT) | `feat: 트랜잭션 세션 추적` |
| T4-5 | read_after_write_delay 구현 | `feat: 쓰기 후 읽기 지연 라우팅` |
| T4-6 | 라운드로빈 로드밸런서 구현 | `feat: Reader 라운드로빈 로드밸런싱` |
| T4-7 | Reader 장애 감지 및 자동 제외/복구 | `feat: Reader 장애 감지 및 자동 복구` |

### Phase 5: 쿼리 캐싱 (W9 + W10)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T5-1 | LRU 캐시 자료구조 구현 | `feat: LRU 캐시 구현` |
| T5-2 | 캐시 키 해싱 (쿼리 + 파라미터) | `feat: 쿼리 캐시 키 생성` |
| T5-3 | TTL 만료 및 max_entries 제한 | `feat: 캐시 TTL 및 용량 제한` |
| T5-4 | 쓰기 쿼리에서 테이블명 추출 | `feat: 쿼리 테이블명 추출` |
| T5-5 | 테이블별 캐시 무효화 구현 | `feat: 테이블별 캐시 무효화` |

### Phase 6: 테스트 & 마무리 (W11)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T6-1 | docker-compose로 PG Primary + Replica 구성 | `test: docker-compose 테스트 환경 구성` |
| T6-2 | 통합 테스트 작성 (R/W 분산, 캐시 동작) | `test: 통합 테스트 작성` |
| T6-3 | 벤치마크 및 부하 테스트 | `test: 벤치마크 및 부하 테스트` |

### Phase 7: 고도화 1차 (W13 + W14 + W15)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T7-1 | Prometheus 메트릭 인프라 (레지스트리, HTTP 엔드포인트) | `feat: Prometheus 메트릭 인프라 구현` |
| T7-2 | 풀/캐시/라우팅 메트릭 계측 | `feat: 풀/캐시/라우팅 메트릭 추가` |
| T7-3 | Parse 메시지 SQL 추출 | `feat: Extended Query Parse 메시지 SQL 추출` |
| T7-4 | Prepared Statement reader 라우팅 | `feat: Prepared Statement reader 라우팅` |
| T7-5 | Admin HTTP 서버 + Stats/Health 엔드포인트 | `feat: Admin API 서버 구현` |
| T7-6 | Cache flush 엔드포인트 | `feat: Admin cache flush API` |
| T7-7 | 고도화 테스트 및 블로그 포스팅 | `test: 고도화 기능 테스트 및 문서화` |

---

### Task 상세 정의

<details>
<summary>Phase 1 상세</summary>

#### T1-1: 프로젝트 초기 세팅
- **범위**: `go mod init`, 디렉토리 구조(`cmd/`, `internal/`), `.gitignore`, `Makefile`(build/test/lint), `golangci-lint` 설정
- **완료 기준**: `make build`로 빈 바이너리 빌드 성공

#### T1-2: YAML 설정 파싱 구현
- **범위**: `internal/config/config.go`에 설정 구조체 정의, `config.yaml` 로드
- **완료 기준**: `config.Load("config.yaml")` 호출 시 구조체에 값이 정상 바인딩, 단위 테스트 통과

#### T1-3: 설정 validation 추가
- **범위**: 필수값 누락 체크, 숫자 범위 검증 (min < max 등), 기본값 설정
- **완료 기준**: 잘못된 설정 파일로 로드 시 명확한 에러 메시지 반환

</details>

<details>
<summary>Phase 2 상세</summary>

#### T2-1: TCP 리스너 구현
- **범위**: `internal/proxy/server.go`에 `net.Listen` + accept loop, graceful shutdown (signal 핸들링)
- **완료 기준**: `psql -h localhost -p 5432` 접속 시 TCP 연결 수립 확인

#### T2-2: PostgreSQL 핸드셰이크 구현
- **범위**: PG wire protocol의 StartupMessage 파싱, AuthenticationOk 응답, ReadyForQuery 전송
- **완료 기준**: `psql`로 접속 시 프록시가 백엔드 DB로 인증을 중계하여 정상 접속

#### T2-3: 쿼리 릴레이 구현
- **범위**: Query 메시지를 백엔드로 전달, 응답을 클라이언트로 전달 (양방향 바이트 릴레이)
- **완료 기준**: 프록시를 통해 `SELECT 1`, `CREATE TABLE`, `INSERT` 등 정상 실행

</details>

<details>
<summary>Phase 3 상세</summary>

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

</details>

<details>
<summary>Phase 4 상세</summary>

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

</details>

<details>
<summary>Phase 5 상세</summary>

#### T5-1: LRU 캐시 구현
- **범위**: `internal/cache/cache.go`에 Cache 구조체, Get/Set/Remove
- **완료 기준**: max_entries 초과 시 가장 오래된 항목 제거, 최근 접근 항목 유지

#### T5-2: 쿼리 캐시 키 생성
- **범위**: FNV-1a로 쿼리 텍스트 + 파라미터 해싱
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

</details>

<details>
<summary>Phase 6 상세</summary>

#### T6-1: docker-compose 테스트 환경 구성
- **범위**: PG Primary 1대 + Replica 2대, 프록시 컨테이너, 네트워크 구성
- **완료 기준**: `docker-compose up`으로 전체 환경 구동, replication 동작 확인

#### T6-2: 통합 테스트 작성
- **범위**: Go test로 프록시 경유 R/W 분산 검증, 캐시 히트/미스 검증
- **완료 기준**: `make test-integration` 통과

#### T6-3: 벤치마크 및 부하 테스트
- **범위**: Go benchmark, pgbench를 통한 직접연결 대비 프록시 경유 성능 비교
- **완료 기준**: 벤치마크 결과 문서화, 직접연결 대비 오버헤드 10% 이내

</details>

<details>
<summary>Phase 7 상세</summary>

#### T7-1: Prometheus 메트릭 인프라 구현
- **범위**: `internal/metrics/metrics.go`에 Metrics 구조체, Prometheus 레지스트리 등록, `/metrics` HTTP 엔드포인트
- **완료 기준**: `curl localhost:9090/metrics` 응답 확인, `pgmux_` 접두사 메트릭 노출

#### T7-2: 풀/캐시/라우팅 메트릭 추가
- **범위**: server.go에서 쿼리 처리 시 메트릭 기록 — 라우팅 카운터, 쿼리 레이턴시, 캐시 hit/miss, 풀 상태
- **완료 기준**: 프록시 경유 쿼리 실행 후 `/metrics`에서 카운터 증가 확인

#### T7-3: Parse 메시지 SQL 추출
- **범위**: `internal/protocol/message.go`에 `ParseParseMessage(payload) (stmtName, query string)` 함수
- **완료 기준**: Parse 메시지 바이트에서 statement name과 SQL 텍스트 정확 추출 단위 테스트

#### T7-4: Prepared Statement reader 라우팅
- **범위**: server.go에서 Parse 시 route 결정 → Bind/Execute는 같은 대상으로 전달
- **완료 기준**: `SELECT` Prepared Statement가 reader로 라우팅되는 E2E 테스트 통과

#### T7-5: Admin HTTP 서버 + Stats/Health 엔드포인트
- **범위**: `internal/admin/admin.go`에 Handler, `GET /admin/stats`, `GET /admin/health`, `GET /admin/config`
- **완료 기준**: JSON 형식의 풀/캐시/헬스 정보 반환, 비밀번호 마스킹 확인

#### T7-6: Admin cache flush API
- **범위**: `POST /admin/cache/flush` (전체), `POST /admin/cache/flush/{table}` (테이블별)
- **완료 기준**: flush 후 캐시 entries가 0(전체) 또는 해당 테이블 캐시만 삭제 확인

#### T7-7: 고도화 테스트 및 블로그 포스팅
- **범위**: Docker 환경 E2E 테스트 보강, 블로그 6편 작성 (고도화 내용)
- **완료 기준**: 전체 테스트 통과, 블로그 포스트 완성

</details>

### Phase 8: Transaction Pooling (W16)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T8-1 | Writer 커넥션 풀 도입 | `feat(proxy): Transaction Pooling - Writer 커넥션 다중화` |
| T8-2 | 트랜잭션 레벨 커넥션 바인딩 | (동일 이슈) |
| T8-3 | 세션 상태 리셋 (DISCARD ALL) | (동일 이슈) |
| T8-4 | Simple Query 트랜잭션 풀링 통합 | (동일 이슈) |
| T8-5 | Extended Query 트랜잭션 풀링 통합 | (동일 이슈) |
| T8-6 | Transaction Pooling E2E 테스트 | (동일 이슈) |

<details>
<summary>Phase 8 상세</summary>

#### T8-1: Writer 커넥션 풀 도입
- **범위**: `NewServer()`에서 Writer도 `pool.Pool`로 생성. 인증 흐름 분리 — 임시 커넥션으로 클라이언트 인증, 풀 커넥션은 `pgConnect()`로 사전 인증
- **완료 기준**: Writer 커넥션이 풀에서 관리되고, 여러 클라이언트가 백엔드 커넥션을 공유

#### T8-2: 트랜잭션 레벨 커넥션 바인딩
- **범위**: BEGIN 시 풀에서 Acquire → 트랜잭션 동안 고정 → COMMIT/ROLLBACK 시 Release
- **완료 기준**: 트랜잭션 중에는 동일 백엔드 커넥션 사용, 트랜잭션 종료 후 반환 확인

#### T8-3: 세션 상태 리셋
- **범위**: 커넥션 반환 시 `DISCARD ALL` 전송, `reset_query` 설정 추가
- **완료 기준**: 클라이언트 A가 SET 후 반환 → 클라이언트 B가 같은 커넥션 획득 시 기본값

#### T8-4: Simple Query 트랜잭션 풀링 통합
- **범위**: 비트랜잭션 단일 쿼리 시 Writer 풀에서 빌려 사용 후 즉시 반환
- **완료 기준**: 동시 다수 클라이언트 INSERT 시 백엔드 커넥션이 max_connections 이내

#### T8-5: Extended Query 트랜잭션 풀링 통합
- **범위**: Parse~Sync 배치 단위로 Writer 풀에서 커넥션 획득/반환
- **완료 기준**: Prepared Statement 기반 쓰기도 풀링 동작 확인

#### T8-6: Transaction Pooling E2E 테스트
- **범위**: 동시 20 클라이언트 / 10 커넥션 풀 공유, 트랜잭션 격리, 세션 리셋, 풀 고갈 복구 검증
- **완료 기준**: 전체 E2E 시나리오 통과

</details>

### Phase 9: SSL/TLS Termination + Front-end Auth (W17)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T9-1 | TLS 설정 구조체 추가 | `feat(proxy): TLS Termination 구현` |
| T9-2 | TLS Listener 구현 | (동일 이슈) |
| T9-3 | Front-end Auth 설정 | `feat(proxy): 프록시 자체 인증 (Front-end Auth)` |
| T9-4 | Front-end Auth 구현 | (동일 이슈) |
| T9-5 | TLS + Auth E2E 테스트 | (동일 이슈) |

<details>
<summary>Phase 9 상세</summary>

#### T9-1: TLS 설정 구조체 추가
- **범위**: `config.go`에 `TLSConfig` 추가 — `enabled`, `cert_file`, `key_file`
- **완료 기준**: TLS 설정이 YAML에서 파싱되고 validation 통과

#### T9-2: TLS Listener 구현
- **범위**: SSL 요청 시 `'S'` 응답 후 `tls.Server()`로 업그레이드. TLS 미설정 시 기존 `'N'` 응답 유지
- **완료 기준**: `sslmode=require`로 TLS 접속 성공, `sslmode=disable`도 접속 가능

#### T9-3: Front-end Auth 설정
- **범위**: `config.go`에 `AuthConfig` 추가 — `enabled`, `users` 리스트 (username/password 쌍)
- **완료 기준**: 인증 설정 파싱 및 validation

#### T9-4: Front-end Auth 구현
- **범위**: 클라이언트 StartupMessage의 user 확인 → 설정 users와 대조 → 실패 시 ErrorResponse (백엔드 접속 없이 차단). SCRAM-SHA-256 인증 지원
- **완료 기준**: 설정에 없는 유저 → 프록시에서 즉시 거부, 올바른 유저 → 정상 쿼리

#### T9-5: TLS + Auth E2E 테스트
- **범위**: 자체 서명 인증서 TLS 접속, 올바른/잘못된 유저 인증 시나리오
- **완료 기준**: TLS + Auth 전체 시나리오 통과

</details>

### Phase 10: Circuit Breaker & Rate Limiting (W18)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T10-1 | Circuit Breaker 상태 머신 구현 | `feat(resilience): Circuit Breaker 구현` |
| T10-2 | Circuit Breaker 풀 통합 | (동일 이슈) |
| T10-3 | Token Bucket Rate Limiter 구현 | `feat(resilience): Token Bucket Rate Limiter 구현` |
| T10-4 | Rate Limiter 프록시 통합 | (동일 이슈) |
| T10-5 | Circuit Breaker & Rate Limiter 테스트 | (동일 이슈) |

<details>
<summary>Phase 10 상세</summary>

#### T10-1: Circuit Breaker 상태 머신
- **범위**: `internal/resilience/breaker.go`에 구현. 상태: Closed → Open → Half-Open
- **완료 기준**: 에러율 초과 → Open → 일정 시간 후 Half-Open → 성공 시 Closed 복귀

#### T10-2: Circuit Breaker 풀 통합
- **범위**: Writer/Reader 풀에 Circuit Breaker 적용. Open 시 즉시 에러 반환
- **완료 기준**: 백엔드 장애 시 불필요한 연결 시도 차단

#### T10-3: Token Bucket Rate Limiter
- **범위**: `internal/resilience/ratelimit.go`에 구현. 설정: `rate`, `burst`
- **완료 기준**: 초당 제한 초과 시 즉시 거부

#### T10-4: Rate Limiter 프록시 통합
- **범위**: `relayQueries()`에서 쿼리 처리 전 체크. 초과 시 PG ErrorResponse 반환. 메트릭: `pgmux_rate_limited_total`
- **완료 기준**: 특정 클라이언트가 과도한 쿼리 시 프록시에서 거부

#### T10-5: Circuit Breaker & Rate Limiter 테스트
- **범위**: 단위 테스트 + E2E 시나리오
- **완료 기준**: CB + Rate Limiter 동시 동작 확인

</details>

### Phase 11: Zero-Downtime Reload (W19)

| Task | 작업 | 이슈 제목 |
|------|------|-----------|
| T11-1 | SIGHUP 핸들링 + 설정 리로드 | `feat(config): SIGHUP 무중단 설정 리로드 + Admin /reload 엔드포인트` |
| T11-2 | Admin API /admin/reload 엔드포인트 | (동일 이슈) |
| T11-3 | Reader Pool Hot Swap | (동일 이슈) |
| T11-4 | Graceful Reload E2E 테스트 | (동일 이슈) |

<details>
<summary>Phase 11 상세</summary>

#### T11-1: SIGHUP 핸들링 + 설정 리로드
- **범위**: SIGHUP 시그널 수신 → `config.Load()` 재호출 → Server에 새 설정 전달
- **리로드 가능**: Reader 목록, 풀 크기, 캐시 TTL, Rate Limit
- **리로드 불가**: `proxy.listen`, Writer 주소
- **완료 기준**: `kill -HUP <pid>` → "config reloaded" 로그, 새 설정 적용

#### T11-2: Admin API /admin/reload 엔드포인트
- **범위**: `POST /admin/reload` — SIGHUP과 동일한 리로드 트리거
- **완료 기준**: `curl -X POST localhost:9091/admin/reload` → 성공 JSON 응답

#### T11-3: Reader Pool Hot Swap
- **범위**: 새 Reader 목록으로 새 풀 생성 → Balancer atomic swap → 기존 풀 drain
- **완료 기준**: Reader 추가/제거 시 기존 진행 중인 쿼리에 영향 없이 새 Reader로 전환

#### T11-4: Graceful Reload E2E 테스트
- **범위**: 쿼리 실행 중 SIGHUP → 기존 쿼리 정상 완료 + 새 설정 적용 확인
- **완료 기준**: 장시간 쿼리 실행 중 리로드 → 쿼리 정상 완료

</details>

### Phase 12: LSN 기반 Causal Consistency

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T12-1 | LSN 타입 및 비교 유틸리티 | #52 / #53 |
| T12-2 | Writer LSN 트래킹 | #54 / #55 |
| T12-3 | Reader LSN 폴링 및 LSN-Aware 밸런서 | #56 / #57 |
| T12-4 | Causal Consistency E2E 테스트 | #58 / #59 |

### Phase 13: AST 기반 쿼리 파서 + 쿼리 방화벽

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T13-1 | pg_query_go AST 파서 도입 | #60 / #61 |
| T13-2 | Classify/ExtractTables AST 전환 | #62 / #63 |
| T13-3 | 쿼리 방화벽 (Query Firewall) | #64 / #65 |
| T13-4 | Semantic Cache Key (AST 정규화) | #66 / #67 |
| T13-5 | AST 파서 테스트 및 벤치마크 | #68 / #69 |

### Phase 13.5: 보안 QA & 핫픽스

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | 캐시 충돌 수정 (Fingerprint → Parse+Deparse) | #70 / #71 |
| - | 무한 재귀 수정 (cacheKey self-call) | #70 / #71 |
| - | CTE 방화벽 우회 수정 | #72 / #75 |
| - | Dollar Quoting 힌트 주입 수정 | #72 / #75 |

### Phase 14: Audit Logging & Slow Query Tracker (W23)

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T14-1~5 | Audit 설정 + Logger + Slow Query + Webhook + 메트릭 | #78 / #79 |

### Phase 15: Helm Chart (W24)

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T15-1~4 | Multi-stage Dockerfile + Helm Chart + 문서화 | #80 / #81 |

### Phase 16: Serverless Data API (W25)

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T16-1~5 | Data API HTTP 서버 + PG→JSON 변환 + 인증 + 기능 통합 + 테스트 | #82 / #83 |

### Hotfix: Audit Channel Blocking & Connection Poisoning

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | Audit Webhook 동기 호출 → 비동기 고루틴 분리 (Channel Blocking 방지) | #86 / #88 |
| - | drainUntilReady 에러 반환 추가 → 죽은 커넥션 Discard 처리 (Connection Poisoning 방지) | #87 / #89 |

### Phase 17: OpenTelemetry 분산 추적

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T17-1 | OTel 의존성 및 TelemetryConfig 설정 구조체 추가 | #95 / #98 |
| T17-2 | TracerProvider + Exporter 초기화 (internal/telemetry/telemetry.go) | #95 / #98 |
| T17-3 | Simple Query 경로 Span 계측 (pgmux.query → parse → cache → pool → exec) | #95 / #98 |
| T17-4 | Extended Query 경로 Span 계측 (pgmux.extended_query) | #95 / #98 |
| T17-5 | Data API traceparent HTTP 전파 | #95 / #98 |
| T17-7 | README/CLAUDE.md telemetry 문서화 | #95 / #98 |

### Phase 18: Config File Watch (fsnotify)

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T18-1 | fsnotify 의존성 및 ConfigOptionsConfig.Watch 설정 추가 | #94 / #96 |
| T18-2 | FileWatcher 구현 (부모 디렉토리 감시, 디바운싱, symlink swap 지원) | #94 / #96 |
| T18-3 | main.go 통합 (기존 reloadConfig() 재사용) | #94 / #96 |
| T18-4 | 단위 테스트 4건 (수정 감지, 디바운싱, symlink swap, Stop 안전성) | #94 / #96 |

### Phase 19: Prepared Statement Multiplexing PoC

| Task | 작업 | 이슈/PR |
|------|------|---------|
| T19-1 | Bind 메시지 파서 확장 (ParseBindMessageFull: 파라미터, 포맷코드, 결과포맷코드 추출) | #105 / #106 |
| T19-2 | PG 타입별 SQL 리터럴 직렬화 (literal.go: 20+ 타입 이스케이핑, NULL byte 방어) | #105 / #106 |
| T19-3 | Query Synthesizer 구현 (Parse+Bind → Simple Query 합성, 문자열 리터럴 내 플레이스홀더 보호) | #105 / #106 |
| T19-4 | Describe 메시지 프록시 처리 (임시 Parse→Describe→Close 릴레이) | #105 / #106 |
| T19-5 | Multiplexing 모드 설정 및 server.go 통합 (prepared_statement_mode: multiplex) | #105 / #106 |
| T19-6 | SQL Injection 방어 테스트 매트릭스 (DROP TABLE, 플레이스홀더, NULL byte, 중첩 이스케이핑 등) | #105 / #106 |

### Hotfix: COPY Protocol Deadlock

| Task | 작업 | 이슈/PR |
|------|------|---------|
| HF-1 | relayUntilReady/relayAndCollect COPY 프로토콜 처리 추가 (CopyIn/CopyOut/CopyBoth) | #107 / #109 |

### Hotfix: Audit Logger Memory Leak

| Task | 작업 | 이슈/PR |
|------|------|---------|
| HF-2 | lastWebhook map 주기적 정리 goroutine 추가, DedupInterval 설정 노출 | #108 / #110 |

### Hotfix: Connection Pool Poisoning (Protocol Desync)

| Task | 작업 | 이슈/PR |
|------|------|---------|
| HF-3 | relayAndCollect/queryReplayLSN 에러 시 Release → Discard로 오염 커넥션 즉시 폐기 | #111 / #113 |

### Hotfix: Global Panic Vulnerability

| Task | 작업 | 이슈/PR |
|------|------|---------|
| HF-4 | 클라이언트 핸들링 고루틴에 defer recover() 격리형 복구 도입 | #112 / #114 |

### Hotfix: Data API Zombie Goroutine Leak

| Task | 작업 | 이슈/PR |
|------|------|---------|
| HF-5 | Data API executeOnPool()에 context 취소 워치독 추가 (SetDeadline 패턴) | #115 / #117 |

### Hotfix: Admin/DataAPI Dangling Pointer After Reload

| Task | 작업 | 이슈/PR |
|------|------|---------|
| HF-6 | Admin/DataAPI 서버의 직접 포인터 → getter 함수 패턴으로 전환, 핫-리로드 안전성 확보 | #116 / #118 |

### Feature: Writer-Only Mode

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | readers 선택사항으로 변경, writer-only 모드 지원 (최소 설정으로 사용 가능) | #101 / #102 |

### Hotfix: Hot Reload Data Race

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | sync.RWMutex 도입으로 hot reload 시 concurrent map read/write 방지 | #103 / #104 |

### Refactoring: server.go 파일 분리

| Task | 작업 | 이슈/PR |
|------|------|---------|
| RF-1 | server.go (2,259줄) → 9개 역할별 파일 분리 (server, auth, query, query_read, query_extended, copy, backend, lsn, helpers) | #119 / #120 |

### Hotfix: Balancer RLock

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | RoundRobin의 MarkUnhealthy, checkBackends, HealthyCount에 RLock 추가 | #121 / #124 |

### Hotfix: Graceful Shutdown Timeout

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | Graceful shutdown 시 무한 대기 방지를 위한 타임아웃 추가 | #122 / #125 |

### Hotfix: CancelRequest Protocol

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | PostgreSQL CancelRequest 프로토콜 처리 (proxy/cancel.go 추가) | #123 / #126 |

### Hotfix: DataAPI Live Config & Cache Broadcast

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | Data API에서 매 요청마다 라이브 설정에서 API Key 읽기 (hot reload 대응) | #127 / #132 |
| - | Data API 쓰기 시 다른 인스턴스에 캐시 무효화 브로드캐스트 | #128 / #133 |

### Hotfix: ConfigMap Symlink & Audit Race

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | ConfigMap symlink swap 감지를 모든 OS 이벤트 타입에서 처리 | #129 / #134 |
| - | Reload() 주석 수정 + audit 테스트 race condition 수정 | #130, #131 / #135 |

### QA Round 3-4 & Bugfix

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | AST 라우팅 사각지대, 캐시 무효화 실종, 중복 파싱, 헬스체크 병렬화, splitStatements 보강 | #143~#147 |
| - | 캐시 키 namespace, Data API read table extractor, balancer 상태 보존, HTTP lifecycle, AST 재파싱 | #153~#157 / #158~#162 |

### Cleanup: Dead Code Removal

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | 미사용 함수 5건 제거 (handleReadQuery, Ping, ReplayLSN, NormalizeQuery, SemanticCacheKeyWithParams) | #163 / #164 |

### Phase 21: Query Mirroring

| Task | 작업 | 이슈/PR |
|------|------|---------|
| - | Shadow DB 비동기 미러링 (fire-and-forget 워커 풀) | #165 / #166 |
| - | 패턴별 P50/P99 레이턴시 비교, 순환 버퍼 통계, 자동 회귀 감지 | #165 / #166 |
| - | MirrorConfig 설정, 테이블 필터, read_only/all 모드 | #165 / #166 |
| - | Admin API: GET /admin/mirror/stats 엔드포인트 | #165 / #166 |
