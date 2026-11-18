package evaluation

import (
	"fmt"
	"os"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Thresholds is tests/evaluation/thresholds.yaml (EVA-REQ-007).
type Thresholds struct {
	// Min: the aggregate must be ≥ the value.
	Min map[string]float64 `yaml:"min"`
	// Max: the aggregate must be ≤ the value (cross_tenant_leaks: 0).
	Max map[string]float64 `yaml:"max"`
	// RequiresRealGenerator lists metrics that only mean something with a
	// real model; with the deterministic fake generator they are reported
	// but not gated.
	RequiresRealGenerator []string `yaml:"requires_real_generator"`
}

// LoadThresholds reads the YAML file.
func LoadThresholds(path string) (Thresholds, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return Thresholds{}, err
	}
	var t Thresholds
	if err := yaml.Unmarshal(b, &t); err != nil {
		return Thresholds{}, fmt.Errorf("thresholds: %w", err)
	}
	for k := range t.Min {
		if !slices.Contains(MetricOrder, k) {
			return Thresholds{}, fmt.Errorf("thresholds: unknown metric %q", k)
		}
	}
	for k := range t.Max {
		if !slices.Contains(MetricOrder, k) {
			return Thresholds{}, fmt.Errorf("thresholds: unknown metric %q", k)
		}
	}
	return t, nil
}

// GateResult is the outcome of the threshold check.
type GateResult struct {
	Passed   bool          `json:"passed"`
	Failures []GateFailure `json:"failures"`
	Skipped  []string      `json:"skipped"` // gated metrics with no value
	Ungated  []string      `json:"ungated"` // need a real generator
	Checks   []GateCheck   `json:"checks"`  // every gated comparison
}

// GateFailure is one metric outside its threshold.
type GateFailure struct {
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Bound     string  `json:"bound"` // min or max
}

// GateCheck is one gated comparison.
type GateCheck struct {
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Bound     string  `json:"bound"`
	OK        bool    `json:"ok"`
}

// Gate compares aggregates with thresholds (EVA-AC-002). A skipped metric
// is not a failure (EVA-AC-006): it is listed under Skipped.
func Gate(agg map[string]Metric, t Thresholds, realGenerator bool) GateResult {
	g := GateResult{Passed: true, Failures: []GateFailure{}, Skipped: []string{}, Ungated: []string{}, Checks: []GateCheck{}}
	check := func(metric string, threshold float64, bound string) {
		if !realGenerator && slices.Contains(t.RequiresRealGenerator, metric) {
			g.Ungated = append(g.Ungated, metric)
			return
		}
		m, ok := agg[metric]
		if !ok || m.Value == nil {
			g.Skipped = append(g.Skipped, metric)
			return
		}
		v := *m.Value
		pass := v >= threshold
		if bound == "max" {
			pass = v <= threshold
		}
		g.Checks = append(g.Checks, GateCheck{Metric: metric, Value: v, Threshold: threshold, Bound: bound, OK: pass})
		if !pass {
			g.Passed = false
			g.Failures = append(g.Failures, GateFailure{Metric: metric, Value: v, Threshold: threshold, Bound: bound})
		}
	}
	for _, m := range MetricOrder {
		if th, ok := t.Min[m]; ok {
			check(m, th, "min")
		}
		if th, ok := t.Max[m]; ok {
			check(m, th, "max")
		}
	}
	return g
}
