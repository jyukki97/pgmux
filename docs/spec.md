## DB Proxy

애플리케이션과 데이터베이스 사이에 위치하여 커넥션 관리, 쿼리 라우팅, 캐싱을 수행하는 프록시 레이어.

---

### 1. 커넥션 풀링

애플리케이션이 DB에 직접 연결하지 않고 프록시가 커넥션을 재사용하여 리소스를 절약한다.

- **풀 관리**
  - 최소/최대 커넥션 수 설정 (`min_connections`, `max_connections`)
  - 유휴 커넥션 타임아웃 — 일정 시간 미사용 시 자동 반환 (`idle_timeout`)
  - 커넥션 최대 수명 — 오래된 커넥션 주기적 갱신 (`max_lifetime`)
- **커넥션 획득**
  - 풀에 여유가 있으면 즉시 할당
  - 풀이 가득 찬 경우 대기 큐에 넣고, 설정된 타임아웃 초과 시 에러 반환 (`connection_timeout`)
- **헬스체크**
  - 주기적으로 커넥션에 ping을 보내 유효성 검증
  - 비정상 커넥션은 풀에서 제거 후 새 커넥션으로 교체

---

### 2. 읽기/쓰기(R/W) 쿼리 자동 분산

쿼리를 파싱하여 Writer(Primary)와 Reader(Replica)로 자동 라우팅한다.

- **쿼리 분류 규칙**
  - `SELECT` → Reader
  - `INSERT`, `UPDATE`, `DELETE`, `CREATE`, `ALTER`, `DROP` → Writer
  - 트랜잭션 내부(`BEGIN` ~ `COMMIT`/`ROLLBACK`) → 모든 쿼리를 Writer로 전송
  - 힌트 주석으로 강제 라우팅 지원 (예: `/* route:writer */ SELECT ...`)
- **Reader 로드밸런싱**
  - 다수의 Replica가 있을 때 라운드로빈 방식으로 분산
  - 특정 Replica 장애 시 자동으로 목록에서 제외하고 나머지로 분산
- **Replication Lag 대응**
  - 쓰기 직후 읽기 시 일정 시간(`read_after_write_delay`) 동안 Writer에서 읽기 수행
  - 이를 통해 "방금 쓴 데이터가 안 보이는" 문제 방지

---

### 3. 반복 쿼리 캐싱

동일한 쿼리가 짧은 시간 내에 반복될 때 DB를 거치지 않고 캐시된 결과를 반환한다.

- **캐시 대상**
  - `SELECT` 쿼리만 캐싱 (쓰기 쿼리는 캐싱하지 않음)
  - 쿼리 텍스트 + 파라미터를 해시하여 캐시 키 생성
- **만료 정책**
  - TTL 기반 — 항목별 유효 시간 설정 (`cache_ttl`)
  - 관련 테이블에 쓰기 발생 시 해당 테이블의 캐시 무효화 (write-through invalidation)
- **제한**
  - 최대 캐시 항목 수 제한 (`max_cache_entries`)
  - LRU 방식으로 오래된 항목부터 제거
  - 결과 크기가 임계값을 초과하는 쿼리는 캐싱 제외 (`max_result_size`)

---

---

### 4. Prometheus 메트릭

프록시의 런타임 상태를 Prometheus 형식으로 노출하여 Grafana 등에서 모니터링한다.

- **커넥션 풀 메트릭**
  - 현재 열린 커넥션 수 (writer/reader별) — `dbproxy_pool_connections_open`
  - 유휴 커넥션 수 — `dbproxy_pool_connections_idle`
  - 커넥션 획득 대기 수 — `dbproxy_pool_waiting_total`
  - 커넥션 획득 레이턴시 히스토그램 — `dbproxy_pool_acquire_duration_seconds`
- **쿼리 라우팅 메트릭**
  - 쿼리 라우팅 카운터 (writer/reader별) — `dbproxy_queries_routed_total{target="writer|reader"}`
  - 쿼리 처리 레이턴시 히스토그램 — `dbproxy_query_duration_seconds`
  - Writer fallback 횟수 — `dbproxy_reader_fallback_total`
- **캐시 메트릭**
  - 캐시 히트/미스 카운터 — `dbproxy_cache_hits_total`, `dbproxy_cache_misses_total`
  - 캐시 항목 수 — `dbproxy_cache_entries`
  - 캐시 무효화 카운터 — `dbproxy_cache_invalidations_total`
- **엔드포인트**
  - `GET /metrics` — Prometheus scrape 엔드포인트
  - 설정의 `metrics.listen` 포트에서 HTTP로 제공

---

### 5. Prepared Statement 라우팅

Extended Query Protocol의 `Parse` 메시지에서 SQL을 추출하여 reader 라우팅을 지원한다. 현재는 Extended Query가 무조건 writer로 가지만, SELECT의 Prepared Statement도 reader로 보낼 수 있어야 한다.

- **Parse 메시지 SQL 추출**
  - `Parse` 메시지 포맷: statement name (string) + query (string) + param count + param OIDs
  - 쿼리 텍스트를 추출하여 `Classify()` 적용
- **세션별 Statement 맵**
  - `map[string]Route` — statement name → 라우팅 결과 캐싱
  - `Bind`/`Execute` 시 해당 statement의 라우팅 결과를 참조
  - unnamed statement (`""`)은 매번 분류
- **라우팅 규칙**
  - SELECT Prepared Statement → reader (RoundRobin)
  - 쓰기 Prepared Statement → writer
  - 트랜잭션 내부 → 전체 writer (기존 Session 로직 유지)
- **제한**
  - reader로 라우팅된 Extended Query의 응답은 캐싱 지원
  - `DEALLOCATE` 시 statement 맵에서 제거

---

### 6. Admin API

프록시의 런타임 상태를 조회하고 동적으로 제어하는 HTTP 관리 인터페이스.

- **상태 조회**
  - `GET /admin/stats` — 풀, 캐시, 라우팅 통계 JSON
  - `GET /admin/health` — 백엔드 헬스 상태 (writer + reader별)
  - `GET /admin/config` — 현재 적용된 설정 (비밀번호 마스킹)
- **동적 제어**
  - `POST /admin/cache/flush` — 전체 캐시 즉시 비우기
  - `POST /admin/cache/flush/{table}` — 특정 테이블 캐시만 무효화
- **보안**
  - 별도 포트에서 제공 (외부 노출 방지)
  - 향후 Bearer token 인증 추가 가능

---

### 설정 예시

```yaml
proxy:
  listen: "0.0.0.0:5432"

writer:
  host: "primary.db.internal"
  port: 5432

readers:
  - host: "replica-1.db.internal"
    port: 5432
  - host: "replica-2.db.internal"
    port: 5432

pool:
  min_connections: 5
  max_connections: 50
  idle_timeout: "10m"
  max_lifetime: "1h"
  connection_timeout: "5s"

routing:
  read_after_write_delay: "500ms"

cache:
  enabled: true
  cache_ttl: "10s"
  max_cache_entries: 10000
  max_result_size: "1MB"

backend:
  user: "postgres"
  password: "postgres"
  database: "testdb"

metrics:
  enabled: true
  listen: "0.0.0.0:9090"

admin:
  enabled: true
  listen: "0.0.0.0:9091"
```