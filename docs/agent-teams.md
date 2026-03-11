## Claude Code Agent Teams 가이드

모든 코딩 작업은 **Claude Code Agent Teams**(멀티 에이전트 오케스트레이션)를 활용한다.

---

### Agent Teams란?

여러 Claude Code 에이전트가 팀으로 협업하는 실험적 기능이다.

- **Team Lead**: 작업을 조율하고 Task를 분배하는 리더 에이전트
- **Teammate**: 각자 독립적인 컨텍스트에서 병렬로 작업하는 에이전트
- Teammate끼리 직접 소통 가능 (일반 서브에이전트와 차별점)
- 공유 Task 리스트로 의존성 자동 관리

### 활성화

`settings.json`에 아래 설정 추가:

```json
{
  "env": {
    "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1"
  }
}
```

또는 환경 변수: `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`

---

### Phase별 Agent Teams 활용 전략

| Phase | Team 구성 | 역할 분담 |
|-------|-----------|-----------|
| Phase 1 (기반) | Lead + 1 Teammate | Lead: 모듈 초기화 / Teammate: 설정 파싱+validation |
| Phase 2 (TCP) | Lead + 1 Teammate | Lead: TCP 리스너+릴레이 / Teammate: PG 프로토콜 핸드셰이크 |
| Phase 3 (풀링) | Lead + 2 Teammates | Lead: 풀 구조+획득반환 / T1: 대기큐+만료처리 / T2: 헬스체크 |
| Phase 4 (라우팅) | Lead + 3 Teammates | Lead: 라우팅 코어 / T1: 파서+힌트 / T2: 트랜잭션+delay / T3: 로드밸런서+장애감지 |
| Phase 5 (캐싱) | Lead + 2 Teammates | Lead: LRU 캐시 / T1: 키해싱+TTL / T2: 테이블추출+무효화 |
| Phase 6 (테스트) | Lead + 2 Teammates | Lead: docker-compose / T1: 통합테스트 / T2: 벤치마크 |
| Phase 8 (Tx풀링) | Lead + 2 Teammates | Lead: Writer 풀 + 아키텍처 / T1: 세션 리셋 + Simple Query / T2: Extended Query + 테스트 |
| Phase 9 (TLS/Auth) | Lead + 1 Teammate | Lead: TLS Listener / Teammate: Front-end Auth |
| Phase 10 (Resilience) | Lead + 1 Teammate | Lead: Circuit Breaker / Teammate: Rate Limiter |
| Phase 11 (Reload) | Lead + 1 Teammate | Lead: SIGHUP + Hot Swap / Teammate: Admin API + 테스트 |

---

### 프롬프트 예시

```
# Phase 8 작업 시작
"Agent team을 만들어서 Transaction Pooling을 구현해줘.

Lead: proxy/server.go 아키텍처 재설계 — Writer도 pool.Pool 사용
Teammate 1: pool 반환 시 DISCARD ALL 세션 리셋 + Simple Query 풀링 통합
Teammate 2: Extended Query 풀링 통합 + E2E 테스트

docs/tasks-enhancement.md의 Phase 8 참고.
각자 단위 테스트도 같이 작성해줘."

# 코드 리뷰 시
"Agent team으로 이 PR을 리뷰해줘.
- Reviewer 1: 동시성 이슈 (race condition, deadlock)
- Reviewer 2: 성능 (불필요한 할당, 락 경합)
- Reviewer 3: 테스트 커버리지"
```

---

### 주의 사항

- **토큰 비용**: 팀 규모에 비례하여 비용 증가 — 단순 작업은 단일 에이전트로 충분
- **실험적 기능**: 세션 재개, 종료 처리에 제한이 있을 수 있음
- **디스플레이 모드**: 기본은 in-process (단일 터미널), tmux/iTerm2에서는 split-pane 모드 사용 가능
