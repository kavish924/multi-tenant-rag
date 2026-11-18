package evaluation

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Hit is a retrieved chunk as the harness sees it.
type Hit struct {
	Label   string // "<tenant>/<file>#<ordinal>"; "" when unknown
	Content string
}

// Citation is an emitted citation event.
type Citation struct {
	N     int
	Label string
}

// Answer is the outcome of one ask through the real handler.
type Answer struct {
	Text         string
	Citations    []Citation
	Insufficient bool // the zero-source path
	Refused      bool // a guard refusal
}

// System is the platform under test (cmd/eval wires the real one).
type System interface {
	Search(ctx context.Context, c Case, k int) ([]Hit, error)
	Ask(ctx context.Context, c Case) (Answer, error)
}

// Ranks used by the retrieval metrics (EVA-REQ-003).
const (
	RecallK = 8
	NDCGK   = 10
	// SearchK is how many results the harness asks for.
	SearchK = 50
)

// CaseResult is one case's metrics; nil pointers are undefined/skipped.
type CaseResult struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Tenant   string   `json:"tenant"`
	Question string   `json:"question"`
	Top      []string `json:"top"` // first NDCGK labels

	RecallAt8 *float64 `json:"recall_at_8"`
	MRR       *float64 `json:"mrr"`
	NDCGAt10  *float64 `json:"ndcg_at_10"`
	Leaks     int      `json:"cross_tenant_leaks"`

	Answer         string `json:"answer"`
	Declined       bool   `json:"declined"`
	RefusalCorrect bool   `json:"refusal_correct"`
	Markers        int    `json:"citation_markers"`
	ValidCitations int    `json:"valid_citations"`

	Faithfulness       *float64 `json:"faithfulness"`
	Correctness        *float64 `json:"correctness"`
	CitationsSupported *bool    `json:"citations_supported"`
	JudgeRationale     string   `json:"judge_rationale,omitempty"`
	JudgeError         string   `json:"judge_error,omitempty"`

	Error string `json:"error,omitempty"`
}

// Runner executes cases.
type Runner struct {
	System System
	Judge  *Judge // nil: answer metrics skipped (EVA-AC-006)
	// Sources is how many retrieved chunks the judge sees.
	Sources int
}

var markerRe = regexp.MustCompile(`\[(\d{1,4})\]`)

// Run executes every case and returns the per-case results. A case error
// is recorded on the case; it does not stop the run.
func (r *Runner) Run(ctx context.Context, cases []Case) []CaseResult {
	out := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		out = append(out, r.runCase(ctx, c))
	}
	return out
}

func (r *Runner) runCase(ctx context.Context, c Case) CaseResult {
	res := CaseResult{ID: c.ID, Type: c.Type, Tenant: c.Tenant, Question: c.Question}
	hits, err := r.System.Search(ctx, c, SearchK)
	if err != nil {
		res.Error = "search: " + err.Error()
		return res
	}
	ranked := make([]string, len(hits))
	for i, h := range hits {
		ranked[i] = h.Label
		if h.Label != "" && !strings.HasPrefix(h.Label, c.Tenant+"/") {
			res.Leaks++ // zero leakage is itself a metric (spec 015)
		}
	}
	res.Top = ranked[:min(len(ranked), NDCGK)]
	if v, ok := RecallAtK(ranked, c.RelevantChunks, RecallK); ok {
		res.RecallAt8 = &v
	}
	if v, ok := ReciprocalRank(ranked, c.RelevantChunks); ok {
		res.MRR = &v
	}
	if v, ok := NDCGAtK(ranked, c.RelevantChunks, NDCGK); ok {
		res.NDCGAt10 = &v
	}

	ans, err := r.System.Ask(ctx, c)
	if err != nil {
		res.Error = "ask: " + err.Error()
		return res
	}
	res.Answer = truncate(ans.Text, 600)
	res.Declined = ans.Insufficient || ans.Refused || declines(ans.Text)
	// EVA-REQ-006 / EVA-AC-003: refusal cases must be declined (and cite
	// nothing); answerable cases must be answered.
	if c.Refusal {
		res.RefusalCorrect = res.Declined && len(ans.Citations) == 0
	} else {
		res.RefusalCorrect = !res.Declined
	}

	// EVA-REQ-005 / EVA-AC-004 (deterministic part): every distinct marker
	// in the text must have resolved to a citation of this tenant's chunk.
	markers := map[int]bool{}
	for _, m := range markerRe.FindAllStringSubmatch(ans.Text, -1) {
		n, _ := strconv.Atoi(m[1])
		markers[n] = true
	}
	res.Markers = len(markers)
	for _, cit := range ans.Citations {
		if markers[cit.N] && cit.Label != "" && strings.HasPrefix(cit.Label, c.Tenant+"/") {
			res.ValidCitations++
		}
		if cit.Label != "" && !strings.HasPrefix(cit.Label, c.Tenant+"/") {
			res.Leaks++
		}
	}

	if r.Judge != nil && !res.Declined {
		sources := make([]string, 0, r.sources())
		for _, h := range hits[:min(len(hits), r.sources())] {
			sources = append(sources, h.Content)
		}
		v, err := r.Judge.Grade(ctx, c.Question, c.ReferenceAnswer, ans.Text, sources)
		if err != nil {
			res.JudgeError = err.Error()
		} else {
			res.Faithfulness, res.Correctness = &v.Faithfulness, &v.Correctness
			res.CitationsSupported = &v.CitationsSupported
			res.JudgeRationale = truncate(v.Rationale, 300)
		}
	}
	return res
}

