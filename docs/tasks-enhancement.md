## 초고도화 Task (Phase 12-13)

> Phase 12-13 모두 완료됨. 차기 작업(Phase 14-16)은 `docs/tasks-next.md` 참고.

---

### Phase 12: LSN 기반 Causal Consistency (W20)

**목표**: Writer에 쓰기 작업이 일어날 때 LSN(Log Sequence Number)을 트래킹하고, 이후 Read 쿼리 시 해당 LSN까지 복제가 완료된 Reader에게만 쿼리를 보내는 Replication-Lag-Aware 라우팅 구현.

**현재 한계**: `read_after_write_delay`는 고정 타이머 기반. 타이머가 짧으면 stale read, 길면 Reader 활용도 저하. Replication lag은 네트워크/부하 상태에 따라 가변적이므로 고정 타이머로는 정확한 제어가 불가능.

**핵심 아이디어**:
- Write 후 `pg_current_wal_lsn()` 조회 → 세션에 LSN 저장
- Read 시 각 Reader의 `pg_last_wal_replay_lsn()` 확인 → 세션 LSN 이상인 Reader만 선택
- 충분히 따라잡은 Reader가 없으면 Writer로 fallback (기존 동작과 동일)

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T12-1 | LSN 타입 및 비교 유틸리티 | `feat(router): LSN 타입 정의 및 비교 유틸리티` |
| T12-2 | Writer LSN 트래킹 | `feat(proxy): Write 후 LSN 트래킹` |
| T12-3 | Reader LSN 폴링 및 LSN-Aware 밸런서 | `feat(router): LSN-Aware Reader 선택` |
| T12-4 | Causal Consistency E2E 테스트 | `test: LSN 기반 Causal Consistency 테스트` |

#### T12-1: LSN 타입 및 비교 유틸리티
- **범위**: PostgreSQL LSN 포맷(`X/XXXXXXXX`) 파싱, `uint64` 변환, 비교 함수
- **완료 기준**: `"0/16B3748"` → uint64 변환 → 크기 비교 단위 테스트 통과
- **위치**: `internal/router/lsn.go`
- **핵심 로직**:
  ```go
  type LSN uint64
  func ParseLSN(s string) (LSN, error)  // "0/16B3748" → LSN
  func (l LSN) String() string           // LSN → "0/16B3748"
  ```

#### T12-2: Writer LSN 트래킹
- **범위**: Write 쿼리 실행 후 같은 백엔드 커넥션에서 `SELECT pg_current_wal_lsn()` 실행 → 세션에 저장
- **완료 기준**: Write 후 `session.lastWriteLSN`에 유효한 LSN 저장 확인
- **핵심 변경**:
  - `Session.lastWriteTime` → `Session.lastWriteLSN` 교체 (기존 타이머 기반 로직 대체)
  - `handleWriteQuery()` 말미에 LSN 조회 추가
- **성능 고려**: LSN 조회는 Writer 커넥션에서 추가 라운드트립 1회. Write 빈도가 Read보다 훨씬 낮으므로 오버헤드 수용 가능
- **설정**: `routing.causal_consistency: true` (기본값 false — 기존 `read_after_write_delay`와 양자택일)

#### T12-3: Reader LSN 폴링 및 LSN-Aware 밸런서
- **범위**: 각 Reader의 replay LSN을 주기적으로 폴링하고, Read 쿼리 시 세션 LSN 이상인 Reader만 선택
- **완료 기준**: Writer에 쓴 직후 Read → replay가 완료된 Reader에서만 응답, 미완료 Reader는 스킵
- **핵심 변경**:
  - `RoundRobin`에 각 Reader의 `replayLSN` 필드 추가
  - 헬스체크 고루틴에서 `SELECT pg_last_wal_replay_lsn()` 주기적 조회 (1초 간격)
  - `Next(minLSN LSN)` — minLSN 이상인 healthy Reader 중 라운드로빈 선택
  - 적합한 Reader가 없으면 Writer fallback + 메트릭 기록
- **메트릭**: `dbproxy_reader_lsn_lag_bytes` (Gauge, Reader별 LSN 지연 바이트)
- **위치**: `internal/router/balancer.go` 수정

#### T12-4: Causal Consistency E2E 테스트
- **범위**: Docker 환경에서 Write → 즉시 Read 시 최신 데이터 보장 검증
- **완료 기준**:
  - INSERT → 즉시 SELECT → 방금 쓴 row 반환 (stale read 없음)
  - 모든 Reader가 지연 중일 때 Writer fallback 동작
  - Replica 따라잡은 후에는 Reader로 정상 분산
- **테스트 방법**: Replica에 인위적 지연 주입 (`pg_sleep` 또는 WAL replay 일시정지)

---

### Phase 13: AST 기반 쿼리 파서 + 쿼리 방화벽 (W21-W22)

**목표**: `pg_query_go`를 도입하여 PostgreSQL 실제 파서(AST)로 쿼리를 분석. 정규식/문자열 기반 파서의 근본적 한계를 해결하고, 위험 쿼리 차단(방화벽) 및 의미적 캐시 키(Semantic Caching) 기능을 추가.

**현재 한계**: 문자열 기반 파서는 Dollar Quoting, Nested Comments, Quoted Identifiers 등 PostgreSQL 고유 문법에서 반복적으로 우회됨. QA 리포트에서 수차례 취약점 발견 — 패치로 덮는 것은 한계가 있음.

