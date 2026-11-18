package evaluation

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavish924/multi-tenant-rag/internal/ingestion/chunk"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
)

const dataDir = "../../tests/evaluation"

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// 2.2: metric math against hand-computed values.
func TestRetrievalMetrics(t *testing.T) {
	ranked := []string{"x", "a", "y", "b", "z"}
	rel := []string{"a", "b", "c"}

	if v, _ := RecallAtK(ranked, rel, 8); !near(v, 2.0/3) {
		t.Errorf("recall@8 %v", v)
	}
	if v, _ := RecallAtK(ranked, rel, 2); !near(v, 1.0/3) {
		t.Errorf("recall@2 %v", v)
	}
	if v, _ := ReciprocalRank(ranked, rel); !near(v, 0.5) {
		t.Errorf("mrr %v", v)
	}
	// DCG = 1/log2(3) + 1/log2(5); IDCG = 1 + 1/log2(3) + 1/log2(4).
	want := (1/math.Log2(3) + 1/math.Log2(5)) / (1 + 1/math.Log2(3) + 0.5)
	if v, _ := NDCGAtK(ranked, rel, 10); !near(v, want) {
		t.Errorf("ndcg %v want %v", v, want)
	}
	if v, _ := NDCGAtK([]string{"a", "b", "c"}, rel, 10); !near(v, 1) {
		t.Errorf("perfect ndcg %v", v)
	}
	if v, _ := ReciprocalRank([]string{"x"}, rel); v != 0 {
		t.Errorf("no hit rr %v", v)
	}
	// Duplicates count once; empty relevant sets are undefined.
	if v, _ := RecallAtK([]string{"a", "a", "a"}, []string{"a", "b"}, 8); !near(v, 0.5) {
		t.Errorf("duplicate recall %v", v)
	}
	if _, ok := RecallAtK(ranked, nil, 8); ok {
		t.Error("recall defined for a refusal case")
	}
}