func (r *Runner) sources() int {
	if r.Sources > 0 {
		return r.Sources
	}
	return 8
}

// declines detects the insufficient-context reply in the text.
func declines(text string) bool {
	t := strings.ToLower(text)
	for _, p := range []string{"don't have enough information", "do not have enough information", "cannot answer", "can't answer", "not in your documents"} {
		if strings.Contains(t, p) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Metric is one aggregate value; Value is nil when skipped.
type Metric struct {
	Value  *float64 `json:"value"`
	N      int      `json:"n"` // cases that contributed
	Reason string   `json:"reason,omitempty"`
}

// Metric names (report schema, design §5).
const (
	MRecall       = "recall_at_8"
	MMRR          = "mrr"
	MNDCG         = "ndcg_at_10"
	MFaithfulness = "faithfulness"
	MCorrectness  = "correctness"
	MCitation     = "citation_validity"
	MSupport      = "citation_support"
	MRefusal      = "refusal_accuracy"
	MLeaks        = "cross_tenant_leaks"
)

// MetricOrder is the report order.
var MetricOrder = []string{MRecall, MMRR, MNDCG, MFaithfulness, MCorrectness, MCitation, MSupport, MRefusal, MLeaks}

// Aggregate computes the run's aggregates.
func Aggregate(results []CaseResult) map[string]Metric {
	collect := func(get func(CaseResult) *float64) Metric {
		var vals []float64
		for _, r := range results {
			if v := get(r); v != nil {
				vals = append(vals, *v)
			}
		}
		m, ok := mean(vals)
		if !ok {
			return Metric{Reason: "no data"}
		}
		return Metric{Value: &m, N: len(vals)}
	}
	agg := map[string]Metric{
		MRecall:       collect(func(r CaseResult) *float64 { return r.RecallAt8 }),
		MMRR:          collect(func(r CaseResult) *float64 { return r.MRR }),
		MNDCG:         collect(func(r CaseResult) *float64 { return r.NDCGAt10 }),
		MFaithfulness: collect(func(r CaseResult) *float64 { return r.Faithfulness }),
		MCorrectness:  collect(func(r CaseResult) *float64 { return r.Correctness }),
	}
	for _, k := range []string{MFaithfulness, MCorrectness} {
		if agg[k].Value == nil {
			agg[k] = Metric{Reason: "skipped: no judge result"}
		}
	}

	markers, valid, refusalOK, asked, leaks := 0, 0, 0, 0, 0
	supported, judged := 0, 0
	for _, r := range results {
		leaks += r.Leaks
		if r.Error != "" {
			continue
		}
		asked++
		markers += r.Markers
		valid += min(r.ValidCitations, r.Markers)
		if r.RefusalCorrect {
			refusalOK++
		}
		if r.CitationsSupported != nil {
			judged++
			if *r.CitationsSupported {
				supported++
			}
		}
	}
	ratio := func(num, den int, reason string) Metric {
		if den == 0 {
			return Metric{Reason: reason}
		}
		v := float64(num) / float64(den)
		return Metric{Value: &v, N: den}
	}
	agg[MCitation] = ratio(valid, markers, "no citation markers")
	agg[MSupport] = ratio(supported, judged, "skipped: no judge result")
	agg[MRefusal] = ratio(refusalOK, asked, "no answered cases")
	l := float64(leaks)
	agg[MLeaks] = Metric{Value: &l, N: len(results)}
	return agg
}

// Report is eval-report.json (EVA-AC-001).
type Report struct {
	Meta       Meta              `json:"meta"`
	Aggregates map[string]Metric `json:"aggregates"`
	Gate       GateResult        `json:"gate"`
	Cases      []CaseResult      `json:"per_case"`
}

// Meta describes the run.
type Meta struct {
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
	DurationS  float64   `json:"duration_s"`
	Cases      int       `json:"cases"`
	Embedder   string    `json:"embedder"`
	Generator  string    `json:"generator"`
	RealModel  bool      `json:"real_generator"`
	Rerank     bool      `json:"rerank"`
	TopK       int       `json:"top_k"`
	ChunkMax   int       `json:"chunk_max_tokens"`
	Judge      string    `json:"judge"`
	JudgeCalls int64     `json:"judge_calls"`
}

// ByType summarizes recall and refusal accuracy per case type.
func ByType(results []CaseResult) map[string]map[string]Metric {
	groups := map[string][]CaseResult{}
	for _, r := range results {
		groups[r.Type] = append(groups[r.Type], r)
	}
	out := map[string]map[string]Metric{}
	types := make([]string, 0, len(groups))
	for t := range groups {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		a := Aggregate(groups[t])
		out[t] = map[string]Metric{MRecall: a[MRecall], MMRR: a[MMRR], MRefusal: a[MRefusal]}
	}
	return out
}
