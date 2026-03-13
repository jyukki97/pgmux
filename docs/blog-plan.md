## 블로그 포스팅 계획

개발이 어느정도 진행되면 블로그에 정리한다.

- **블로그 루트**: `/Users/nhn/Library/Mobile Documents/com~apple~CloudDocs/project/study-blog/`
- **형식**: Hugo (기존 블로그와 동일한 frontmatter 형식)

---

### 프로젝트 페이지

`content/projects/pgmux.md`에 등록 완료. (simple-queue-service와 동일한 형식)

---

### 시리즈 포스트

Phase 완료 시점마다 `content/posts/`에 시리즈 포스트를 작성한다.

- **경로**: `content/posts/YYYY-MM-DD-pgmux-N-제목.md`

| # | 시점 | 제목 (안) | 주요 내용 | 상태 |
|---|------|-----------|-----------|------|
| P1 | Phase 2 완료 후 | "Go로 PostgreSQL 프록시 만들기 (1) - PG Wire Protocol 이해" | PG 프로토콜 구조, 핸드셰이크 과정, 바이트 레벨 분석, 구현 시 삽질 | 완료 |
| P2 | Phase 3 완료 후 | "Go로 PostgreSQL 프록시 만들기 (2) - 커넥션 풀링 직접 구현" | 풀링이 왜 필요한지, 자료구조 설계, 동시성 처리(mutex vs channel), 벤치마크 | 완료 |
| P3 | Phase 4 완료 후 | "Go로 PostgreSQL 프록시 만들기 (3) - 읽기/쓰기 자동 분산" | 쿼리 파싱 전략, 트랜잭션 추적, replication lag 대응, 장애 감지 | 완료 |
| P4 | Phase 5 완료 후 | "Go로 PostgreSQL 프록시 만들기 (4) - 쿼리 캐싱과 무효화" | LRU 구현, 캐시 키 설계, 테이블별 무효화 전략, 캐시 히트율 분석 | 완료 |
| P5 | Phase 6 완료 후 | "Go로 PostgreSQL 프록시 만들기 (5) - 통합, E2E 테스트, 회고" | 컴포넌트 통합, SCRAM-SHA-256, E2E 테스트, 벤치마크 | 완료 |
| P6 | Phase 7 완료 후 | "Go로 PostgreSQL 프록시 만들기 (6) - 메트릭, Admin API, Prepared Statement" | Prometheus 메트릭, Admin API, Extended Query 라우팅 | 완료 |
| P9 | Phase 8 완료 후 | "Go로 PostgreSQL 프록시 만들기 (9) - Transaction Pooling" | 진정한 Conn Multiplexing, DISCARD ALL, PgBouncer 비교 | 완료 |
| P10 | Phase 9 완료 후 | "Go로 PostgreSQL 프록시 만들기 (10) - TLS Termination과 프록시 인증" | TLS Termination, Front-end Auth, 보안 아키텍처 | 완료 |
| P11 | Phase 10 완료 후 | "Go로 PostgreSQL 프록시 만들기 (11) - Circuit Breaker와 Rate Limiting" | 연쇄 장애 방어, Token Bucket, Resilience 패턴 | 완료 |
| P12 | Phase 11 완료 후 | "Go로 PostgreSQL 프록시 만들기 (12) - 무중단 설정 리로드" | SIGHUP, Hot Swap, Graceful Reload | 완료 |
| P13 | Phase 12 완료 후 | "Go로 PostgreSQL 프록시 만들기 (13) - LSN 기반 Causal Consistency" | WAL LSN 추적, Reader 폴링, Causal Read | 완료 |
| P14 | Phase 13 완료 후 | "Go로 PostgreSQL 프록시 만들기 (14) - AST 파서와 쿼리 방화벽" | pg_query_go, AST 분류, Firewall, Semantic Cache | 완료 |
| P15 | 보안 QA 후 | "Go로 PostgreSQL 프록시 만들기 (15) - 보안 QA와 취약점 수정" | 캐시 충돌, 무한 재귀, CTE 방화벽 우회, 힌트 주입 | 완료 |
| P16 | Phase 14-16 후 | "Go로 PostgreSQL 프록시 만들기 (16) - Audit, Helm, Serverless API" | 감사 로그, Slow Query, Webhook, Helm Chart, Data API | 완료 |
| P17 | Hotfix 후 | "Go로 PostgreSQL 프록시 만들기 (17) - Channel Blocking과 Connection Poisoning 버그 수정" | Webhook 블로킹, 커넥션 독극물, 비동기 고루틴, drainUntilReady 에러 처리 | 완료 |
| P18 | Phase 17-18 후 | "Go로 PostgreSQL 프록시 만들기 (18) - OpenTelemetry 분산 추적과 설정 자동 리로드" | OTel TracerProvider, Span 계측, traceparent 전파, fsnotify 파일 감시, K8s ConfigMap 지원 | 완료 |
| P19 | #101 후 | "Go로 PostgreSQL 프록시 만들기 (19) - Writer-Only 모드와 진입장벽 낮추기" | readers 선택사항, writer-only 모드, 최소 설정, DX 개선 | 완료 |
| P20 | #103 후 | "Go로 PostgreSQL 프록시 만들기 (20) - Hot Reload Data Race와 sync.RWMutex" | concurrent map read/write, sync.RWMutex, accessor 패턴, go test -race | 완료 |
| P21 | #105 후 | "Go로 PostgreSQL 프록시 만들기 (21) - Prepared Statement Multiplexing" | Parse/Bind 인터셉트, SQL 리터럴 직렬화, Query Synthesizer, SQL Injection 방어 | 완료 |
| P22 | #107,#108 후 | "Go로 PostgreSQL 프록시 만들기 (22) - COPY 프로토콜 교착과 Map 메모리 누수" | COPY IN/OUT 핸들링, relay deadlock, 무한 map 성장 방지, 주기적 eviction | 완료 |
| P23 | #111,#112 후 | "Go로 PostgreSQL 프록시 만들기 (23) - 커넥션 풀 오염과 Panic 격리" | Release vs Discard, Protocol Desync, recover() 격리형 복구, 고루틴 안전 패턴 | 완료 |
| P24 | #115,#116 후 | "Go로 PostgreSQL 프록시 만들기 (24) - 좀비 고루틴과 Dangling Pointer" | Context 취소 워치독, SetDeadline 패턴, getter 함수로 핫-리로드 안전성 확보 | 완료 |
| P25 | #119 후 | "Go로 PostgreSQL 프록시 만들기 (25) - 2,259줄 God Object 해체기" | server.go 9개 파일 분리, 파일 분리 기준, Go 패키지 내 파일 구조 가이드 | 완료 |
| P28 | #137,#138 후 | "Go로 PostgreSQL 프록시 만들기 (28) - Reload 200 OK 거짓말과 Webhook 고루틴 유실" | admin reload HTTP 200→500, Webhook WaitGroup 추적, 종료 경로 버그 | 완료 |
| P29 | #141 후 | "Go로 PostgreSQL 프록시 만들기 (29) - WriteHeader 이후 헤더 동결과 테스트가 못 잡는 버그" | WriteHeader Content-Type 동결, SetReloadFunc data race, httptest.ResponseRecorder 맹점 | 완료 |
| P30 | #143~#147 후 | "Go로 PostgreSQL 프록시 만들기 (30) - AST 라우팅 사각지대와 캐시 무효화 실종" | AST 라우팅 미반영, 캐시 tables=nil, 중복 파싱 5회/요청, 헬스체크 병렬화, splitStatements 보강 | 완료 |
| P31 | #153~#157 후 | "Go로 PostgreSQL 프록시 만들기 (31) - 캐시 포맷 충돌과 HTTP 서버 수명주기" | 캐시 키 namespace, Data API read table extractor, balancer 상태 보존, HTTP lifecycle, AST 재파싱 제거 | 완료 |
| P32 | #165 후 | "Go로 PostgreSQL 프록시 만들기 (32) - Query Mirroring과 레이턴시 비교" | Shadow DB 비동기 미러링, fire-and-forget, P50/P99 비교, 회귀 감지, PgBouncer 비교 | 완료 |
| P33 | #169,#170 후 | "Go로 PostgreSQL 프록시 만들기 (33) - GitHub Actions CI/CD와 Docker 자동 배포" | CI 파이프라인, Release 워크플로우, multi-platform Docker, GHCR, golangci-lint | 완료 |
| P34 | #171 후 | "Go로 PostgreSQL 프록시 만들기 (34) - Multi-Database Routing" | DatabaseGroup 추상화, per-DB 풀/밸런서/CB, 캐시 키 FNV XOR 혼합, 하위호환 | 완료 |
| P35 | #173 후 | "Go로 PostgreSQL 프록시 만들기 (35) - Query Digest와 Top-N 쿼리 분석" | 쿼리 정규화, 순환 버퍼, P50/P99, RWMutex+DCL, Admin API, pg_stat_statements 프록시 버전 | 완료 |
| P36 | #175 후 | "Go로 PostgreSQL 프록시 만들기 (36) - Grafana Dashboard 템플릿" | 대시보드 설계, 21개 패널, __inputs, Helm sidecar ConfigMap, PromQL 패턴 | 완료 |
| P37 | #179,#181 후 | "Go로 PostgreSQL 프록시 만들기 (37) - 벤치마크 Suite와 PgBouncer 비교" | pgbench 3자 비교, bench-compare 스크립트, Docker 벤치 환경, SELECT-only/TPC-B | 완료 |
| P38 | #182 후 | "Go로 PostgreSQL 프록시 만들기 (38) - pprof 기반 최적화와 투명 프록시의 경계" | pprof CPU/alloc, atomic.Pointer, ReadMessageReuse, wire buffer 재사용, batching 한계 | 완료 |
| P39 | #182 후 | "Go로 PostgreSQL 프록시 만들기 (39) - DISCARD ALL 최적화와 벤치마크 신뢰성" | 세션 dirty 추적, RouteWithTxState lock 최적화, 벤치마크 방법론 | 완료 |
| P40 | #182 후 | "Go로 PostgreSQL 프록시 만들기 (40) - sync.Pool로 할당 줄이기와 최적화의 한계" | Timer sync.Pool, readBuf 사전할당, wire 버퍼 sync.Pool, unsafe.String 실패, Go 런타임 한계 | 완료 |
| P41 | #184 후 | "Go로 PostgreSQL 프록시 만들기 (41) - Per-User/Per-DB 커넥션 제한" | ConnTracker, TryAcquire mutex, SQLSTATE 53300, hot-reload 카운터 유지, PgBouncer 비교 | 완료 |
| P42 | #186 후 | "Go로 PostgreSQL 프록시 만들기 (42) - Query Timeout과 CancelRequest" | time.AfterFunc + CancelRequest, cancelTarget race 방어, 힌트 오버라이드, relay 무수정, PgBouncer 비교 | 완료 |

---

### 포스트 템플릿

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