// The golden set loads, validates and meets EVA-REQ-001's shape.
func TestGoldenSet(t *testing.T) {
	cases, err := LoadCases(filepath.Join(dataDir, "golden.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tenants, types := map[string]int{}, map[string]int{}
	for _, c := range cases {
		tenants[c.Tenant]++
		types[c.Type]++
	}
	if len(cases) < 50 || len(tenants) < 2 {
		t.Errorf("%d cases over %d tenants", len(cases), len(tenants))
	}
	for _, typ := range []string{TypeLexical, TypeSemantic, TypeMultiHop, TypeOutOfScope, TypeCrossTenant} {
		if types[typ] < 3 {
			t.Errorf("only %d %s cases", types[typ], typ)
		}
	}
	if n := len(Filter(cases, true)); n < 8 || n > 20 {
		t.Errorf("fast subset has %d cases", n)
	}
	bad := []Case{{ID: "a", Tenant: "t", Question: "q", Refusal: true, RelevantChunks: []string{"t/x#0"}},
		{ID: "a", Tenant: "t", Question: "q", RelevantChunks: []string{"other/x#0"}}}
	if err := Validate(bad); err == nil {
		t.Error("invalid cases accepted")
	}
}

// Every label in the golden set names a chunk the real chunker produces
// from the corpus (each file is one chunk), so labels cannot drift silently.
func TestLabelsMatchCorpus(t *testing.T) {
	cases, err := LoadCases(filepath.Join(dataDir, "golden.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	tenants, _ := os.ReadDir(filepath.Join(dataDir, "corpus"))
	for _, ten := range tenants {
		files, _ := filepath.Glob(filepath.Join(dataDir, "corpus", ten.Name(), "*.md"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			secs, err := extract.Extract("text/plain", b, extract.DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			chunks := chunk.Split(secs, chunk.Defaults)
			if len(chunks) != 1 {
				t.Errorf("%s: %d chunks, the corpus keeps each file to one", f, len(chunks))
			}
			for _, c := range chunks {
				known[Label(ten.Name(), filepath.Base(f), c.Ordinal)] = true
			}
		}
	}
	for _, c := range cases {
		for _, l := range c.RelevantChunks {
			if !known[l] {
				t.Errorf("case %s: label %s matches no corpus chunk", c.ID, l)
			}
		}
	}
}

type fakeSystem struct {
	hits    map[string][]Hit
	answers map[string]Answer
	err     error
}

func (f fakeSystem) Search(_ context.Context, c Case, _ int) ([]Hit, error) {
	return f.hits[c.ID], f.err
}
func (f fakeSystem) Ask(_ context.Context, c Case) (Answer, error) { return f.answers[c.ID], nil }

type scriptJudge struct{ reply string }

func (scriptJudge) Model() string { return "judge" }
func (j scriptJudge) Stream(ctx context.Context, req llm.GenerateRequest) (llm.Stream, error) {
	return llm.FakeGenerator{Respond: func(llm.GenerateRequest) string { return j.reply }}.Stream(ctx, req)
}

func cases() []Case {
	return []Case{
		{ID: "ok", Tenant: "a", Question: "q1", RelevantChunks: []string{"a/doc#0"}, Type: TypeLexical},
		{ID: "refuse", Tenant: "a", Question: "q2", Refusal: true, Type: TypeOutOfScope},
		{ID: "memory", Tenant: "a", Question: "q3", Refusal: true, Type: TypeOutOfScope},
		{ID: "badcite", Tenant: "a", Question: "q4", RelevantChunks: []string{"a/other#0"}, Type: TypeSemantic},
	}
}

func system() fakeSystem {
	return fakeSystem{
		hits: map[string][]Hit{
			"ok":      {{Label: "a/doc#0", Content: "Refunds take 30 days."}},
			"refuse":  {{Label: "a/doc#0"}},
			"memory":  {{Label: "b/secret#0"}}, // a leak
			"badcite": {{Label: "a/x#0"}, {Label: "a/other#0"}},
		},
		answers: map[string]Answer{
			"ok":      {Text: "Refunds take 30 days [1].", Citations: []Citation{{N: 1, Label: "a/doc#0"}}},
			"refuse":  {Text: llm.InsufficientContext, Insufficient: true},
			"memory":  {Text: "France won it [1].", Citations: []Citation{{N: 1, Label: "a/doc#0"}}},
			"badcite": {Text: "Something [1] and [7].", Citations: []Citation{{N: 1, Label: "a/other#0"}}},
		},
	}
}

// EVA-AC-003/004/006 and the leak metric.
func TestRunnerDeterministicMetrics(t *testing.T) {
	results := (&Runner{System: system()}).Run(context.Background(), cases())
	by := map[string]CaseResult{}
	for _, r := range results {
		by[r.ID] = r
	}
	if !by["ok"].RefusalCorrect || *by["ok"].RecallAt8 != 1 || by["ok"].ValidCitations != 1 {
		t.Errorf("ok: %+v", by["ok"])
	}
	if !by["refuse"].RefusalCorrect || !by["refuse"].Declined {
		t.Errorf("refuse: %+v", by["refuse"])
	}
	if by["memory"].RefusalCorrect || by["memory"].Leaks != 1 {
		t.Errorf("answering from memory must fail and the foreign hit is a leak: %+v", by["memory"])
	}
	if b := by["badcite"]; b.Markers != 2 || b.ValidCitations != 1 || *b.MRR != 0.5 {
		t.Errorf("badcite: %+v", b)
	}

	agg := Aggregate(results)
	if v := *agg[MCitation].Value; !near(v, 3.0/4) { // ok 1/1, memory 1/1, badcite 1/2
		t.Errorf("citation validity %v", v)
	}
	if v := *agg[MRefusal].Value; !near(v, 3.0/4) {
		t.Errorf("refusal accuracy %v", v)
	}
	if *agg[MLeaks].Value != 1 {
		t.Errorf("leaks %v", *agg[MLeaks].Value)
	}
	// No judge: answer metrics are skipped, not zero (EVA-AC-006).
	for _, m := range []string{MFaithfulness, MCorrectness, MSupport} {
		if agg[m].Value != nil || !strings.Contains(agg[m].Reason, "skipped") {
			t.Errorf("%s: %+v", m, agg[m])
		}
	}
}

func TestRunnerJudge(t *testing.T) {
	j := &Judge{Generator: scriptJudge{reply: "```json\n{\"faithfulness\": 5, \"correctness\": 4, \"citations_supported\": true, \"rationale\": \"fine\"}\n```"}}
	results := (&Runner{System: system(), Judge: j}).Run(context.Background(), cases())
	agg := Aggregate(results)
	if agg[MFaithfulness].Value == nil || *agg[MFaithfulness].Value != 5 || *agg[MCorrectness].Value != 4 || *agg[MSupport].Value != 1 {
		t.Errorf("judged aggregates %+v", agg)
	}
	if j.Calls() != 3 { // the declined case is not judged
		t.Errorf("judge calls %d", j.Calls())
	}

	// Budget: calls past the cap fail and those cases are skipped.
	capped := &Judge{Generator: j.Generator, MaxCalls: 1}
	results = (&Runner{System: system(), Judge: capped}).Run(context.Background(), cases())
	errs := 0
	for _, r := range results {
		if strings.Contains(r.JudgeError, "budget") {
			errs++
		}
	}
	if errs != 2 {
		t.Errorf("budget errors %d", errs)
	}

	// A broken judge leaves the deterministic metrics intact.
	broken := &Judge{Generator: scriptJudge{reply: "looks good to me"}}
	agg = Aggregate((&Runner{System: system(), Judge: broken}).Run(context.Background(), cases()))
	if agg[MFaithfulness].Value != nil || agg[MRecall].Value == nil || agg[MRefusal].Value == nil {
		t.Errorf("broken judge aggregates %+v", agg)
	}
}

func TestParseVerdict(t *testing.T) {
	if _, err := ParseVerdict(`{"faithfulness": 6, "correctness": 3, "citations_supported": true, "rationale": ""}`); err == nil {
		t.Error("out-of-range score accepted")
	}
	if _, err := ParseVerdict(`{"faithfulness": 4, "correctness": 3, "extra": 1}`); err == nil {
		t.Error("unknown field accepted")
	}
	if v, err := ParseVerdict(` {"faithfulness": 4, "correctness": 3, "citations_supported": false, "rationale": "x"} `); err != nil || v.Faithfulness != 4 {
		t.Errorf("%+v %v", v, err)
	}
}

// EVA-AC-002: below threshold fails; skipped and ungated metrics do not.
func TestGate(t *testing.T) {
	th, err := LoadThresholds(filepath.Join(dataDir, "thresholds.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v := func(x float64) Metric { return Metric{Value: &x, N: 1} }
	good := map[string]Metric{
		MRecall: v(0.9), MMRR: v(0.8), MNDCG: v(0.8), MCitation: v(1), MLeaks: v(0),
		MRefusal: v(0.2), MFaithfulness: {Reason: "skipped"}, MCorrectness: {Reason: "skipped"}, MSupport: {Reason: "skipped"},
	}
	g := Gate(good, th, false)
	if !g.Passed || len(g.Ungated) != 4 {
		t.Errorf("fake-generator run: %+v", g)
	}
	// With a real generator, a skipped judge is skipped, refusal is gated.
	g = Gate(good, th, true)
	if g.Passed || len(g.Failures) != 1 || g.Failures[0].Metric != MRefusal || len(g.Skipped) != 3 {
		t.Errorf("real-generator run: %+v", g)
	}
	bad := map[string]Metric{MRecall: v(0.1), MMRR: v(0.8), MNDCG: v(0.8), MCitation: v(1), MLeaks: v(1)}
	g = Gate(bad, th, false)
	if g.Passed || len(g.Failures) != 2 {
		t.Errorf("bad run: %+v", g)
	}
	if _, err := LoadThresholds(writeTemp(t, "min:\n  recal_at_8: 0.5\n")); err == nil {
		t.Error("typo in a metric name accepted")
	}
}

func writeTemp(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// EVA-AC-001/005: the summary and the A/B table render every metric.
func TestReports(t *testing.T) {
	th, _ := LoadThresholds(filepath.Join(dataDir, "thresholds.yaml"))
	results := (&Runner{System: system()}).Run(context.Background(), cases())
	agg := Aggregate(results)
	a := Report{Meta: Meta{Name: "A"}, Aggregates: agg, Gate: Gate(agg, th, false), Cases: results}
	b := a
	b.Meta.Name = "B"
	s := Summary(a, th, &b, 5)
	for _, want := range []string{"Gate: FAILED", "cross_tenant_leaks", "refusal violation", "memory", "+0.000", "not gated"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	ab := ABTable(a, b)
	for _, m := range MetricOrder {
		if !strings.Contains(ab, "| "+m+" |") {
			t.Errorf("A/B table lacks %s", m)
		}
	}
	p := filepath.Join(t.TempDir(), "r.json")
	if err := WriteJSON(p, a); err != nil {
		t.Fatal(err)
	}
	back, err := LoadReport(p)
	if err != nil || len(back.Cases) != len(a.Cases) || *back.Aggregates[MRecall].Value != *a.Aggregates[MRecall].Value {
		t.Errorf("round trip: %v", err)
	}
}

func TestSearchErrorRecorded(t *testing.T) {
	sys := system()
	sys.err = errors.New("db down")
	r := (&Runner{System: sys}).Run(context.Background(), cases()[:1])
	if r[0].Error == "" {
		t.Error("search error not recorded")
	}
}
