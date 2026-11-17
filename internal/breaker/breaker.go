// Package breaker is a per-provider circuit breaker (spec 011,
// QTA-REQ-005): after Threshold consecutive failures (errors or 429s) it
// opens and fails calls fast for OpenFor; then it lets one probe through
// (half-open) and closes again only if the probe succeeds.
package breaker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrOpen is returned while the circuit is open (or a half-open probe is
// already in flight). The ask path maps it to 503 provider_unavailable.
var ErrOpen = errors.New("breaker: provider circuit open")

// State is the breaker state.
type State int

// States.
const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	return [...]string{"closed", "open", "half_open"}[s]
}

// Defaults.
const (
	DefaultThreshold = 5
	DefaultOpenFor   = 30 * time.Second
)

// Breaker guards one provider. The zero value is not usable; use New.
type Breaker struct {
	Name      string
	Threshold int
	OpenFor   time.Duration
	Now       func() time.Time
	Log       *slog.Logger

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

// New builds a closed breaker.
func New(name string, threshold int, openFor time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if openFor <= 0 {
		openFor = DefaultOpenFor
	}
	return &Breaker{Name: name, Threshold: threshold, OpenFor: openFor}
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// State reports the current state (an expired open state reads half-open).
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.now().Sub(b.openedAt) >= b.OpenFor {
		return HalfOpen
	}
	return b.state
}

// Allow reserves a call. It returns ErrOpen while open, and while a
// half-open probe is in flight. Every nil return must be followed by Done.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if b.now().Sub(b.openedAt) < b.OpenFor {
			return ErrOpen
		}
		b.transition(HalfOpen)
		fallthrough
	case HalfOpen:
		if b.probing {
			return ErrOpen
		}
		b.probing = true
	}
	return nil
}

// Available reports whether a call would be allowed now, without
// reserving one (the ask path checks it before opening a stream).
func (b *Breaker) Available() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.now().Sub(b.openedAt) < b.OpenFor {
		return ErrOpen
	}
	if b.state == HalfOpen && b.probing {
		return ErrOpen
	}
	return nil
}

// Done records a call's outcome. Cancellation by the caller is neither a
// success nor a failure.
func (b *Breaker) Done(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	probe := b.probing
	b.probing = false
	if errors.Is(err, context.Canceled) {
		return
	}
	if err == nil {
		b.failures = 0
		if b.state != Closed {
			b.transition(Closed)
		}
		return
	}
	b.failures++
	if probe || b.failures >= b.Threshold {
		b.openedAt = b.now()
		b.transition(Open)
	}
}

func (b *Breaker) transition(to State) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	log := b.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("circuit breaker transition", "provider", b.Name, "from", from.String(), "to", to.String())
}

// Do runs fn under the breaker.
func (b *Breaker) Do(fn func() error) error {
	if err := b.Allow(); err != nil {
		return err
	}
	err := fn()
	b.Done(err)
	return err
}
