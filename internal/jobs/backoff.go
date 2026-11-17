package jobs

import (
	"math/rand/v2"
	"time"
)

// Backoff parameters (JOB-REQ-006): base · 2^attempts + uniform jitter in
// [0, base), capped at MaxBackoff.
const (
	BackoffBase = 5 * time.Second
	MaxBackoff  = 10 * time.Minute
)

// Backoff returns the delay before retrying a job that has failed after
// `attempts` attempts. jitter returns a value in [0, 1); nil uses math/rand.
func Backoff(attempts int, jitter func() float64) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if jitter == nil {
		jitter = rand.Float64 //nolint:gosec // jitter, not security
	}
	d := MaxBackoff
	// 5s · 2^7 = 640s already exceeds the cap; avoid shifting further.
	if attempts < 7 {
		d = BackoffBase << attempts
	}
	d += time.Duration(jitter() * float64(BackoffBase))
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return d
}
