package evaluation

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
)

// WriteJSON writes the report artifact.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// LoadReport reads a previous report (the baseline for the diff).
func LoadReport(path string) (Report, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return Report{}, err
	}
	var r Report
	return r, json.Unmarshal(b, &r)
}

func fmtVal(m Metric) string {
	if m.Value == nil {
		if m.Reason != "" {
			return "— (" + m.Reason + ")"
		}
		return "—"
	}
	return fmt.Sprintf("%.3f", *m.Value)
}

// Summary is the human-readable companion of the report (EVA-AC-001): the
// aggregate table with thresholds and the delta against a baseline run, the
// gate verdict, per-type breakdown, failing cases, and the spot-check
// sample for the manual review protocol (EVA-REQ-004).
func Summary(r Report, t Thresholds, baseline *Report, spotCheck int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Evaluation: %s\n\n", r.Meta.Name)
	fmt.Fprintf(&b, "%d cases · embedder `%s` · generator `%s` · judge `%s` (%d calls) · rerank %v · top-k %d · %.1fs\n\n",
		r.Meta.Cases, r.Meta.Embedder, r.Meta.Generator, r.Meta.Judge, r.Meta.JudgeCalls, r.Meta.Rerank, r.Meta.TopK, r.Meta.DurationS)

	verdict := "PASSED"
	if !r.Gate.Passed {
		verdict = "FAILED"
	}
	fmt.Fprintf(&b, "**Gate: %s**\n\n", verdict)
	b.WriteString("| Metric | Value | Threshold | Δ vs baseline |\n|---|---|---|---|\n")
	for _, name := range MetricOrder {
		m := r.Aggregates[name]
		th := "—"
		if v, ok := t.Min[name]; ok {
			th = fmt.Sprintf("≥ %.3f", v)
		}
		if v, ok := t.Max[name]; ok {
			th = fmt.Sprintf("≤ %.3f", v)
		}
		for _, u := range r.Gate.Ungated {
			if u == name {
				th += " (not gated: fake generator)"
			}
		}
		delta := "—"
		if baseline != nil && m.Value != nil {
			if bm, ok := baseline.Aggregates[name]; ok && bm.Value != nil {
				delta = fmt.Sprintf("%+.3f", *m.Value-*bm.Value)
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", name, fmtVal(m), th, delta)
	}
	if len(r.Gate.Failures) > 0 {
		b.WriteString("\n**Below threshold:**\n\n")
		for _, f := range r.Gate.Failures {
			fmt.Fprintf(&b, "- `%s` = %.3f (%s %.3f)\n", f.Metric, f.Value, f.Bound, f.Threshold)
		}
	}

	b.WriteString("\n## By case type\n\n| Type | recall@8 | MRR | refusal accuracy |\n|---|---|---|---|\n")
	byType := ByType(r.Cases)
	types := make([]string, 0, len(byType))
	for k := range byType {
		types = append(types, k)
	}
	sort.Strings(types)
	for _, k := range types {
		m := byType[k]
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", k, fmtVal(m[MRecall]), fmtVal(m[MMRR]), fmtVal(m[MRefusal]))
	}

	var failing []CaseResult
	for _, c := range r.Cases {
		if c.Error != "" || !c.RefusalCorrect || (c.RecallAt8 != nil && *c.RecallAt8 < 1) || c.Leaks > 0 {
			failing = append(failing, c)
		}
	}
	if len(failing) > 0 {
		b.WriteString("\n## Cases to look at\n\n")
		for _, c := range failing {
			var why []string
			if c.Error != "" {
				why = append(why, "error: "+c.Error)
			}
			if !c.RefusalCorrect {
				why = append(why, "refusal violation")
			}
			if c.RecallAt8 != nil && *c.RecallAt8 < 1 {
				why = append(why, fmt.Sprintf("recall@8 %.2f", *c.RecallAt8))
			}
			if c.Leaks > 0 {
				why = append(why, fmt.Sprintf("%d cross-tenant leaks", c.Leaks))
			}
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", c.ID, c.Type, strings.Join(why, "; "))
		}
	}

	var judged []CaseResult
	for _, c := range r.Cases {
		if c.Faithfulness != nil {
			judged = append(judged, c)
		}
	}
	if len(judged) > 0 && spotCheck > 0 {
		rand.Shuffle(len(judged), func(i, j int) { judged[i], judged[j] = judged[j], judged[i] })
		b.WriteString("\n## Spot check (review by hand; see tests/evaluation/README.md)\n\n")
		for _, c := range judged[:min(spotCheck, len(judged))] {
			fmt.Fprintf(&b, "### %s\n\n- **Question:** %s\n- **Answer:** %s\n- **Judge:** faithfulness %.0f, correctness %.0f — %s\n- **Reviewer agrees?** ☐ yes ☐ no\n\n",
				c.ID, c.Question, c.Answer, *c.Faithfulness, *c.Correctness, c.JudgeRationale)
		}
	}
	return b.String()
}

// ABTable is the side-by-side comparison of two runs (EVA-AC-005).
func ABTable(a, b Report) string {
	var s strings.Builder
	fmt.Fprintf(&s, "# A/B: %s vs %s\n\n", a.Meta.Name, b.Meta.Name)
	fmt.Fprintf(&s, "| | A: %s | B: %s |\n|---|---|---|\n", a.Meta.Name, b.Meta.Name)
	fmt.Fprintf(&s, "| rerank | %v | %v |\n| top-k | %d | %d |\n| chunk max tokens | %d | %d |\n| duration (s) | %.1f | %.1f |\n\n",
		a.Meta.Rerank, b.Meta.Rerank, a.Meta.TopK, b.Meta.TopK, a.Meta.ChunkMax, b.Meta.ChunkMax, a.Meta.DurationS, b.Meta.DurationS)
	s.WriteString("| Metric | A | B | Δ (B − A) |\n|---|---|---|---|\n")
	for _, name := range MetricOrder {
		ma, mb := a.Aggregates[name], b.Aggregates[name]
		delta := "—"
		if ma.Value != nil && mb.Value != nil {
			delta = fmt.Sprintf("%+.3f", *mb.Value-*ma.Value)
		}
		fmt.Fprintf(&s, "| %s | %s | %s | %s |\n", name, fmtVal(ma), fmtVal(mb), delta)
	}
	return s.String()
}
