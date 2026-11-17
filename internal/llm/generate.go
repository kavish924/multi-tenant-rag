package llm

import (
	"context"
	"errors"
	"html"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// GenerateRequest is one grounded generation call. System carries the
// rules; Prompt carries the delimited sources and the question.
type GenerateRequest struct {
	System          string
	Prompt          string
	MaxOutputTokens int // zero = provider default
}

// Usage is a call's token usage. Providers report exact counts when the
// stream ends; on cancellation it holds what was consumed so far.
type Usage struct {
	InputTokens  int `json:"input"`
	OutputTokens int `json:"output"`
}

// Stream is the single stream abstraction (docs/tech-stack.md): token
// events, one terminal error, and final usage.
type Stream interface {
	// Recv returns the next text fragment, io.EOF after the last one, or
	// the terminal error (ctx.Err() when the caller's context ended).
	Recv() (string, error)
	// Usage is valid once Recv has returned an error (including io.EOF).
	Usage() Usage
	// Close releases the stream; it cancels an unfinished provider call.
	Close() error
}

// Generator starts streamed generations. The context governs the whole
// stream: cancelling it stops the provider call and its token spend
// (GEN-REQ-004).
type Generator interface {
	Stream(ctx context.Context, req GenerateRequest) (Stream, error)
	Model() string
}

// ApproxTokens estimates tokens as one per four bytes (rounded up), the
// usual rule of thumb; fakes use it, real providers report exact counts.
func ApproxTokens(s string) int {
	return (len(s) + 3) / 4
}

// FakeGenerator is a deterministic generator for development and tests. By
// default it answers extractively: the first sentence of source 1, cited
// as [1], or an insufficient-context reply when the prompt has no sources.
type FakeGenerator struct {
	// Respond overrides the answer text.
	Respond func(GenerateRequest) string
	// Delay is waited before each token (to test streaming and cancellation).
	Delay time.Duration
	// FailAfter, when > 0, fails the stream with ErrFakeFailure after that
	// many tokens.
	FailAfter int
	// Started, when set, receives each stream's context (cancellation tests).
	Started func(ctx context.Context)
}

// ErrFakeFailure is the injected mid-stream failure.
var ErrFakeFailure = errors.New("llm: fake generator failure")

// Model implements Generator.
func (f FakeGenerator) Model() string { return "fake-generator-v1" }

// Stream implements Generator.
func (f FakeGenerator) Stream(ctx context.Context, req GenerateRequest) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.Started != nil {
		f.Started(ctx)
	}
	answer := ""
	if f.Respond != nil {
		answer = f.Respond(req)
	} else {
		answer = extractiveAnswer(req.Prompt)
	}
	return &fakeStream{
		ctx: ctx, tokens: splitTokens(answer), delay: f.Delay, failAfter: f.FailAfter,
		usage: Usage{InputTokens: ApproxTokens(req.System) + ApproxTokens(req.Prompt)},
	}, nil
}

type fakeStream struct {
	ctx       context.Context
	tokens    []string
	next      int
	delay     time.Duration
	failAfter int
	usage     Usage
	done      bool
}

func (s *fakeStream) Recv() (string, error) {
	if s.done {
		return "", io.EOF
	}
	if err := s.ctx.Err(); err != nil {
		return "", err
	}
	if s.failAfter > 0 && s.next >= s.failAfter {
		return "", ErrFakeFailure
	}
	if s.next >= len(s.tokens) {
		s.done = true
		return "", io.EOF
	}
	if s.delay > 0 {
		t := time.NewTimer(s.delay)
		select {
		case <-s.ctx.Done():
			t.Stop()
			return "", s.ctx.Err()
		case <-t.C:
		}
	}
	tok := s.tokens[s.next]
	s.next++
	s.usage.OutputTokens += ApproxTokens(tok)
	return tok, nil
}

func (s *fakeStream) Usage() Usage { return s.usage }
func (s *fakeStream) Close() error { s.done = true; return nil }

// splitTokens splits text into word-sized fragments that keep their
// leading whitespace, so concatenating them restores the text.
func splitTokens(s string) []string {
	var out []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i] == ' ' && s[i-1] != ' ' {
			out = append(out, s[start:i])
			start = i
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

var firstSource = regexp.MustCompile(`(?s)<source n="1"[^>]*>\n?(.*?)\n?</source>`)

// InsufficientContext is the fake's (and the zero-hit path's) reply.
const InsufficientContext = "I don't have enough information in your documents to answer that."

func extractiveAnswer(prompt string) string {
	m := firstSource.FindStringSubmatch(prompt)
	if m == nil {
		return InsufficientContext
	}
	text := strings.Join(strings.Fields(html.UnescapeString(m[1])), " ")
	if i := strings.IndexAny(text, ".!?"); i >= 0 {
		text = text[:i+1]
	}
	if utf8.RuneCountInString(text) > 300 {
		text = string([]rune(text)[:300]) + "…"
	}
	return "According to your documents: " + text + " [1]"
}
