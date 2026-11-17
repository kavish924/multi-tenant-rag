// Package guard is the fail-closed guardrail pipeline (spec 012). The model
// is not a security boundary: every hard guarantee here is deterministic
// code, and the optional model-based check (faithfulness) is an extra,
// probabilistic layer that also fails closed.
//
// Layers and where they run on the ask path:
//
//	input      before retrieval   length cap, control characters, probing escalation
//	retrieval  after search       tenant purity, deleted exclusion, per-document cap (asserted)
//	prompt     prompt build       untrusted-data delimiters + context budget (conversations.BuildPrompt)
//	tools      —                  none exist; the registry denies everything (tools.go)
//	output     while streaming    secret/PII redaction, system-prompt leak detection
//	           after the answer   citations resolved server-side (008), faithfulness (optional)
//	budget     before retrieval   quotas and circuit breakers (011), composed as a stage
package guard

import (
	"context"
	"errors"
	"fmt"
)

// Verdict is a stage's decision.
type Verdict int

// Verdicts, in increasing severity.
const (
	Allow Verdict = iota
	Redact
	Block
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Redact:
		return "redact"
	default:
		return "block"
	}
}

// Guard is one stage.
type Guard interface {
	Name() string
	// Check returns the verdict and the (possibly cleaned) text.
	Check(ctx context.Context, text string) (Verdict, string, error)
}

// ErrBlocked is wrapped by Decision.Err for a Block, so callers can map
// any block to a refusal.
var ErrBlocked = errors.New("guard: blocked")

// Decision is the outcome of a pipeline run.
type Decision struct {
	Verdict Verdict
	Text    string // cleaned text; empty on Block
	Stage   string // the stage that blocked or last redacted (names are specific)
	Reason  string // "blocked", "redacted", "guard_error" or "deadline"; never content
	Err     error  // the stage error that caused a fail-closed Block, if any
}

// Blocked reports a Block.
func (d Decision) Blocked() bool { return d.Verdict == Block }

// Evaluate runs stages in order (GRD-REQ-001): the first Block wins and
// later stages do not run (GRD-AC-002); a Redact's output feeds the next
// stage; an error or a panic is a Block (fail closed, GRD-AC-001); a
// cancelled or expired ctx is a Block too.
func Evaluate(ctx context.Context, text string, stages ...Guard) Decision {
	d := Decision{Verdict: Allow, Text: text}
	for _, g := range stages {
		if err := ctx.Err(); err != nil {
			return Decision{Verdict: Block, Stage: g.Name(), Reason: "deadline", Err: err}
		}
		v, out, err := check(ctx, g, d.Text)
		if err != nil {
			return Decision{Verdict: Block, Stage: g.Name(), Reason: "guard_error", Err: fmt.Errorf("guard %s: %w", g.Name(), err)}
		}
		switch v {
		case Allow:
		case Redact:
			d.Verdict, d.Text, d.Stage, d.Reason = Redact, out, g.Name(), "redacted"
		default: // Block, or any unknown verdict
			return Decision{Verdict: Block, Stage: g.Name(), Reason: "blocked"}
		}
	}
	return d
}

// Run is the spec's signature over Evaluate: (verdict, cleaned text, err),
// with err non-nil exactly when a stage failed.
func Run(ctx context.Context, text string, stages ...Guard) (Verdict, string, error) {
	d := Evaluate(ctx, text, stages...)
	return d.Verdict, d.Text, d.Err
}

func check(ctx context.Context, g Guard, text string) (v Verdict, out string, err error) {
	defer func() {
		if p := recover(); p != nil {
			v, out, err = Block, "", fmt.Errorf("panic: %v", p)
		}
	}()
	return g.Check(ctx, text)
}

// Func adapts a function to a Guard.
type Func struct {
	N string
	F func(ctx context.Context, text string) (Verdict, string, error)
}

// Name implements Guard.
func (f Func) Name() string { return f.N }

// Check implements Guard.
func (f Func) Check(ctx context.Context, text string) (Verdict, string, error) { return f.F(ctx, text) }
