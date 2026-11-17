package retrieval

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func hit(doc uuid.UUID, vrank, trank int) Hit {
	return Hit{ChunkID: uuid.New(), DocumentID: doc, VectorRank: vrank, TextRank: trank}
}

// RET-AC-007: k math, and both legs beat one leg.
func TestRRF(t *testing.T) {
	if got := RRF(60, 1); math.Abs(got-1.0/61) > 1e-12 {
		t.Errorf("RRF(60,1) = %v", got)
	}
	if RRF(60, 0) != 0 {
		t.Error("absent leg must contribute 0")
	}
	doc := uuid.New()
	both := hit(doc, 10, 10)
	vecOnly := hit(doc, 1, 0)
	textOnly := hit(doc, 0, 1)
	out := Fuse([]Hit{vecOnly, textOnly, both}, 60, 0, 10)
	if out[0].ChunkID != both.ChunkID {
		t.Fatalf("both-legs chunk ranked %v", out)
	}
	if want := 2.0 / 70; math.Abs(out[0].Score-want) > 1e-12 {
		t.Errorf("score %v, want %v", out[0].Score, want)
	}
	// The same chunk scores higher with a second leg than with either alone.
	if RRF(60, 1)+RRF(60, 1) <= RRF(60, 1) {
		t.Error("second leg did not add score")
	}
	for i, h := range out {
		if h.Rank != i+1 {
			t.Errorf("rank %d at %d", h.Rank, i)
		}
	}
}

// RET-AC-003.
func TestFusePerDocumentCap(t *testing.T) {
	big, small := uuid.New(), uuid.New()
	var cands []Hit
	for i := 1; i <= 20; i++ {
		cands = append(cands, hit(big, i, i))
	}
	cands = append(cands, hit(small, 30, 0), hit(small, 31, 0))
	out := Fuse(cands, 60, 5, 50)
	n := map[uuid.UUID]int{}
	for _, h := range out {
		n[h.DocumentID]++
	}
	if n[big] != 5 || n[small] != 2 || len(out) != 7 {
		t.Errorf("per-doc counts %v, total %d", n, len(out))
	}
	// Cap keeps the best chunks of the dominant document.
	if out[0].VectorRank != 1 || out[4].VectorRank != 5 {
		t.Errorf("kept %d..%d", out[0].VectorRank, out[4].VectorRank)
	}
}

// RET-AC-008 and ordering.
func TestFuseLimitAndOrder(t *testing.T) {
	doc := uuid.New()
	cands := []Hit{hit(doc, 3, 0), hit(doc, 0, 2), hit(doc, 1, 1), {ChunkID: uuid.New(), DocumentID: doc}}
	out := Fuse(cands, 60, 0, 100)
	if len(out) != 3 {
		t.Fatalf("got %d hits, want 3 (leg-less candidate dropped)", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].Score > out[i-1].Score {
			t.Errorf("not sorted: %v", out)
		}
	}
	if got := Fuse(cands, 60, 0, 2); len(got) != 2 {
		t.Errorf("limit 2 returned %d", len(got))
	}
	if got := Fuse(nil, 60, 5, 8); len(got) != 0 {
		t.Errorf("empty input returned %v", got)
	}
	// Equal scores break ties deterministically (vector rank first).
	a, b := hit(doc, 2, 0), hit(doc, 0, 2)
	for i := 0; i < 5; i++ {
		if out := Fuse([]Hit{b, a}, 60, 0, 2); out[0].ChunkID != a.ChunkID {
			t.Fatal("tie not broken by vector rank")
		}
	}
}

type panicEmbedder struct{}

func (panicEmbedder) Model() string { return "panic" }
func (panicEmbedder) Dim() int      { return 768 }
func (panicEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	panic("provider called for an invalid question")
}

// RET-AC-006: empty, whitespace or oversized questions fail before any
// provider (or database) call.
func TestInvalidQuestionsSkipProviders(t *testing.T) {
	h := NewHybrid(nil, panicEmbedder{}, Options{}, nil)
	for q, want := range map[string]error{
		"":                                      ErrEmptyQuestion,
		"   \t\n":                               ErrEmptyQuestion,
		strings.Repeat("a", MaxQuestionRunes+1): ErrQuestionTooLong,
		"\xff\xfe":                              ErrEmptyQuestion,
	} {
		if _, err := h.Search(context.Background(), uuid.New(), q, 8); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", q[:min(len(q), 10)], err, want)
		}
	}
	if q, err := ValidateQuestion("  refund policy  "); err != nil || q != "refund policy" {
		t.Errorf("trim: %q %v", q, err)
	}
}

type failingEmbedder struct{ panicEmbedder }

func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("503")
}

func TestEmbeddingFailure(t *testing.T) {
	h := NewHybrid(nil, failingEmbedder{}, Options{}, nil)
	if _, err := h.Search(context.Background(), uuid.New(), "q", 8); !errors.Is(err, ErrEmbeddingFailure) {
		t.Errorf("err %v", err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"0.8.0": true, "0.8.1": true, "0.10.0": true, "1.0": true, "0.7.4": false, "0.5": false, "x": false, "": false} {
		if versionAtLeast(v, 0, 8) != want {
			t.Errorf("%q", v)
		}
	}
}
