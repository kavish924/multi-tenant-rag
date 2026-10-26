# 015 — Isolation Tests & Red Teaming: Design

- **Spec ID:** 015 `RT` · **Milestone:** 5
- **Shared design:** [system-design.md §11](../../docs/system-design.md#11-testing-strategy), [diagrams §7](../../docs/diagrams/architecture.md#7-trust-boundaries-and-guard-layers)

## 1. Context and goals

"Guardrails without attacks are untested assumptions; attacks without guardrails are just a list
of problems." This spec is the attack half, wired to prove the guard half (`012`, `011`, `013`).

## 2. Scope

**In:** isolation suite, canary infrastructure, red-team suites per category, threat model doc,
report generation, CI wiring. **Out:** external pentest coordination.

## 3. Components

- `tests/isolation/` — `go test` (always on): parameterized cross-tenant probes over every
  id-bearing endpoint; forged-header/claim matrix; signed-URL replay.
- `tests/redteam/` — build tag `redteam`: fixture corpus generator (injection payloads, hidden
  text PDFs/DOCX, look-alike poisoning docs, bombs), capture harness (streams answers for all
  cases), canary scanner, per-category assertions.
- `docs/threat-model.md`, `docs/red-team-report.md` (generated + curated).

## 4. Data model changes

None (test tenants + fixtures only; cleanup via `013` purge).

## 5. API contract (internal)

Harness config `redteam.yaml`: categories on/off, LLM budget caps for judge-dependent checks,
capture dir. Report JSON: `{category, cases, passed, failed, findings[]}`.

## 6. Key flows

**Run:** provision test tenants + canary-seeded corpora → isolation suite (hermetic) →
red-team categories (LLM-dependent ones use the real provider with budget cap) → canary scan on
every captured answer → category report → CI gate on any failed category.

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Canary detected | scanner | fail run, attach traces/logs for the offending request |
| Budget cap hit (LLM categories) | meter | mark category `incomplete` (not pass) — fail CI unless explicitly waived |
| Flaky injection case | repeat rule (3×) | quarantine issue, never silent-skip |

## 8. Security and multi-tenancy

The suite itself is dangerous by design: runs only against the local compose stack (guard:
`REDTEAM_TARGET` must be localhost; hard refuse otherwise).

## 9. Testing strategy

Meta but deliberate: hermetic categories are deterministic `go test`; LLM categories capped and
repeat-ruled; every finding historically maps to a guardrail (`docs/red-team-report.md`).

## 10. Open questions

- Frequency of full vs fast sets in CI → **default:** isolation suite per PR; full red-team
  nightly + release.
