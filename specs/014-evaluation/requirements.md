# 014 — Evaluation Harness: Requirements

- **Spec ID:** 014 `EVA`
- **Status:** Draft
- **Milestone:** 5 — Release-quality capstone
- **Depends on:** `specs/007`, `008`, `009`
- **Shared context:** `docs/system-design.md §8` (evaluation)

## Overview

Without a test set, "better retrieval" is a feeling. A small golden dataset plus a harness that
runs on every change: retrieval metrics (recall@k, MRR, nDCG), answer metrics (faithfulness,
correctness), citation validity, and refusal behavior — with thresholds that gate regressions.

## User stories

- **EVA-US-1:** As a developer, I want `make eval` to score my changes against the golden set so quality regressions surface in CI.
- **EVA-US-2:** As a platform operator, I want tuning knobs (chunk size, rerank on/off, k) evaluated comparably so decisions are data-driven.
- **EVA-US-3:** As a reviewer, I want LLM-judge scores spot-checkable so we don't trust judges blindly.

## Requirements

### Functional

- **EVA-REQ-001:** `tests/evaluation/golden.jsonl`: `{"tenant", "question", "relevant_chunks": [ids], "refusal": bool, "reference_answer": str?}` — ≥ 50 cases across ≥ 2 tenants, incl. lexical-only, semantic-only, multi-hop, and out-of-scope questions.
- **EVA-REQ-002:** Harness (`cmd/eval` + `make eval`): boots the stack (compose profile), ingests fixture corpus, runs each case, computes metrics.
- **EVA-REQ-003:** Retrieval metrics: recall@k (k=8), MRR, nDCG@10 vs labelled chunks.
- **EVA-REQ-004:** Answer metrics: faithfulness to retrieved context + correctness vs reference — LLM-as-judge with structured rubric; **manual spot-check protocol documented** (sample of judged outputs reviewed by a human each run).
- **EVA-REQ-005:** Citation metric: every emitted marker resolves to a real, authorized chunk that supports the sentence (sentence-support check is judge-assisted, resolution check is deterministic).
- **EVA-REQ-006:** Refusal metric: `refusal: true` cases are declined (insufficient-context path), `false` cases are answered.
- **EVA-REQ-007:** Thresholds file (`tests/evaluation/thresholds.yaml`) gates CI: any metric below threshold fails the build with a diff report vs last run.
- **EVA-REQ-008:** A/B mode: two configurations (e.g. rerank on/off, chunk 400 vs 700) evaluated in one run with a comparison table artifact.

### Non-functional

- **EVA-NFR-001:** Full run ≤ 15 min in CI with mocked/budgeted LLM judging (judge calls capped; deterministic retrieval metrics uncapped).

## Acceptance criteria (EVA map)

- **EVA-AC-001:** WHEN `make eval` runs, THE SYSTEM SHALL produce `eval-report.json` + human-readable summary with all metric values per case and aggregate.
- **EVA-AC-002:** WHEN any aggregate metric falls below its threshold, THE SYSTEM SHALL exit non-zero and print the offending metrics.
- **EVA-AC-003:** WHEN a case expects refusal and the system answers from model memory, THE SYSTEM SHALL fail that case (refusal violation).
- **EVA-AC-004:** WHEN a citation marker fails resolution or support, THE SYSTEM SHALL count it as a citation failure and reflect it in the aggregate.
- **EVA-AC-005:** WHEN A/B mode runs, THE SYSTEM SHALL emit a side-by-side table artifact for the two configs.
- **EVA-AC-006:** WHEN a judge is unavailable, THE SYSTEM SHALL still compute and report all deterministic metrics (retrieval + citation resolution + refusal) and mark answer metrics `skipped`.

## Out of scope

- Continuous online evaluation from production traffic; annotation tooling UI.
