# 014 — Evaluation Harness: Design

- **Spec ID:** 014 `EVA` · **Milestone:** 5
- **Shared design:** [system-design.md §8](../../docs/system-design.md#8-evaluation)

## 1. Context and goals

Convert quality from opinion to measurement. The harness is the arbiter for tuning knobs
(chunk size, rerank, k) and the regression gate for CI.

## 2. Scope

**In:** golden set format + seed content, `cmd/eval`, metrics, thresholds, CI wiring, A/B mode.
**Out:** production telemetry-driven eval; human annotation tooling.

## 3. Components

- `tests/evaluation/`: `golden.jsonl`, `corpus/` (fixture docs incl. the labelled chunks),
  `thresholds.yaml`, `judge-rubric.md`.
- `cmd/eval`: modes `run`, `ab`, `report`.
- Judge: `llm.Generator` with structured-output rubric (faithfulness 1–5, correctness 1–5 +
  rationale) — provider-agnostic via existing interfaces.

## 4. Data model changes

None (eval creates its own test tenants; cleans up via `013` purge — dogfooding).

## 5. API contract (internal)

`eval run --config X --out report.json`; exit code policy per EVA-AC-002. Report schema:
`{meta, per_case[], aggregates:{recall_at_8, mrr, ndcg_at_10, faithfulness, correctness,
citation_validity, refusal_accuracy}}`.

## 6. Key flows

**Run:** compose-up profile `eval` → migrations → ingest corpus (real pipeline) → for each case:
search → answer (stream assembled) → deterministic metrics inline; judge calls batched at end →
aggregate → compare thresholds → write report + summary.

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Stack boot failure | health checks | abort with clear error |
| Judge unavailable / cap hit | errors/budget meter | skip answer metrics, mark skipped (EVA-AC-006) |
| Corpus drift (chunk ids changed) | missing relevant ids | eval warns; ids are stable by design (ING-REQ-004) |

## 8. Security and multi-tenancy

Golden cases span two tenants and include cross-tenant probes (imports `015` expectations:
zero leakage is itself an eval metric). Judge prompts contain fixture content only.

## 9. Testing strategy

- Unit: metric math against hand-computed vectors (recall/MRR/nDCG).
- Integration: tiny 5-case set end-to-end in CI (fast); full set nightly.
- Gate: thresholds fail-path test (EVA-AC-002).

## 10. Open questions

- Judge model choice + cost cap → **default:** same provider as generation, monthly judge budget
  flagged in config; spot-check protocol guards over-trust.
