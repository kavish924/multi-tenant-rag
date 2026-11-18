package evaluation

import "math"

// RecallAtK is the share of relevant labels found in the first k ranked
// results (EVA-REQ-003). It is undefined (ok=false) with no relevant labels.
func RecallAtK(ranked []string, relevant []string, k int) (float64, bool) {
	if len(relevant) == 0 {
		return 0, false
	}
	rel := set(relevant)
	found := 0
	for i, r := range ranked {
		if i == k {
			break
		}
		if rel[r] {
			found++
			delete(rel, r) // a label counts once
		}
	}
	return float64(found) / float64(len(relevant)), true
}

// ReciprocalRank is 1/rank of the first relevant result, 0 if none.
func ReciprocalRank(ranked []string, relevant []string) (float64, bool) {
	if len(relevant) == 0 {
		return 0, false
	}
	rel := set(relevant)
	for i, r := range ranked {
		if rel[r] {
			return 1 / float64(i+1), true
		}
	}
	return 0, true
}

// NDCGAtK is binary-relevance nDCG over the first k results:
// DCG = Σ rel_i / log2(i+1) (1-based i), normalized by the ideal DCG.
func NDCGAtK(ranked []string, relevant []string, k int) (float64, bool) {
	if len(relevant) == 0 {
		return 0, false
	}
	rel := set(relevant)
	dcg := 0.0
	for i, r := range ranked {
		if i == k {
			break
		}
		if rel[r] {
			dcg += 1 / math.Log2(float64(i+2))
			delete(rel, r)
		}
	}
	ideal := 0.0
	for i := 0; i < min(len(relevant), k); i++ {
		ideal += 1 / math.Log2(float64(i+2))
	}
	return dcg / ideal, true
}

func set(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// mean of the defined values; ok=false when there are none.
func mean(vals []float64) (float64, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	s := 0.0
	for _, v := range vals {
		s += v
	}
	return s / float64(len(vals)), true
}
