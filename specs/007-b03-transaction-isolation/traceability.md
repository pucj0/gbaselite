# B03 FR/SC Traceability

**Status**: 设计级映射；源码路径为候选、真实 test names 和通过状态要等实现后补齐，不可将本表当运行结果。

| Requirements | Implementation Phases | Acceptance scenarios | Gates |
|---|---|---|---|
| FR-001–008 | P2 | M01–M07,M44 | SC-001 |
| FR-009,035 | P3,P4 | M08–M12 | SC-003 |
| FR-010 | P3,P5 | M13–M16 | SC-002 |
| FR-011,017 | P7 | M29–M35 | SC-004 |
| FR-012,020,021,033 | P6 | M21–M28 | SC-005 |
| FR-013 | P4,P6 | M12,M21–M27,M42 | SC-003,SC-005 |
| FR-014,018,019 | P3,P5,P7 | M17–M20,M33,M42 | SC-006 |
| FR-015 | P3–P7 | M11,M20,M26,M34,M42 | SC-003,SC-006 |
| FR-016,032 | P3–P8 | M37–M41 | SC-007 |
| FR-022,023 | P5–P8 | M26,M39–M41 | SC-007 |
| FR-024,027 | P2,P3,P6,P7 | M06,M07,M27,M36 | SC-008 |
| FR-025,026 | P1,P3,P7,P8 | M36–M38,M43 | SC-008,SC-010 |
| FR-028,029,030 | P1,P8 | M01–M44 | SC-009,SC-010 |
| FR-031,034 | P2–P9 | M06,M27,M36,M44 | SC-008,SC-011 |
| FR-036 | P1,P8,P9 | M41–M44 | SC-007,SC-011 |

## Exact Task ID ranges

| Phase | Task IDs |
|---|---|
| P1 — Phase 1: Setup & Architectural Proof | T001–T008 |
| P2 — Phase 2: SQL Grammar & Session State | T009–T018 |
| P3 — Phase 3: Read View Foundation & GC | T019–T029 |
| P4 — Phase 4: REPEATABLE READ | T030–T036 |
| P5 — Phase 5: READ COMMITTED | T037–T046 |
| P6 — Phase 6: SERIALIZABLE | T047–T056 |
| P7 — Phase 7: READ UNCOMMITTED | T057–T065 |
| P8 — Phase 8: End-to-End, Replication & MySQL Parity | T066–T076 |
| P9 — Phase 9: Documentation, Traceability & RC Gate | T077–T083 |

## Success criteria

| SC | Required proof |
|---|---|
| SC-001 | M01–M07,M44 + P2 protocol tests |
| SC-002 | M13–M16 + single statement view pin/release |
| SC-003 | M08–M12 + B02 locked current/read proof |
| SC-004 | M29–M36 + stable completed-staged ownership test |
| SC-005 | M21–M28 + range predicates & write-skew no dual invalid commits |
| SC-006 | M17–M20,M33 + per-dependency committed baseline validation |
| SC-007 | M37–M41 + 100 fixed schedules + -race and resource zero-leak checks |
| SC-008 | M06,M27,M36,M43 and README exact unsupported cases |
| SC-009 | M01–M44 differential report vs verified MySQL 8.4 |
| SC-010 | gofmt/go test/go vet + B01/B02/A05 regression + format/dataset unchanged |
| SC-011 | FR→contract→TID→real test evidence, unverified remains [ ] |
| SC-012 | deliver docs only, no branch push/code/deployment by this package |

## Task proof fill-in template

| Task ID | Commit SHA | Changed file(s) | Test name(s) | Command / exit code | Evidence file | Result |
|---|---|---|---|---|---|---|
| T001 | — | — | — | NOT RUN | — | [ ] |

所有通过态必须以同次 HEAD 证据填充。不要先填预期值，也不要从 B02 已完成数量推定 B03 实现状态。
