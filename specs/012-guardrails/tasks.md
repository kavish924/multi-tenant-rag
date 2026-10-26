# 012 — Guardrails: Tasks

## Group 1: Core pipeline

- [ ] 1.1 `guard.go`: Verdict, Guard, Run — Block-first-wins, Redact chaining, error ⇒ Block, panic recovery. (GRD-REQ-001, GRD-AC-001/002)

## Group 2: Stages

- [ ] 2.1 `input.go`: length cap (400) + probing escalation counter. (GRD-REQ-002, GRD-AC-003)
- [ ] 2.2 `retrieval.go`: hygiene assertions (tenant purity, deleted exclusion, doc cap) + Block+alert. (GRD-REQ-003, GRD-AC-004)
- [ ] 2.3 `prompt.go`: untrusted-data delimiters + token-budget assembler (highest-rank first). (GRD-REQ-004, GRD-AC-005)
- [ ] 2.4 `tools.go`: deny-by-default allowlist registry; docs stating privileged-action policy. (GRD-REQ-005)
- [ ] 2.5 `output.go`: citation assertion hook over `008` resolver + redaction pattern set + mid-stream policy. (GRD-REQ-006/007, GRD-AC-006/007)
- [ ] 2.6 `faithfulness.go`: optional LLM check with timeout + warn-only rollout config. (GRD-REQ-008, GRD-AC-008)
- [ ] 2.7 Budget/breaker composition with `011` (Block on breaker-open). (GRD-REQ-009)

## Group 3: Wiring and telemetry

- [ ] 3.1 Ask-path integration incl. bounded output buffer strategy (documented trade-off). (design §6)
- [ ] 3.2 `guard_verdicts_total{stage,verdict}` + structured (text-free) Block logs. (GRD-REQ-010)

## Group 4: Tests and verification

- [ ] 4.1 Pipeline semantics unit suite (all `Run` rules). (GRD-AC-001/002)
- [ ] 4.2 Stage-triggered integration tests end-to-end on the ask path. (GRD-AC-003–008)
- [ ] 4.3 Injection suite (direct + indirect, hidden text, metadata fields) — no leakage, no privileged action. (GRD-AC-009)
- [ ] 4.4 Overhead benchmark ≤ 20 ms deterministic stages. (GRD-NFR-001)
