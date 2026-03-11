## 고도화 Task (Phase 8-11)

---

### Phase 8: Transaction Pooling (W16)

**목표**: Writer 커넥션도 풀에서 관리하여, 수천 클라이언트가 수십 개의 백엔드 커넥션을 공유하는 진정한 Connection Multiplexing 구현.

**현재 문제**: `proxy/server.go:200` — 클라이언트 접속마다 `net.Dial`로 Writer에 1:1 전용 커넥션을 맺음. 1000명 접속 = 1000개 백엔드 커넥션.

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T8-1 | Writer 커넥션 풀 도입 | `feat(pool): Writer 커넥션 풀링 도입` |
| T8-2 | 트랜잭션 레벨 커넥션 바인딩 | `feat(proxy): 트랜잭션 레벨 커넥션 바인딩` |
| T8-3 | 세션 상태 리셋 (DISCARD ALL) | `feat(pool): 커넥션 반환 시 세션 상태 리셋` |
| T8-4 | Simple Query 트랜잭션 풀링 통합 | `feat(proxy): Simple Query 트랜잭션 풀링` |
| T8-5 | Extended Query 트랜잭션 풀링 통합 | `feat(proxy): Extended Query 트랜잭션 풀링` |
| T8-6 | Transaction Pooling E2E 테스트 | `test: Transaction Pooling E2E 테스트` |

#### T8-1: Writer 커넥션 풀 도입
- **범위**: `NewServer()`에서 Writer도 `pool.Pool`로 생성. `handleConn()`에서 `net.Dial` 대신 풀에서 커넥션 관리
- **완료 기준**: Writer 커넥션이 풀에서 관리되고, 여러 클라이언트가 백엔드 커넥션을 공유
- **핵심 변경**: `writerConn net.Conn` → `writerPool *pool.Pool`, 인증 흐름을 풀 기반으로 재설계

#### T8-2: 트랜잭션 레벨 커넥션 바인딩
- **범위**: BEGIN 시 Writer 풀에서 커넥션 획득 → 트랜잭션 동안 고정 → COMMIT/ROLLBACK 시 풀에 반환
- **완료 기준**: 트랜잭션 중에는 동일 백엔드 커넥션 사용, 트랜잭션 종료 후 반환 확인
- **핵심 로직**: `session.writerConn`을 트랜잭션 시작/종료에 맞춰 Acquire/Release

#### T8-3: 세션 상태 리셋
- **범위**: 커넥션이 풀에 반환될 때 `DISCARD ALL` 또는 `RESET ALL` 전송하여 세션 변수, Prepared Statement 등 초기화
- **완료 기준**: 클라이언트 A가 `SET timezone='UTC'` 후 반환 → 클라이언트 B가 같은 커넥션 획득 시 기본 timezone
- **참고**: PgBouncer의 `server_reset_query` 설정과 동일 개념

#### T8-4: Simple Query 트랜잭션 풀링 통합
- **범위**: `relayQueries()`에서 비트랜잭션 단일 쿼리 시 Writer 풀에서 빌려 사용 후 즉시 반환
- **완료 기준**: 동시 100개 클라이언트가 INSERT 실행 시 백엔드 커넥션은 `max_connections` 이내
- **핵심 변경**: `handleWriteQuery()`가 풀에서 커넥션을 획득/반환하도록 수정

#### T8-5: Extended Query 트랜잭션 풀링 통합
- **범위**: Parse~Sync 배치 단위로 Writer 풀에서 커넥션 획득/반환
- **완료 기준**: Prepared Statement 기반 쓰기도 풀링 동작 확인
- **주의**: 트랜잭션 내부 Extended Query는 바인딩된 커넥션 유지

#### T8-6: Transaction Pooling E2E 테스트
- **범위**: 동시 접속 수 >> max_connections 시나리오, 트랜잭션 격리 검증, 세션 리셋 검증
- **완료 기준**: 100개 동시 클라이언트 / 10개 백엔드 커넥션으로 전체 시나리오 통과
- **테스트 시나리오**:
  - 동시 INSERT 100건 → 백엔드 커넥션 10개 이내
  - BEGIN → INSERT → SELECT → COMMIT 격리 확인
  - SET 후 커넥션 반환 → 다음 사용자에게 영향 없음

---

### Phase 9: SSL/TLS Termination + Front-end Auth (W17)

**목표**: 프록시에서 TLS를 종단하여 클라이언트-프록시 구간을 암호화하고, 프록시 자체 인증으로 불량 접속을 사전 차단.

