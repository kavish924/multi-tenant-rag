package retrieval

import (
	"bytes"
	"sort"

	"github.com/google/uuid"
)

// RRF is one leg's Reciprocal Rank Fusion term, 1 / (k + rank); a rank of 0
// (absent) contributes nothing.
func RRF(k float64, rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return 1 / (k + float64(rank))
}

// Fuse scores candidates (chunks found by at least one leg; VectorRank and
// TextRank are 1-based, 0 when absent) with RRF over both legs (RET-REQ-004), orders them
// by score, keeps at most perDocCap chunks per document (RET-REQ-006) and
// returns the first limit, ranked from 1. Ties break by vector rank, then
// text rank, then chunk id, so results are deterministic.
func Fuse(cands []Hit, k float64, perDocCap, limit int) []Hit {
	scored := make([]Hit, 0, len(cands))
	for _, c := range cands {
		if c.VectorRank <= 0 && c.TextRank <= 0 {
			continue
		}
		c.Score = RRF(k, c.VectorRank) + RRF(k, c.TextRank)
		scored = append(scored, c)
	}
	sort.Slice(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if ra, rb := rankKey(a.VectorRank), rankKey(b.VectorRank); ra != rb {
			return ra < rb
		}
		if ra, rb := rankKey(a.TextRank), rankKey(b.TextRank); ra != rb {
			return ra < rb
		}
		return bytes.Compare(a.ChunkID[:], b.ChunkID[:]) < 0
	})

	out := make([]Hit, 0, min(limit, len(scored)))
	perDoc := map[uuid.UUID]int{}
	for _, h := range scored {
		if len(out) == limit {
			break
		}
		if perDocCap > 0 && perDoc[h.DocumentID] >= perDocCap {
			continue
		}
		perDoc[h.DocumentID]++
		h.Rank = len(out) + 1
		out = append(out, h)
	}
	return out
}

func rankKey(r int) int {
	if r <= 0 {
		return int(^uint(0) >> 1)
	}
	return r
}
