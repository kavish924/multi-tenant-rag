package guard

import (
	"context"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RedactionMarker replaces a redacted span.
const RedactionMarker = "[REDACTED]"

type pattern struct {
	name string
	re   *regexp.Regexp
}

// High-confidence secret and PII patterns (GRD-REQ-007). Each is short
// (well under the streaming window), so a match is always complete before
// the text around it is released.
var secretPatterns = []pattern{
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{"github_token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`)},
	{"slack_token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`)},
	{"api_secret_key", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_\-]{20,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{"private_ipv4", regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b`)},
}

var emailPattern = pattern{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)}

// A private key block can be longer than the streaming window, so it
// cannot be redacted safely mid-stream: its header blocks the whole answer
// (GRD-AC-006, "Block if redaction is impossible mid-stream").
var privateKeyHeader = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)

// Redactor replaces secret and PII patterns.
type Redactor struct {
	patterns []pattern
}

// NewRedactor builds the redactor; emails are redacted only when asked.
func NewRedactor(redactEmails bool) *Redactor {
	r := &Redactor{patterns: append([]pattern(nil), secretPatterns...)}
	if redactEmails {
		r.patterns = append(r.patterns, emailPattern)
	}
	return r
}

// Name implements Guard.
func (*Redactor) Name() string { return StageOutputRedact }

// Check implements Guard.
func (r *Redactor) Check(_ context.Context, text string) (Verdict, string, error) {
	if privateKeyHeader.MatchString(text) {
		return Block, "", nil
	}
	out, changed := text, false
	for _, p := range r.patterns {
		if p.re.MatchString(out) {
			out, changed = p.re.ReplaceAllString(out, RedactionMarker), true
		}
	}
	if changed {
		return Redact, out, nil
	}
	return Allow, text, nil
}

// LeakShingle is the run of consecutive words that counts as quoting the
// system prompt.
const LeakShingle = 8

// LeakDetector blocks answers that quote the system prompt (GRD-AC-009).
// The prompt holds no secrets (GRD-REQ-004), but repeating it is the
// signature of a successful extraction attempt, so it is refused.
type LeakDetector struct {
	shingles map[string]bool
}

// NewLeakDetector indexes every LeakShingle-word run of the prompt.
func NewLeakDetector(systemPrompt string) *LeakDetector {
	words := normWords(systemPrompt)
	d := &LeakDetector{shingles: map[string]bool{}}
	for i := 0; i+LeakShingle <= len(words); i++ {
		d.shingles[strings.Join(words[i:i+LeakShingle], " ")] = true
	}
	return d
}

// Name implements Guard.
func (*LeakDetector) Name() string { return StageOutputLeak }

// Check implements Guard.
func (d *LeakDetector) Check(_ context.Context, text string) (Verdict, string, error) {
	words := normWords(text)
	for i := 0; i+LeakShingle <= len(words); i++ {
		if d.shingles[strings.Join(words[i:i+LeakShingle], " ")] {
			return Block, "", nil
		}
	}
	return Allow, text, nil
}

func normWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// OutputMode selects how much of the answer is held back (design §6).
type OutputMode string

// Output modes.
const (
	// Windowed holds back the last WindowBytes of text. A Block decided
	// after earlier text was released stops the stream with a refusal the
	// client must honour by discarding what it rendered.
	Windowed OutputMode = "windowed"
	// Strict holds back the whole answer until every output stage, the
	// final ones included, has passed: no unguarded byte reaches the client.
	Strict OutputMode = "strict"
)

// DefaultWindowBytes is ≈256 tokens.
const DefaultWindowBytes = 1024

// MinWindowBytes is the smallest window allowed. A pattern is only
// guaranteed to be caught before any of it is released if it is shorter
// than the window; every secret pattern and leak shingle above fits in
// 256 bytes (a very long JWT may not: strict mode covers that).
const MinWindowBytes = 256

// Output runs the streaming output stages over a bounded buffer. Text is
// released only once it is more than the window behind the newest token,
// so any pattern shorter than the window is complete (and redacted)
// before any of it leaves.
type Output struct {
	stages    []Guard
	mode      OutputMode
	window    int
	pending   string
	released  strings.Builder
	redacted  bool
	unchecked int
}

// NewOutput builds a filter for one answer.
func NewOutput(mode OutputMode, windowBytes int, stages ...Guard) *Output {
	if windowBytes <= 0 {
		windowBytes = DefaultWindowBytes
	}
	windowBytes = max(windowBytes, MinWindowBytes)
	if mode != Strict {
		mode = Windowed
	}
	return &Output{stages: stages, mode: mode, window: windowBytes}
}

// checkEvery is how much new text accumulates between stage runs. Text is
// only ever released right after a run, with the full window still held
// behind it, so batching changes cost, not safety (GRD-NFR-001).
const checkEvery = 128

// Write adds a fragment and returns the text that may be sent now. On a
// Block the stream must stop; nothing more is returned.
func (o *Output) Write(ctx context.Context, fragment string) (string, Decision) {
	o.pending += fragment
	o.unchecked += len(fragment)
	if o.unchecked < checkEvery {
		return "", Decision{Verdict: Allow}
	}
	d := o.check(ctx)
	if d.Blocked() || o.mode == Strict || len(o.pending) <= o.window {
		return "", d
	}
	cut := len(o.pending) - o.window
	for cut > 0 && !utf8.RuneStart(o.pending[cut]) {
		cut--
	}
	emit := o.pending[:cut]
	o.pending = o.pending[cut:]
	o.released.WriteString(emit)
	return emit, d
}

// check runs the streaming stages over the held-back text.
func (o *Output) check(ctx context.Context) Decision {
	o.unchecked = 0
	d := Evaluate(ctx, o.pending, o.stages...)
	if d.Blocked() {
		o.pending = ""
		return d
	}
	if d.Verdict == Redact {
		o.redacted = true
	}
	o.pending = d.Text
	return d
}

// Text is the full cleaned answer so far (released and pending).
func (o *Output) Text() string { return o.released.String() + o.pending }

// Released reports whether any text has already been sent.
func (o *Output) Released() bool { return o.released.Len() > 0 }

// Redacted reports whether any stage redacted text.
func (o *Output) Redacted() bool { return o.redacted }

// Finish runs the final stages over the whole cleaned answer and returns
// the remaining text to send. On a Block nothing is returned; if text was
// already released (windowed mode) the caller must send a refusal.
func (o *Output) Finish(ctx context.Context, final ...Guard) (string, Decision) {
	if d := o.check(ctx); d.Blocked() { // the tail written since the last run
		return "", d
	}
	d := Evaluate(ctx, o.Text(), final...)
	if d.Blocked() {
		o.pending = ""
		return "", d
	}
	rest := o.pending
	if d.Verdict == Redact { // a final stage cleaned the text
		o.redacted = true
		full := d.Text
		done := o.released.Len()
		if done <= len(full) && full[:done] == o.released.String() {
			rest = full[done:]
		} else {
			// The cleaned text no longer extends what was sent: refuse rather
			// than send an inconsistent remainder.
			return "", Decision{Verdict: Block, Stage: d.Stage, Reason: "redaction_after_release"}
		}
	}
	o.released.WriteString(rest)
	o.pending = ""
	return rest, d
}
