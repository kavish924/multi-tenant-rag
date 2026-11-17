package guard

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Stage names (metric label values; bounded).
const (
	StageInputLength      = "input.length"
	StageInputControl     = "input.control_chars"
	StageInputProbing     = "input.probing"
	StageRetrievalHygiene = "retrieval.hygiene"
	StageBudgetProvider   = "budget.provider"
	StageOutputRedact     = "output.redact"
	StageOutputLeak       = "output.system_prompt_leak"
	StageOutputFaithful   = "output.faithfulness"
)

// Length blocks empty or over-long questions (GRD-AC-003). The ask handler
// turns this Block into a 400 before retrieval.
type Length struct{ MaxRunes int }

// Name implements Guard.
func (Length) Name() string { return StageInputLength }

// Check implements Guard.
func (l Length) Check(_ context.Context, text string) (Verdict, string, error) {
	t := strings.TrimSpace(text)
	if t == "" || !utf8.ValidString(t) || utf8.RuneCountInString(t) > l.MaxRunes {
		return Block, "", nil
	}
	return Allow, text, nil
}

// ControlChars blocks questions with control or invisible format
// characters other than ordinary whitespace: the usual carriers of hidden
// instructions (zero-width text, bidi overrides, NUL).
type ControlChars struct{}

// Name implements Guard.
func (ControlChars) Name() string { return StageInputControl }

// Check implements Guard.
func (ControlChars) Check(_ context.Context, text string) (Verdict, string, error) {
	for _, r := range text {
		if r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return Block, "", nil
		}
	}
	return Allow, text, nil
}

// Probing escalates repeated guard blocks from one user in one tenant
// (GRD-REQ-002): after Limit blocks within Window the user gets 429 until
// the window rolls over. Counts are per API instance, which is enough to
// slow probing down; the per-tenant quotas (011) bound total spend.
type Probing struct {
	Limit  int
	Window time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	windows map[probeKey]*probeWindow
}

type probeKey struct{ tenant, user uuid.UUID }

type probeWindow struct {
	start time.Time
	count int
}

// NewProbing builds a tracker; limit <= 0 uses 10 per minute.
func NewProbing(limit int, window time.Duration) *Probing {
	if limit <= 0 {
		limit = 10
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Probing{Limit: limit, Window: window, windows: map[probeKey]*probeWindow{}}
}

func (p *Probing) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Record counts one block for the user.
func (p *Probing) Record(tenant, user uuid.UUID) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	k := probeKey{tenant, user}
	w := p.windows[k]
	if w == nil || now.Sub(w.start) >= p.Window {
		w = &probeWindow{start: now}
		p.windows[k] = w
	}
	w.count++
	if len(p.windows) > 10_000 { // bound memory: drop expired windows
		for key, win := range p.windows {
			if now.Sub(win.start) >= p.Window {
				delete(p.windows, key)
			}
		}
	}
}

// Throttled reports whether the user is over the limit, and for how long.
func (p *Probing) Throttled(tenant, user uuid.UUID) (bool, time.Duration) {
	if p == nil {
		return false, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.windows[probeKey{tenant, user}]
	if w == nil {
		return false, 0
	}
	left := p.Window - p.now().Sub(w.start)
	if left <= 0 || w.count < p.Limit {
		return false, 0
	}
	return true, left
}
