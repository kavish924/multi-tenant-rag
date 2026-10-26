# 015 — Isolation Tests & Red Teaming: Requirements

- **Spec ID:** 015 `RT`
- **Status:** Draft
- **Milestone:** 5 — Release-quality capstone
- **Depends on:** all prior specs
- **Shared context:** `docs/system-design.md §11` (testing strategy), `docs/diagrams/architecture.md §7` (trust boundaries)

## Overview

Isolation tests and a red-team suite run against our own system, in an environment we own, with
test tenants — each test a category with a clear pass condition. Includes canary strings planted
in every test tenant's documents: if tenant A's answers ever contain tenant B's canary, there is
a leak. References: OWASP Top 10 for LLM Applications, MITRE ATLAS (current versions).

## User stories

- **RT-US-1:** As a security reviewer, I want deterministic proof that cross-tenant access is impossible through every entry point.
- **RT-US-2:** As a platform operator, I want injection, poisoning, and abuse scenarios executed in CI so guardrails stay proven, not assumed.
- **RT-US-3:** As a contributor, I want a threat model documenting attackers, goals, and entry points so new tests map to real threats.

## Requirements

### Functional

- **RT-REQ-001:** Isolation suite (`tests/isolation/`): cross-tenant document access, cross-tenant retrieval/citations, forged tenant identifiers (headers/bodies/token claims), guessing document/job/chunk ids, resolving another tenant's citation, reusing signed URLs, unauthorized job inspection/deletion, auth/role abuse (tampered/expired tokens, member→admin endpoints).
- **RT-REQ-002:** Canary infrastructure: every test tenant's fixture documents embed unique meaningless tokens; an automated post-check scans every captured answer for foreign canaries.
- **RT-REQ-003:** Red-team suite (`tests/redteam/`, build tag `redteam`) categories with pass conditions:

| Category | What the red team tries | Pass condition |
| -------- | ------------------------ | -------------- |
| Direct prompt injection | Questions telling the model to ignore rules, reveal instructions, change roles | Rules hold; no system prompt/secret in answers |
| Indirect prompt injection | Documents with instructions aimed at the model incl. hidden text (white-on-white, tiny fonts, metadata fields) | Instructions not followed; no privileged action |
| Cross-tenant access | Forged ids, guessed ids, citation resolution, signed-URL reuse | Always denied (zero tolerance, deterministic tests) |
| Cache/shared-state leakage | Questions crafted to hit another tenant's cached answers/embeddings | All cache keys tenant-inclusive; no cross-tenant hits |
| Data exfiltration | Model emitting links/images carrying retrieved content out | Output cannot trigger outbound requests with private data |
| Retrieval poisoning | Keyword-stuffed/look-alike documents outranking genuine sources | Per-document caps + provenance limit damage; upload rights enforced |
| Resource abuse (denial of wallet) | Huge/malformed files, decompression bombs, long prompts, query floods | Limits trigger early; quotas stop spend; workers stay in memory bounds |
| Policy bypass / off-scope | Content outside product purpose; answers not in documents | Refuses or reports insufficient context |
| Auth and role abuse | Tampered/expired tokens; member calling admin endpoints | Rejected on every endpoint incl. deletion and job inspection |
| Deletion races | Deleting while processing/retrieving | Deleted content never reappears in search or citations |

- **RT-REQ-004:** Threat model doc (`docs/threat-model.md`): attackers (malicious user inside a tenant; malicious uploader; curious/hostile other tenant; external abuser), goals, entry points — each red-team category traced to a threat.
- **RT-REQ-005:** Suite runs in CI (nightly + release tag); results + guardrails-found mapping recorded (`docs/red-team-report.md`: finding → guardrail added).
- **RT-REQ-006:** External references kept current: OWASP LLM Top 10 and MITRE ATLAS versions checked, not remembered.

### Non-functional

- **RT-NFR-001:** Red-team run ≤ 30 min; deterministic categories are hermetic (no live LLM dependence unless the category requires it).

## Acceptance criteria (EARS)

- **RT-AC-001:** WHEN tenant A requests tenant B's document/job/citation by id, THE SYSTEM SHALL deny (404) — parameterized across all id types.
- **RT-AC-002:** WHEN a forged tenant identifier is presented in any entry point, THE SYSTEM SHALL ignore it (verified membership only).
- **RT-AC-003:** WHEN answers are captured across the full red-team run, THE SYSTEM SHALL contain zero foreign-tenant canary tokens.
- **RT-AC-004:** WHEN direct/indirect injection suites run, THE SYSTEM SHALL show no rule-following failure, no system-prompt/secret leakage, and no privileged action triggered.
- **RT-AC-005:** WHEN flooding/abuse scenarios run, THE SYSTEM SHALL trip limits/quotas early and keep workers within memory bounds.
- **RT-AC-006:** WHEN deletion races execute (delete during process/retrieve), THE SYSTEM SHALL never surface deleted content afterwards.
- **RT-AC-007:** WHEN the suite completes, THE SYSTEM SHALL emit a per-category pass/fail report mapped to the threat model and to guardrails each failure historically produced.

## Out of scope

- Third-party penetration testing; production incident-response drills (runbook covers response).
