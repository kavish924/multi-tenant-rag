// Command eval is the evaluation harness (spec 014): it ingests the
// fixture corpus into fresh tenants through the real pipeline, runs the
// golden set through real search and the real /ask handler (guards
// included), computes retrieval / citation / refusal metrics and optional
// LLM-judge answer metrics, gates them against thresholds.yaml, and writes
// eval-report.json + eval-summary.md.
//
//	eval run    [flags]          one configuration (make eval)
//	eval ab     [flags] --b-...  two configurations side by side
//	eval report --in report.json re-render the summary of a saved report
//
// Exit codes: 0 gate passed, 1 gate failed (EVA-AC-002), 2 harness error.
// DATABASE_URL must allow migrations (an admin DSN): the harness creates a
// login role that is a member of `app` only, so retrieval runs under forced
// RLS as in production.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/evaluation"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == extract.HelperArg {
		extract.HelperMain() // the harness doubles as the parser helper
	}
	os.Exit(run(os.Args[1:]))
}

type options struct {
	dir        string // tests/evaluation
	out        string
	summary    string
	baseline   string
	fast       bool
	keep       bool
	judge      string
	judgeMax   int
	spotCheck  int
	config     runConfig
	configB    runConfig
	reportPath string
}

func run(args []string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: eval run|ab|report [flags]")
		return 2
	}
	mode, args := args[0], args[1:]
	o := options{}
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.StringVar(&o.dir, "dir", "tests/evaluation", "directory with golden.jsonl, corpus/ and thresholds.yaml")
	fs.StringVar(&o.out, "out", "eval-report.json", "report artifact")
	fs.StringVar(&o.summary, "summary", "eval-summary.md", "human-readable summary")
	fs.StringVar(&o.baseline, "baseline", "", "previous report for the Δ columns")
	fs.BoolVar(&o.fast, "fast", false, "run only the cases tagged fast (CI per pull request)")
	fs.BoolVar(&o.keep, "keep", false, "keep the run's tenants instead of purging them")
	fs.StringVar(&o.judge, "judge", "none", "answer judge: none or generator")
	fs.IntVar(&o.judgeMax, "judge-max-calls", 100, "judge call budget (EVA-NFR-001)")
	fs.IntVar(&o.spotCheck, "spot-check", 5, "judged cases sampled into the summary for manual review")
	fs.StringVar(&o.reportPath, "in", "", "report to re-render (report mode)")
	o.config.register(fs, "", "A")
	o.configB.register(fs, "b-", "B")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	switch mode {
	case "run":
		code, err := runOnce(ctx, log, o, o.config, o.out, o.summary)
		if err != nil {
			log.Error("eval failed", "err", err)
			return 2
		}
		return code
	case "ab":
		return runAB(ctx, log, o)
	case "report":
		return rerender(o)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		return 2
	}
}

// runOnce runs one configuration end to end and returns the gate exit code.
func runOnce(ctx context.Context, log *slog.Logger, o options, cfg runConfig, outPath, summaryPath string) (int, error) {
	rep, th, err := evaluate(ctx, log, o, cfg)
	if err != nil {
		return 2, err
	}
	var base *evaluation.Report
	if o.baseline != "" {
		b, err := evaluation.LoadReport(o.baseline)
		if err != nil {
			return 2, fmt.Errorf("baseline: %w", err)
		}
		base = &b
	}
	if err := evaluation.WriteJSON(outPath, rep); err != nil {
		return 2, err
	}
	summary := evaluation.Summary(rep, th, base, o.spotCheck)
	if err := os.WriteFile(summaryPath, []byte(summary), 0o600); err != nil {
		return 2, err
	}
	fmt.Println(summary)
	if !rep.Gate.Passed {
		for _, f := range rep.Gate.Failures {
			fmt.Fprintf(os.Stderr, "FAIL %s = %.3f (%s %.3f)\n", f.Metric, f.Value, f.Bound, f.Threshold)
		}
		return 1, nil
	}
	return 0, nil
}

func runAB(ctx context.Context, log *slog.Logger, o options) int {
	a, th, err := evaluate(ctx, log, o, o.config)
	if err != nil {
		log.Error("eval A failed", "err", err)
		return 2
	}
	b, _, err := evaluate(ctx, log, o, o.configB)
	if err != nil {
		log.Error("eval B failed", "err", err)
		return 2
	}
	if err := evaluation.WriteJSON("eval-ab.json", map[string]evaluation.Report{"a": a, "b": b}); err != nil {
		log.Error("write", "err", err)
		return 2
	}
	table := evaluation.ABTable(a, b) + "\n---\n\n" + evaluation.Summary(a, th, nil, 0) + "\n---\n\n" + evaluation.Summary(b, th, nil, 0)
	if err := os.WriteFile("eval-ab.md", []byte(table), 0o600); err != nil {
		log.Error("write", "err", err)
		return 2
	}
	fmt.Println(evaluation.ABTable(a, b))
	return 0 // A/B informs a decision; it does not gate
}

func rerender(o options) int {
	if o.reportPath == "" {
		fmt.Fprintln(os.Stderr, "report mode needs --in")
		return 2
	}
	rep, err := evaluation.LoadReport(o.reportPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	th, err := evaluation.LoadThresholds(o.dir + "/thresholds.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var base *evaluation.Report
	if o.baseline != "" {
		b, err := evaluation.LoadReport(o.baseline)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		base = &b
	}
	fmt.Println(evaluation.Summary(rep, th, base, o.spotCheck))
	return 0
}

// evaluate prepares the environment for one configuration and runs it.
func evaluate(ctx context.Context, log *slog.Logger, o options, cfg runConfig) (evaluation.Report, evaluation.Thresholds, error) {
	start := time.Now()
	cases, err := evaluation.LoadCases(o.dir + "/golden.jsonl")
	if err != nil {
		return evaluation.Report{}, evaluation.Thresholds{}, err
	}
	cases = evaluation.Filter(cases, o.fast)
	if len(cases) == 0 {
		return evaluation.Report{}, evaluation.Thresholds{}, errors.New("no cases selected")
	}
	th, err := evaluation.LoadThresholds(o.dir + "/thresholds.yaml")
	if err != nil {
		return evaluation.Report{}, evaluation.Thresholds{}, err
	}

	env, err := setup(ctx, log, o.dir, cfg)
	if err != nil {
		return evaluation.Report{}, th, fmt.Errorf("setup: %w", err)
	}
	defer env.close(ctx, o.keep)

	var judge *evaluation.Judge
	judgeName := "none"
	if o.judge == "generator" {
		judge = &evaluation.Judge{Generator: env.generator, MaxCalls: o.judgeMax}
		judgeName = env.generator.Model()
	}
	runner := &evaluation.Runner{System: env, Judge: judge}
	results := runner.Run(ctx, cases)
	agg := evaluation.Aggregate(results)
	rep := evaluation.Report{
		Meta: evaluation.Meta{
			Name: cfg.name, StartedAt: start, DurationS: time.Since(start).Seconds(), Cases: len(cases),
			Embedder: env.embedder.Model(), Generator: env.generator.Model(), RealModel: cfg.realGenerator(),
			Rerank: cfg.rerank, TopK: cfg.topK, ChunkMax: cfg.chunkMax, Judge: judgeName,
		},
		Aggregates: agg,
		Gate:       evaluation.Gate(agg, th, cfg.realGenerator()),
		Cases:      results,
	}
	if judge != nil {
		rep.Meta.JudgeCalls = judge.Calls()
	}
	return rep, th, nil
}
