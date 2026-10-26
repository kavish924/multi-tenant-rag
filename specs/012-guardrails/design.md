# 012 — Guardrails: Design

- **Spec ID:** 012 `GRD` · **Milestone:** 5
- **Shared design:** [system-design.md §10](../../docs/system-design.md#10-guardrails-defense-in-depth) (layer table + `internal/guard/guard.go` skeleton)

## 1. Context and goals

Defense in depth: each layer assumes the previous one can fail. This spec turns the layer table
into a composable, fail-closed pipeline with named stages, metrics, and tests.

## 2. Scope

**In:** `guard` package + all six layer stages, ask-path wiring, metrics.
**Out:** provider quotas (`011` owns math; integration only), citation resolver mechanics (`008`).

## 3. Components

```text
internal/guard/
├── guard.go        # Verdict, Guard, Run (fail closed)
├── input.go        # length cap, probing escalation
├── retrieval.go    # hygiene assertions (tenant purity, deleted, doc cap)
├── prompt.go       # delimiter writer, token budget assembler
├── tools.go        # allowlist registry (empty by default = deny all)
├── output.go       # citation assertion hook, redaction patterns
└── faithfulness.go # optional LLM check (timeout, warn-only config)
```

Pipeline placements: **pre-retrieval** (input), **post-search** (retrieval hygiene), **prompt
build** (assembly), **post-generation, pre-emit** (output stages on the *complete* answer for
citation/redaction; streaming buffers behind a small window — see §6).

## 4. Data model changes

None. Metrics only (`guard_verdicts_total{stage,verdict}`).

## 5. API contract (internal)

```go
type Verdict int
const (Allow Verdict = iota; Redact; Block)
type Guard interface {
    Name() string
    Check(ctx context.Context, text string) (Verdict, string, error)
}
func Run(ctx context.Context, text string, stages ...Guard) (Verdict, string, error)
```

`ErrBlocked` sentinel mapped by the ask handler to the refusal SSE event.

## 6. Key flows

**Ask with guards:** input guards → retrieve → hygiene assert → budget assembler builds prompt →
LLM stream buffered in a bounded ring (last-k tokens) while citation/redaction stages evaluate
on near-final text → on Block, emit refusal event and discard buffer; on Redact, stream the
cleaned tail; on Allow, flush. Rationale: output guards need enough text to be meaningful; the
buffer trades a few hundred ms for safety, configurable.

**Fail closed:** `Run` maps any `err` to `(Block, "", err)`; handler converts to refusal +
`guard_verdicts_total` increment (GRD-AC-001/002).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Guard panic | recover in `Run` | Block + log (never stream partial) |
| Faithfulness check timeout | ctx deadline | Block (or warn-only per rollout config) |
| Hygiene violation | post-search assert | Block + alert (bug signal, GRD-AC-004) |
| Redaction impossible mid-stream | config | Block whole answer |

## 8. Security and multi-tenancy

This spec *is* the defense-in-depth implementation. Core rule enforced structurally: no
privileged tools exist to be talked into (GRD-REQ-005 — deny-by-default allowlist). Tenant
purity is asserted, not assumed (GRD-AC-004).

## 9. Testing strategy

- Unit: `Run` semantics (Block-first-wins, Redact chaining, error ⇒ Block, panic ⇒ Block);
  pattern redaction table; budget assembler ordering.
- Integration: ask-path with each stage triggered; refusal SSE on Block (GRD-AC-002/006/008).
- Security (with `015`): direct + indirect injection suites (GRD-AC-009), citation forgery,
  system-prompt extraction attempts.

## 10. Open questions

- Streaming granularity vs output-guard safety → **default:** bounded buffer (≈256 tokens) +
  configurable; document trade-off in README.
