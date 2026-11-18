# Evaluation harness

Spec: [014](../../specs/014-evaluation/requirements.md). Code:
[`internal/evaluation`](../../internal/evaluation) and [`cmd/eval`](../../cmd/eval).

## Files

| File | What it is |
| ---- | ---------- |
| `golden.jsonl` | 52 cases over two tenants (`acme`, `globex`): lexical, semantic, multi-hop, out-of-scope and cross-tenant probes. 12 are tagged `fast`. |
| `corpus/<tenant>/*.md` | Fixture documents. Each file is exactly one chunk, so its label is `<tenant>/<file>#0` (a unit test enforces this). |
| `thresholds.yaml` | The CI gate. |
| `judge-rubric.md` | The answer-grading rubric, for humans. |

## Running

The harness needs a PostgreSQL 17 + pgvector database it may migrate (an
admin DSN). In Cloud Shell use the compose stack; in Cloud Build use the
pipeline's Postgres container.

```bash
make up
DATABASE_URL=postgres://postgres:postgres@localhost:5432/app?sslmode=disable make eval
```

`make eval` runs `go run ./cmd/eval run`:

1. migrates the database and creates a login role that is a member of `app`
   only, so retrieval runs under forced RLS exactly as in production;
2. creates two fresh tenants for the run and ingests the corpus through the
   real pipeline (extraction, chunking, embedding, indexing);
3. for each case: runs the hybrid search (retrieval metrics) and asks through
   the real `/ask` handler, guards included (refusal and citation metrics);
4. optionally sends answered cases to the judge (answer metrics);
5. writes `eval-report.json` and `eval-summary.md`, compares the aggregates
   with `thresholds.yaml`, and exits 1 if any gated metric is out of bounds;
6. purges the run's tenants with the spec 013 purge job.

Useful flags: `--fast` (the 12 fast cases), `--baseline previous.json`
(adds Δ columns), `--judge none|generator`, `--judge-max-calls N`,
`--rerank`, `--top-k`, `--keep` (do not purge).

A/B: `go run ./cmd/eval ab --b-rerank` runs the default configuration (A)
and the modified one (B) on separate tenants and writes `eval-ab.md` with
both reports. Flags prefixed `--b-` change config B only.

## Metrics

| Metric | How | Gated |
| ------ | --- | ----- |
| `recall_at_8`, `mrr`, `ndcg_at_10` | labelled chunks vs the ranked search results (binary relevance) | always |
| `citation_validity` | distinct `[n]` markers in the answer that resolved to a citation of the case tenant's chunk | always |
| `cross_tenant_leaks` | results or citations from another tenant (must be 0) | always |
| `refusal_accuracy` | refusal cases declined with no citations; others answered | real generator only |
| `faithfulness`, `correctness`, `citation_support` | LLM judge, rubric in `judge-rubric.md` | real generator + judge |

Without a judge (or when it fails, or its call budget runs out) the answer
metrics are `skipped` and the deterministic ones still gate the run
(EVA-AC-006).

## Manual spot-check protocol (EVA-REQ-004)

LLM judges are useful but not trusted blindly. Every run that used a judge
puts a random sample of five judged cases at the end of `eval-summary.md`.

1. The person who merges a change that moves an answer metric by more than
   0.2 reviews the sample, using `judge-rubric.md`.
2. For each case, tick whether you agree with the judge's two scores
   (within one point counts as agreement).
3. Record the agreement rate in the PR description.
4. If agreement is below 4 of 5 in two consecutive runs, the judge prompt or
   model is revisited before its scores are used for any tuning decision.

## Tuning decisions

Record each decision made with the harness (A/B report linked, the metrics
that decided it) in `docs/evaluation-decisions.md`.
