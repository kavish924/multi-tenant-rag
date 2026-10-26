# 012 — Guardrails (Fail-Closed Pipeline): Requirements

- **Spec ID:** 012 `GRD`
- **Status:** Draft
- **Milestone:** 5 — Release-quality capstone
- **Depends on:** `specs/007`, `008`, `011`
- **Shared context:** `docs/system-design.md §10` (guardrail layers, core rule: **the model is not a security boundary**)

## Overview

A small pipeline of deterministic guard stages wrapping inputs and outputs: input limits,
retrieval hygiene, prompt-assembly hygiene, tool/action policy, output checks (citation
verification, refusal/groundedness, redaction), budget integration. **Fail closed:** a guard
error or timeout blocks the response.

## User stories

- **GRD-US-1:** As a platform operator, I want prompt-injected documents to be unable to trigger privileged actions.
- **GRD-US-2:** As a tenant member, I want answers whose citations always point at real passages I can view.
- **GRD-US-3:** As a compliance owner, I want secrets/PII patterns redacted from answers.
- **GRD-US-4:** As a developer, I want one `guard.Run` pipeline so stages compose and ordering is explicit.

## Requirements

### Functional

- **GRD-REQ-001:** `Verdict ∈ {Allow, Redact, Block}`; `Guard` interface (`Name`, `Check(ctx, text) (Verdict, cleaned string, err)`); `Run(ctx, text, stages...)` executes in order: first Block wins; Redact output feeds the next stage; **any error returns Block** (fail closed).
- **GRD-REQ-002 (Input):** question length cap, upload size/type re-checks at guard level (defense in depth beyond handlers), abuse detection on repeated probing (429 escalation after N rejections/minute).
- **GRD-REQ-003 (Retrieval hygiene):** per-document chunk cap (from `007`) and soft-delete exclusion are asserted post-search; violation ⇒ Block (bug indicator) + alert.
- **GRD-REQ-004 (Prompt assembly):** retrieved text delimited with explicit untrusted-data markers; context token budget enforced (drop lowest-ranked extra chunks over budget); no secrets/keys in the system prompt.
- **GRD-REQ-005 (Tools/actions):** no privileged tools are exposed to the model; any future tool goes through an allowlist + schema validation + tenant-bound credentials + human confirmation for consequential actions ("the model proposes; your code decides").
- **GRD-REQ-006 (Output — citation verification):** every emitted citation resolves to this request's retrieved records (`008` resolver) — unverifiable markers dropped; if the answer cites nothing while hits existed and the question implied grounding, flag `uncited_answer` metric (soft signal, not Block).
- **GRD-REQ-007 (Output — redaction):** regex/pattern stage redacts high-confidence secret and PII patterns (API keys, AWS-style keys, emails optional config, private IP ranges) before streaming to the client.
- **GRD-REQ-008 (Output — groundedness/refusal, probabilistic layer):** optional LLM-based check classifying answer vs context (faithful/unfaithful/unanswerable); configured `Block` for unfaithful ⇒ replace with refusal; measured as probabilistic (dashboard: flag rate + sampled audits).
- **GRD-REQ-009 (Budget integration):** `011` checks compose as the budget stage; breaker-open ⇒ Block with `provider_unavailable`.
- **GRD-REQ-010:** Every Block/Redact emits a metric event `{stage, verdict, tenant}` and a structured log (no document text).

### Non-functional

- **GRD-NFR-001:** Deterministic guard stages add ≤ 20 ms p95 combined.
- **GRD-NFR-002:** The optional LLM output check has a hard timeout (default 1.5 s) and its failure ⇒ Block (fail closed) unless configured `warn-only` during rollout.

## Acceptance criteria (EARS)

- **GRD-AC-001:** WHEN any guard stage returns an error or times out, THE SYSTEM SHALL Block the response (no partial output to the client).
- **GRD-AC-002:** WHEN a stage Blocks, THE SYSTEM SHALL not run later stages and the client receives a refusal, not the content.
- **GRD-AC-003:** WHEN a question exceeds the length cap, THE SYSTEM SHALL respond 400 before retrieval.
- **GRD-AC-004:** WHEN retrieved chunks violate hygiene invariants (foreign tenant, deleted doc), THE SYSTEM SHALL Block + alert (this indicates an internal bug, not user error).
- **GRD-AC-005:** WHEN the context exceeds the token budget, THE SYSTEM SHALL assemble the prompt from the highest-ranked chunks within budget.
- **GRD-AC-006:** WHEN an answer contains a pattern-classified secret, THE SYSTEM SHALL Redact it in streamed output (or Block if redaction is impossible mid-stream, per config).
- **GRD-AC-007:** WHEN a citation cannot be resolved to this request's hits, THE SYSTEM SHALL drop it from the emitted citations.
- **GRD-AC-008:** WHEN the optional faithfulness check classifies an answer unfaithful (Block mode), THE SYSTEM SHALL emit a refusal instead of the answer.
- **GRD-AC-009:** WHEN a document contains instructions to reveal the system prompt, THE SYSTEM SHALL NOT reveal it and no privileged action SHALL be triggerable from document content (indirect injection test).

## Out of scope

- Full DLP/PII detection ML; moderator UI; per-tenant guard configuration (global config first).
