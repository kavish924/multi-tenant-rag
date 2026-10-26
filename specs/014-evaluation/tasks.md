# 014 — Evaluation: Tasks

## Group 1: Assets

- [ ] 1.1 Seed `golden.jsonl` (≥ 50 cases, 2+ tenants, lexical/semantic/multi-hop/refusal mix) + fixture corpus + `thresholds.yaml` + judge rubric. (EVA-REQ-001)

## Group 2: Harness

- [ ] 2.1 `cmd/eval run`: compose profile boot, corpus ingest, case execution. (EVA-REQ-002, EVA-AC-001)
- [ ] 2.2 Deterministic metrics: recall@8, MRR, nDCG@10 (+ unit tests on hand-computed vectors). (EVA-REQ-003)
- [ ] 2.3 Answer judging: batched LLM judge with rubric + manual spot-check protocol doc. (EVA-REQ-004)
- [ ] 2.4 Citation validity (deterministic resolution + judge-supported sentence check). (EVA-REQ-005, EVA-AC-004)
- [ ] 2.5 Refusal accuracy metric. (EVA-REQ-006, EVA-AC-003)
- [ ] 2.6 Threshold gate + report artifacts + non-zero exit path. (EVA-REQ-007, EVA-AC-002)
- [ ] 2.7 `ab` mode with comparison table. (EVA-REQ-008, EVA-AC-005)
- [ ] 2.8 Judge-unavailable degradation (deterministic metrics only). (EVA-AC-006)

## Group 3: CI and decisions

- [ ] 3.1 CI wiring: fast set on PR, full set nightly; report artifact upload.
- [ ] 3.2 First tuning decision recorded via A/B (e.g. rerank on/off) — evidence in docs. (RER-AC-005 tie-in)
