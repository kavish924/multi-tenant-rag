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

**Limit of the streaming buffer — be explicit about it.** Only the buffered tail can be withheld.
Once a token leaves the buffer it has reached the client, so a Block decided later can stop the
stream and emit a `refusal` event (the client must discard what it rendered) but cannot un-send
earlier text. "Fail closed" therefore holds fully only for answers shorter than the buffer.
Two modes, selected per tenant/config (`GUARD_OUTPUT_MODE`):

| Mode | Behaviour | Use when |
| ---- | --------- | -------- |
| `windowed` (default) | ≈256-token buffer; late Block ⇒ stop + `refusal` event + client discards | latency matters; redaction/citation patterns are local |
| `strict` | buffer the **whole** answer, run output stages, then stream it | regulated tenants; zero tolerance for any unguarded byte |

Citation verification is unaffected either way: citations are emitted as separate events only
after the answer completes and are resolved server-side (`008`).

**Fail closed:** `Run` maps any `err` to `(Block, "", err)`; handler converts to refusal +
`guard_verdicts_total` increment (GRD-AC-001/002).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Guard panic | recover in `Run` | Block + log; stop stream + `refusal` (text already past the buffer cannot be recalled — see §6) |
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

- Streaming granularity vs output-guard safety → **default:** `windowed` mode, bounded buffer
  (≈256 tokens), with `strict` (whole-answer) mode available; document trade-off in README.

## 11. As built

- `guard.Evaluate` / `guard.Run`: stages in order; first Block wins and later stages never run;
  Redact output feeds the next stage; an error, a panic, an unknown verdict or an expired
  context is a Block (fail closed). `Decision` carries the stage name (bounded metric label) and
  a reason code, never text.
- **Input** (before retrieval): `Length` (1–2,000 runes, valid UTF-8) and `ControlChars`
  (control and invisible format characters such as zero-width and bidi overrides) → 400.
  `Probing`: 10 guard blocks per user per minute → 429 `abuse_suspected` (per API instance).
  Upload size and type are already re-checked inside `documents.Service.Upload`, below the HTTP
  handler (002), which is the defense-in-depth layer the requirement asks for.
- **Budget:** `ProviderAvailable` composes the 011 circuit breakers as a stage (→ 503); quotas
  stay in `quotas.Limiter` (→ 429), run in the same pre-retrieval position.
- **Retrieval hygiene:** `hydrate.sql` now returns each row's own `tenant_id` and deleted flag;
  `AssertHygiene` checks tenant purity, deleted exclusion, the per-document cap and duplicate
  chunks after search and rerank. A violation is refused (SSE `refusal`, stage
  `retrieval.hygiene`) before the model is called, and logged as `ALERT`.
- **Prompt:** delimiters and the context budget stay in `conversations.BuildPrompt` (008),
  highest-ranked first; a test asserts the system prompt matches no secret pattern.
- **Tools:** `guard.Tools` is an empty allowlist; `Authorize` requires an allowlisted name,
  schema validation (registration panics without it) and human confirmation for consequential
  tools. The generator interface has no tool calls, so an injected document has nothing to
  trigger.
- **Output, streaming:** `guard.Output` holds back a window (default 1,024 bytes ≈ 256 tokens,
  minimum 256) and runs `Redactor` (AWS/Google/GitHub/Slack/`sk-` keys, JWTs, private IPv4,
  emails optional) and `LeakDetector` (any 8-word run of the system prompt) every 128 new bytes.
  Text is released only right after a run with the full window behind it, so a pattern shorter
  than the window is redacted before any of it leaves. A private-key header blocks the answer
  (a key block can outlast the window). `strict` mode releases nothing until the final stages pass.
- **Output, final:** optional `Faithfulness` (`off` | `warn` | `block`): a judge model (the
  generation provider) answers `SUPPORTED` / `UNSUPPORTED` / `REFUSAL` within 1.5 s; in block
  mode an unsupported answer, a timeout, a judge error or an unparseable reply blocks.
- **Refusal:** SSE `event: refusal` `{"code":"guard_blocked","stage","message","discard"}`;
  `discard: true` means text was already released (windowed mode) and the client must remove it.
  A provider failure mid-stream discards the unreleased tail (it never passed the final checks).
- **Citations:** resolved server-side from this request's sources (008); an answer with sources
  but no citation increments `uncited_answers_total` (soft signal).
- **Telemetry:** `guard_verdicts_total{stage,verdict,tenant_id}` for every Block and Redact
  (`stage` and `verdict` added to the metric label allowlist) and a structured log without text.
- **Config:** `GUARD_OUTPUT_MODE` (windowed), `GUARD_WINDOW_TOKENS` (256), `GUARD_REDACT_EMAILS`
  (false), `GUARD_FAITHFULNESS` (off), `GUARD_FAITHFULNESS_TIMEOUT` (1.5s), `GUARD_PROBE_LIMIT` (10).
- **Overhead:** `BenchmarkDeterministicStages` (input stages + streaming filter over a ~2 KB
  answer fed word by word): ≈ 2.2 ms per answer on a laptop, within the 20 ms budget.