**트레이드오프**: `pg_query_go`는 cgo 의존 (libpg_query C 라이브러리). 빌드 시 C 컴파일러 필요. "외부 의존성 최소화" 원칙과 상충하지만, 보안 결함의 근본 원인 제거를 위해 수용.

| Task | 작업 | 예상 이슈 제목 |
|------|------|----------------|
| T13-1 | pg_query_go 도입 및 AST 파서 기반 구축 | `feat(router): pg_query_go AST 파서 도입` |
| T13-2 | Classify/ExtractTables AST 전환 | `refactor(router): Classify/ExtractTables AST 기반 전환` |
| T13-3 | 쿼리 방화벽 (Query Firewall) | `feat(router): AST 기반 쿼리 방화벽` |
| T13-4 | Semantic Cache Key (AST 정규화) | `feat(cache): AST 정규화 기반 Semantic Cache Key` |
| T13-5 | AST 파서 테스트 및 벤치마크 | `test: AST 파서 정확도 및 성능 벤치마크` |

#### T13-1: pg_query_go 도입 및 AST 파서 기반 구축
- **범위**: `go get github.com/pganalyze/pg_query_go/v5`, AST 파싱 래퍼 함수 작성
- **완료 기준**: 임의 SQL → AST 트리 변환 성공, 노드 탐색 유틸리티 동작
- **위치**: `internal/router/ast.go`
- **핵심 로직**:
  ```go
  func ParseSQL(query string) (*pg_query.ParseResult, error)
  func WalkNodes(tree *pg_query.ParseResult, fn func(node *pg_query.Node) bool)
  ```
- **빌드 변경**: `Makefile`, CI에 cgo 빌드 환경 추가, `Dockerfile` 업데이트

#### T13-2: Classify/ExtractTables AST 전환
- **범위**: 기존 `Classify()`, `ExtractTables()`를 AST 기반으로 교체
- **완료 기준**: 기존 단위 테스트 전체 통과 + QA 리포트의 모든 우회 케이스 방어
- **핵심 로직**:
  - `Classify`: AST 루트 노드 타입으로 판단 (`SelectStmt` → Read, `InsertStmt`/`UpdateStmt`/`DeleteStmt` → Write)
  - `ExtractTables`: AST에서 `RangeVar` 노드 수집 → 테이블명 추출
  - 힌트 주석: `pg_query_go`의 comment 추출 기능 활용
- **하위 호환**: 기존 문자열 파서를 fallback으로 유지 (`ast_parser: true/false` 설정)
- **위치**: `internal/router/parser.go` 수정, `internal/router/parser_ast.go` 신규

#### T13-3: 쿼리 방화벽 (Query Firewall)
- **범위**: AST 트리 구조 검사로 위험 쿼리를 프록시 단에서 차단
- **완료 기준**: `DELETE FROM users;` (WHERE 없음) → ErrorResponse 반환, `DELETE FROM users WHERE id=1` → 통과
- **차단 규칙** (설정으로 ON/OFF):
  - `DELETE`/`UPDATE` 문에 `WHERE` 절 누락 → 차단
  - `DROP TABLE`/`TRUNCATE` → 차단 (옵션)
  - `SELECT *` 무제한 (LIMIT 없이 대형 테이블) → 경고 로그 (차단은 선택적)
- **설정 예시**:
  ```yaml
  firewall:
    enabled: true
    block_delete_without_where: true
    block_update_without_where: true
    block_drop_table: false
    block_truncate: false
  ```
- **메트릭**: `dbproxy_firewall_blocked_total{rule="delete_no_where|update_no_where|drop_table"}` 카운터
- **위치**: `internal/router/firewall.go`

#### T13-4: Semantic Cache Key (AST 정규화)
- **범위**: AST를 정규화(normalize)하여 의미적으로 동일한 쿼리가 같은 캐시 키를 갖도록 개선
- **완료 기준**: `WHERE A=1 AND B=2`와 `WHERE B=2 AND A=1`이 같은 캐시 키 → 캐시 히트
- **정규화 규칙**:
  - WHERE 조건의 AND/OR 자식 노드를 알파벳 순으로 정렬
  - 리터럴 값을 플레이스홀더(`$N`)로 치환 (parameterized cache)
  - 불필요한 공백/대소문자 차이 제거
- **핵심 함수**: `NormalizeAST(tree) → canonical string → FNV hash`
- **메트릭**: 캐시 히트율 변화 모니터링 (기존 `dbproxy_cache_hits_total`)
- **위치**: `internal/cache/normalize.go`

#### T13-5: AST 파서 테스트 및 벤치마크
- **범위**: 정확도 테스트 (기존 모든 파서 테스트 + QA 우회 케이스) + 성능 벤치마크 (문자열 파서 vs AST 파서)
- **완료 기준**:
  - 기존 parser_test.go, parser_edge_test.go, dollar_quote_test.go, nested_comment_test.go, quoted_table_test.go 전체 통과
  - AST 파싱 레이턴시: 일반 쿼리 기준 < 1ms (벤치마크)
  - 문자열 파서 대비 오버헤드 측정 및 문서화
- **성능 고려**: pg_query_go는 내부적으로 PostgreSQL 실제 파서를 호출하므로 문자열 파싱보다 느림. 캐시 적용으로 동일 쿼리 반복 파싱 방지 가능
