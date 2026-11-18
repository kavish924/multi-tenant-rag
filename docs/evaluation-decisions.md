# Evaluation decisions

Tuning decisions made with the evaluation harness ([spec 014](../specs/014-evaluation/requirements.md),
[tests/evaluation](../tests/evaluation/README.md)). Each entry links the A/B report that
decided it and names the metrics that mattered. A knob stays at its default until an entry
here says otherwise.

| Date | Decision | Evidence | Deciding metrics |
| ---- | -------- | -------- | ---------------- |
| *pending* | Rerank on or off ([009](../specs/009-reranking/requirements.md) RER-AC-005) | `go run ./cmd/eval ab --b-rerank --judge generator` with the Vertex AI generator | recall@8, nDCG@10, faithfulness, added latency |

## How to add an entry

1. Run `go run ./cmd/eval ab` with the two configurations (flags prefixed `--b-` change
   configuration B), with the real embedder and generator and `--judge generator`.
2. Commit or attach `eval-ab.md`, and complete the spot check in it.
3. Add a row: the decision, a link to the report, and the metric deltas that justified it
   (and the latency/cost delta when the change adds a provider call).

The first decision (rerank on/off) needs the Vertex AI provider: with the deterministic fake
generator the LLM reranker always falls back to the fused order, so A and B are identical.
