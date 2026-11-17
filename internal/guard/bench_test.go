package guard

import (
	"context"
	"strings"
	"testing"
)

// GRD-NFR-001: all deterministic stages for one answer — input stages on
// the question, then the streaming filter (redaction + leak detection) fed
// a ~2 KB answer one word at a time, then Finish.
func BenchmarkDeterministicStages(b *testing.B) {
	ctx := context.Background()
	p := &Policy{}
	leak := NewLeakDetector(strings.Repeat("You answer questions using only the numbered sources you are given. ", 6))
	words := strings.Fields(strings.Repeat("Refunds are paid within five business days of the request [1]. ", 32))
	b.ReportAllocs()
	for b.Loop() {
		if d := Evaluate(ctx, "How long do refunds take?", p.InputStages()...); d.Blocked() {
			b.Fatal("blocked")
		}
		o := p.NewOutput(leak)
		for _, w := range words {
			if _, d := o.Write(ctx, w+" "); d.Blocked() {
				b.Fatal("blocked")
			}
		}
		o.Finish(ctx)
	}
}
