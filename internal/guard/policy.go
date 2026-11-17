package guard

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
)

// Policy is the guard configuration for the ask path (global config first;
// per-tenant configuration is out of scope).
type Policy struct {
	MaxQuestionRunes int // default retrieval.MaxQuestionRunes
	Probing          *Probing
	PerDocumentCap   int // asserted after search; default retrieval.DefaultPerDocumentCap

	OutputMode   OutputMode // GUARD_OUTPUT_MODE: windowed (default) or strict
	WindowBytes  int        // windowed buffer size; default DefaultWindowBytes
	RedactEmails bool

	Faithfulness FaithfulnessMode // off (default), warn or block
	Judge        llm.Generator
	JudgeTimeout time.Duration

	Log *slog.Logger

	once     sync.Once
	redactor *Redactor
}

// DefaultPolicy is the policy used when the handler is given none.
func DefaultPolicy() *Policy {
	return &Policy{Probing: NewProbing(0, 0), OutputMode: Windowed}
}

func (p *Policy) init() {
	p.once.Do(func() { p.redactor = NewRedactor(p.RedactEmails) })
}

// InputStages run before retrieval (GRD-REQ-002).
func (p *Policy) InputStages() []Guard {
	max := p.MaxQuestionRunes
	if max <= 0 {
		max = retrieval.MaxQuestionRunes
	}
	return []Guard{Length{MaxRunes: max}, ControlChars{}}
}

// DocCap is the asserted per-document cap.
func (p *Policy) DocCap() int {
	if p.PerDocumentCap > 0 {
		return p.PerDocumentCap
	}
	return retrieval.DefaultPerDocumentCap
}

// NewOutput builds the streaming output filter for one answer:
// redaction, then system-prompt leak detection.
func (p *Policy) NewOutput(leak *LeakDetector) *Output {
	p.init()
	stages := []Guard{p.redactor}
	if leak != nil {
		stages = append(stages, leak)
	}
	return NewOutput(p.OutputMode, p.WindowBytes, stages...)
}

// FinalStages run over the complete answer (the optional faithfulness
// check); onResult receives the judge label for metrics.
func (p *Policy) FinalStages(sources []retrieval.Hit, onResult func(label string, err error)) []Guard {
	if p.Faithfulness == "" || p.Faithfulness == FaithfulnessOff || p.Judge == nil || len(sources) == 0 {
		return nil
	}
	return []Guard{&Faithfulness{Judge: p.Judge, Sources: sources, Mode: p.Faithfulness, Timeout: p.JudgeTimeout, OnResult: onResult}}
}

// Observe records a decision (GRD-REQ-010): guard_verdicts_total{stage,
// verdict,tenant_id} for every Block and Redact, a structured log without
// any text, and a probing strike for blocks the user caused.
func (p *Policy) Observe(ctx context.Context, tenantID, userID uuid.UUID, d Decision) {
	if d.Verdict == Allow {
		return
	}
	observability.M().GuardVerdict(ctx, tenantID, d.Stage, d.Verdict.String())
	log := p.log()
	attrs := []any{"tenant_id", tenantID, "stage", d.Stage, "verdict", d.Verdict.String(), "reason", d.Reason}
	switch {
	case d.Stage == StageRetrievalHygiene:
		// An invariant violation is a bug, not user behaviour: alert.
		log.ErrorContext(ctx, "ALERT guard: retrieval hygiene violation", append(attrs, "err", d.Err)...)
	case d.Err != nil:
		log.ErrorContext(ctx, "guard failed closed", append(attrs, "err", d.Err)...)
	default:
		log.WarnContext(ctx, "guard verdict", attrs...)
	}
	if d.Blocked() && userID != uuid.Nil && d.Stage != StageRetrievalHygiene && d.Stage != StageBudgetProvider {
		p.Probing.Record(tenantID, userID)
	}
}

func (p *Policy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}
