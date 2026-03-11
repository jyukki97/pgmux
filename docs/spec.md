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
```