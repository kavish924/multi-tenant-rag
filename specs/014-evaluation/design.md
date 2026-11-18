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

**Run:** compose-up profile `eval` in Cloud Shell (or target the **test** project from Cloud Build; real Vertex AI either way for answer metrics) → migrations → ingest corpus (real pipeline) → for each case:
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

## 11. As built

- **Assets** (`tests/evaluation/`): 52 cases over `acme` (outdoor retailer) and `globex`
  (software company handbook): 13 lexical, 14 semantic, 8 multi-hop, 9 out-of-scope, 8
  cross-tenant probes; 12 tagged `fast`. Chunk ids are not stable, so labels are
  `<tenant>/<file>#<ordinal>`; every corpus file is exactly one chunk, and
  `TestLabelsMatchCorpus` runs the real extractor and chunker to prove each label exists.
- **`internal/evaluation`** (pure Go, unit-tested with a fake `System`): `LoadCases` /
  `Validate`, `RecallAtK` / `ReciprocalRank` / `NDCGAtK` (binary relevance), `Runner`,
  `Aggregate`, `Judge` (rubric prompt, strict JSON, call budget, per-call timeout),
  `LoadThresholds` / `Gate`, `Summary`, `ABTable`.
- **`cmd/eval`** `run | ab | report`: migrates the database, creates a login role that is a
  member of `app` only (so RLS applies), creates fresh tenants, ingests the corpus through the
  real pipeline (isolated extractor helper: `cmd/eval` doubles as it), then per case runs real
  hybrid search and the real `/ask` handler (guards included) through an in-process request,
  parsing the SSE stream. Finally it purges the tenants with the 013 purge job and deletes
  them. Exit 0 / 1 (gate failed) / 2 (harness error).
- **Metrics:** citation validity = distinct `[n]` markers in the answer that resolved to a
  citation of the case tenant's chunk; refusal accuracy = refusal cases declined with no
  citations, other cases answered; `cross_tenant_leaks` = results or citations labelled with
  another tenant (gated at 0). The judge also returns `citations_supported` (judge-assisted
  support check).
- **Gate:** `thresholds.yaml` has `min`, `max` and `requires_real_generator`. With the fake
  generator, refusal and answer metrics are reported but not gated (the fake always answers
  from source 1); skipped metrics (no judge, judge errors, budget exhausted) never fail the
  gate. Starting thresholds are conservative for the fake embedder; calibrate after the first
  run (`--baseline` adds Δ columns).
- **Spot check:** the summary samples five judged cases for manual review; the protocol is in
  `tests/evaluation/README.md`.
- **CI:** `make eval`; Cloud Build step `eval` runs the fast set on its own database in the
  pipeline's Postgres container; `_EVAL_SET=full` for the nightly trigger.
