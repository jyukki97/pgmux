## Git 워크플로우

모든 Task는 아래 순서로 진행한다.

```
Issue 등록 → 브랜치 생성 → 작업 → PR 생성 → 리뷰 → PR 머지
```

---

### 상세 흐름

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

---

### 브랜치 전략

```
main
 ├── feat/1-project-setup
 ├── feat/2-config-parsing
 ├── feat/3-connection-pool-struct
 ├── ...
 └── feat/N-feature-name
```