**현재 문제**: `proxy/server.go:184` — SSL 요청 시 `'N'` 거부, 평문 통신만 허용. 인증은 백엔드로 패스스루.

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T9-1 | TLS 설정 구조체 추가 | `feat(config): TLS 설정 항목 추가` |
| T9-2 | TLS Listener 구현 | `feat(proxy): TLS Termination 구현` |
| T9-3 | Front-end Auth 설정 | `feat(config): 프록시 자체 인증 설정` |
| T9-4 | Front-end Auth 구현 | `feat(proxy): 프록시 자체 인증 처리` |
| T9-5 | TLS + Auth E2E 테스트 | `test: TLS 및 자체 인증 E2E 테스트` |

#### T9-1: TLS 설정 구조체 추가
- **범위**: `config.go`에 `TLSConfig` 추가 — `enabled`, `cert_file`, `key_file`, `client_ca_file` (mTLS 옵션)
- **완료 기준**: TLS 설정이 YAML에서 파싱되고 validation 통과
- **설정 예시**:
  ```yaml
  tls:
    enabled: true
    cert_file: "/path/to/server.crt"
    key_file: "/path/to/server.key"
  ```

#### T9-2: TLS Listener 구현
- **범위**: SSL 요청 시 `'S'` 응답 후 `tls.Server()`로 업그레이드. `crypto/tls`로 인증서 로드
- **완료 기준**: `psql "sslmode=require"` 로 프록시에 TLS 접속 성공
- **핵심 변경**: `handleConn()`에서 SSL 요청 분기 — TLS 설정 시 `'S'` 응답 + TLS 핸드셰이크

#### T9-3: Front-end Auth 설정
- **범위**: `config.go`에 `AuthConfig` 추가 — `enabled`, `users` 리스트 (username/password 쌍)
- **완료 기준**: 인증 설정 파싱 및 validation
- **설정 예시**:
  ```yaml
  auth:
    enabled: true
    users:
      - username: "app_user"
        password: "secret"
      - username: "readonly"
        password: "readonly_pass"
  ```

#### T9-4: Front-end Auth 구현
- **범위**: 클라이언트 StartupMessage의 user를 확인 → 설정의 users와 대조 → 실패 시 ErrorResponse 반환 (백엔드 접속 없이 차단)
- **완료 기준**: 설정에 없는 유저로 접속 시 프록시에서 즉시 거부, 올바른 유저는 백엔드로 연결 진행
- **인증 방식**: MD5 또는 SCRAM-SHA-256 (기존 `pgconn.go` 로직 재활용)

#### T9-5: TLS + Auth E2E 테스트
- **범위**: 자체 서명 인증서로 TLS 접속, Front-end Auth 성공/실패 시나리오
- **완료 기준**:
  - `sslmode=require`로 TLS 접속 성공
  - `sslmode=disable`로도 접속 가능 (TLS 선택적)
  - 잘못된 유저/비밀번호로 접속 시 프록시에서 즉시 거부
  - 올바른 유저로 접속 시 정상 쿼리 실행

---

### Phase 10: Circuit Breaker & Rate Limiting (W18)

**목표**: 백엔드 장애 시 연쇄 장애를 방지하고, 악성 트래픽을 프록시 단에서 차단.

**현재 문제**: Reader 장애 시 Writer로 Fallback하지만, Fallback 트래픽이 Writer를 죽이는 Cascading Failure 방어 수단 없음.

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T10-1 | Circuit Breaker 상태 머신 구현 | `feat(resilience): Circuit Breaker 구현` |
| T10-2 | Circuit Breaker 풀 통합 | `feat(proxy): Circuit Breaker 풀 통합` |
| T10-3 | Token Bucket Rate Limiter 구현 | `feat(resilience): Token Bucket Rate Limiter 구현` |
| T10-4 | Rate Limiter 프록시 통합 | `feat(proxy): Rate Limiter 프록시 통합` |
| T10-5 | Circuit Breaker & Rate Limiter 테스트 | `test: Circuit Breaker 및 Rate Limiter 테스트` |

#### T10-1: Circuit Breaker 상태 머신 구현
- **범위**: `internal/resilience/breaker.go`에 Circuit Breaker 구현
- **상태 전이**: Closed(정상) → Open(차단, 에러율 초과 시) → Half-Open(시험 요청 허용)
- **설정**: `error_threshold` (에러율 %), `open_duration` (Open 유지 시간), `half_open_max` (Half-Open 시 허용 요청 수)
- **완료 기준**: 에러율 50% 초과 → Open → 일정 시간 후 Half-Open → 성공 시 Closed 복귀 단위 테스트

