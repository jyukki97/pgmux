## 블로그 포스팅 계획

개발이 어느정도 진행되면 블로그에 정리한다.

- **블로그 루트**: `/Users/nhn/Library/Mobile Documents/com~apple~CloudDocs/project/study-blog/`
- **형식**: Hugo (기존 블로그와 동일한 frontmatter 형식)

---

### 프로젝트 페이지

`content/projects/db-proxy.md`에 등록 완료. (simple-queue-service와 동일한 형식)

---

### 시리즈 포스트

Phase 완료 시점마다 `content/posts/`에 시리즈 포스트를 작성한다.

- **경로**: `content/posts/YYYY-MM-DD-db-proxy-N-제목.md`

| # | 시점 | 제목 (안) | 주요 내용 | 상태 |
|---|------|-----------|-----------|------|
| P1 | Phase 2 완료 후 | "Go로 PostgreSQL 프록시 만들기 (1) - PG Wire Protocol 이해" | PG 프로토콜 구조, 핸드셰이크 과정, 바이트 레벨 분석, 구현 시 삽질 | 완료 |
| P2 | Phase 3 완료 후 | "Go로 PostgreSQL 프록시 만들기 (2) - 커넥션 풀링 직접 구현" | 풀링이 왜 필요한지, 자료구조 설계, 동시성 처리(mutex vs channel), 벤치마크 | 완료 |
| P3 | Phase 4 완료 후 | "Go로 PostgreSQL 프록시 만들기 (3) - 읽기/쓰기 자동 분산" | 쿼리 파싱 전략, 트랜잭션 추적, replication lag 대응, 장애 감지 | 완료 |
| P4 | Phase 5 완료 후 | "Go로 PostgreSQL 프록시 만들기 (4) - 쿼리 캐싱과 무효화" | LRU 구현, 캐시 키 설계, 테이블별 무효화 전략, 캐시 히트율 분석 | 완료 |
| P5 | Phase 6 완료 후 | "Go로 PostgreSQL 프록시 만들기 (5) - 통합, E2E 테스트, 회고" | 컴포넌트 통합, SCRAM-SHA-256, E2E 테스트, 벤치마크 | 완료 |
| P6 | Phase 7 완료 후 | "Go로 PostgreSQL 프록시 만들기 (6) - 메트릭, Admin API, Prepared Statement" | Prometheus 메트릭, Admin API, Extended Query 라우팅 | 완료 |
| P7 | Phase 8 완료 후 | "Go로 PostgreSQL 프록시 만들기 (7) - Transaction Pooling" | 진정한 Conn Multiplexing, DISCARD ALL, PgBouncer 비교 | 예정 |
| P8 | Phase 9 완료 후 | "Go로 PostgreSQL 프록시 만들기 (8) - TLS와 프록시 인증" | TLS Termination, Front-end Auth, 보안 아키텍처 | 예정 |
| P9 | Phase 10 완료 후 | "Go로 PostgreSQL 프록시 만들기 (9) - Circuit Breaker와 Rate Limiting" | 연쇄 장애 방어, Token Bucket, Resilience 패턴 | 예정 |
| P10 | Phase 11 완료 후 | "Go로 PostgreSQL 프록시 만들기 (10) - 무중단 설정 리로드" | SIGHUP, Hot Swap, Graceful Reload | 예정 |

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