#### T10-2: Circuit Breaker 풀 통합
- **범위**: Writer/Reader 풀에 Circuit Breaker 적용. 쿼리 실행 실패를 Circuit Breaker에 기록, Open 시 즉시 에러 반환
- **완료 기준**: 백엔드 장애 시 Circuit Breaker가 Open되어 불필요한 연결 시도 차단
- **핵심**: Writer Fallback 시에도 Circuit Breaker 체크 — Writer도 Open이면 클라이언트에 에러 반환

#### T10-3: Token Bucket Rate Limiter 구현
- **범위**: `internal/resilience/ratelimit.go`에 Token Bucket 알고리즘 구현
- **설정**: `rate` (초당 허용 쿼리 수), `burst` (버스트 허용량), `per` (IP/User 단위)
- **완료 기준**: 초당 100 제한 설정 시 101번째 요청이 거부되는 단위 테스트

#### T10-4: Rate Limiter 프록시 통합
- **범위**: `relayQueries()`에서 쿼리 처리 전 Rate Limiter 체크. 초과 시 PG ErrorResponse 반환
- **완료 기준**: 특정 클라이언트가 과도한 쿼리 시 프록시에서 `too many requests` 에러 반환
- **메트릭**: `dbproxy_rate_limited_total` 카운터 추가

#### T10-5: Circuit Breaker & Rate Limiter 테스트
- **범위**: E2E 시나리오 — 백엔드 다운 시 Circuit Breaker 동작, 대량 요청 시 Rate Limiter 동작
- **완료 기준**:
  - 백엔드 중단 → Circuit Breaker Open → 빠른 에러 반환 (타임아웃 없이)
  - Circuit Breaker 복구 → 정상 처리 재개
  - 초당 제한 초과 요청 → 거부, 제한 이내 → 정상 처리

---

### Phase 11: Zero-Downtime Reload (W19)

**목표**: 프록시를 중단하지 않고 설정을 동적으로 변경할 수 있는 Graceful Reload 체계.

**현재 문제**: `config.yaml` 변경 시 프로세스 재시작 필요.

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T11-1 | SIGHUP 핸들링 + 설정 리로드 | `feat(config): SIGHUP 설정 리로드` |
| T11-2 | Admin API /admin/reload 엔드포인트 | `feat(admin): /admin/reload 엔드포인트` |
| T11-3 | Reader Pool Hot Swap | `feat(pool): Reader Pool 무중단 교체` |
| T11-4 | Graceful Reload E2E 테스트 | `test: 무중단 설정 리로드 E2E 테스트` |

#### T11-1: SIGHUP 핸들링 + 설정 리로드
- **범위**: `cmd/db-proxy/main.go`에서 SIGHUP 시그널 수신 → `config.Load()` 재호출 → Server에 새 설정 전달
- **완료 기준**: `kill -HUP <pid>` 전송 시 로그에 "config reloaded" 출력, 새 설정 적용
- **리로드 가능 항목**: Reader 목록, 풀 크기, 캐시 TTL, Rate Limit 설정
- **리로드 불가 항목**: `proxy.listen` (리스닝 포트), Writer 주소 (재시작 필요)

#### T11-2: Admin API /admin/reload 엔드포인트
- **범위**: `POST /admin/reload` — SIGHUP과 동일한 리로드 트리거
- **완료 기준**: `curl -X POST localhost:9091/admin/reload` → 설정 리로드 성공 JSON 응답

#### T11-3: Reader Pool Hot Swap
- **범위**: 새 설정의 Reader 목록으로 새 풀 생성 → 기존 풀의 활성 커넥션은 자연 종료 대기 → 새 풀로 교체
- **완료 기준**: Reader 추가/제거 시 기존 진행 중인 쿼리에 영향 없이 새 Reader로 전환
- **핵심 로직**:
  1. 새 Reader Pool 생성 + 헬스체크 시작
  2. Balancer에 새 Backend 목록 적용 (atomic swap)
  3. 기존 풀은 drain 후 Close

#### T11-4: Graceful Reload E2E 테스트
- **범위**: 쿼리 실행 중 SIGHUP → 기존 쿼리 정상 완료 + 새 설정 적용 확인
- **완료 기준**:
  - 장시간 쿼리 실행 중 리로드 → 쿼리 정상 완료
  - Reader 추가 후 리로드 → 새 Reader로 트래픽 분산 시작
  - 풀 크기 변경 후 리로드 → 새 max_connections 적용 확인
