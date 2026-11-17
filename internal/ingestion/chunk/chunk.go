// Package chunk splits extracted sections into overlapping passages
// (ING-REQ-003) with stable ordinals (ING-REQ-004).
package chunk

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
)

// Chunk is one passage to embed and index.
type Chunk struct {
	Ordinal    int
	Content    string
	TokenCount int
	Page       int // 0 = unknown
	Heading    string
}

// Options tune chunk size. Chunk size is a tuning knob for the evaluation
// set (014), not a constant.
type Options struct {
	MinTokens    int     // a chunk is not cut at a soft boundary below this
	TargetTokens int     // cut at the next paragraph boundary after this
	MaxTokens    int     // hard ceiling
	Overlap      float64 // fraction of the previous chunk repeated (0.10–0.15)
}

// Defaults follow system-design §3: 300–800 tokens, ~12% overlap.
var Defaults = Options{MinTokens: 300, TargetTokens: 500, MaxTokens: 800, Overlap: 0.12}

// CountTokens is the tokenizer: deterministic and dependency-free. Each run
// of letters/digits counts as one token per started 6 characters (roughly
// how BPE tokenizers split long words), and each other non-space character
// counts as one token. It is consistent, not identical to any model's
// tokenizer; chunk sizes are approximate by design.
func CountTokens(s string) int {
	n, run := 0, 0
	endRun := func() {
		if run > 0 {
			n += (run + 5) / 6
			run = 0
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			run++
		case unicode.IsSpace(r):
			endRun()
		default:
			endRun()
			n++
		}
	}
	endRun()
	return n
}

// unit is a sentence (or a hard-split piece of one) with its position.
type unit struct {
	text    string
	tokens  int
	page    int
	heading string
	paraEnd bool // last unit of a paragraph
}

// Split turns sections into chunks. The same input always yields the same
// chunks and ordinals.
func Split(secs []extract.Section, o Options) []Chunk {
	if o.MaxTokens <= 0 {
		o = Defaults
	}
	units := toUnits(secs, o.MaxTokens)

	var (
		out      []Chunk
		cur      []unit // overlap units followed by new units
		curTok   int
		firstNew = -1 // index in cur of the first new unit
	)
	flush := func(keepOverlap bool) {
		if firstNew < 0 {
			return
		}
		out = append(out, build(len(out), cur, firstNew))
		var tail []unit
		tailTok := 0
		if keepOverlap {
			budget := int(math.Ceil(o.Overlap * float64(curTok)))
			for i := len(cur) - 1; i >= firstNew; i-- {
				if tailTok+cur[i].tokens > budget {
					break
				}
				tail = append([]unit{cur[i]}, tail...)
				tailTok += cur[i].tokens
			}
		}
		cur, curTok, firstNew = tail, tailTok, -1
	}

	for _, u := range units {
		if firstNew >= 0 {
			newTok := curTok
			for _, x := range cur[:firstNew] {
				newTok -= x.tokens
			}
			sectionChange := u.heading != cur[firstNew].heading
			switch {
			case sectionChange && newTok >= o.MinTokens:
				flush(false) // never carry text across a heading
			case curTok+u.tokens > o.MaxTokens:
				flush(true)
			case cur[len(cur)-1].paraEnd && curTok >= o.TargetTokens:
				flush(true)
			}
		}
		if firstNew < 0 && curTok+u.tokens > o.MaxTokens {
			cur, curTok = nil, 0 // overlap would push past the ceiling
		}
		if firstNew < 0 {
			firstNew = len(cur)
		}
		cur = append(cur, u)
		curTok += u.tokens
	}
	flush(false)
	return out
}

func build(ordinal int, units []unit, firstNew int) Chunk {
	var b strings.Builder
	tokens := 0
	for i, u := range units {
		if i > 0 {
			if units[i-1].paraEnd {
				b.WriteString("\n\n")
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteString(u.text)
		tokens += u.tokens
	}
	return Chunk{
		Ordinal:    ordinal,
		Content:    b.String(),
		TokenCount: tokens,
		Page:       units[firstNew].page,
		Heading:    units[firstNew].heading,
	}
}

func toUnits(secs []extract.Section, maxTokens int) []unit {
	var units []unit
	for _, s := range secs {
		for _, para := range strings.Split(s.Text, "\n\n") {
			para = strings.Join(strings.Fields(para), " ")
			if para == "" {
				continue
			}
			sents := sentences(para)
			for i, sent := range sents {
				pieces := hardSplit(sent, maxTokens)
				for j, p := range pieces {
					units = append(units, unit{
						text:    p,
						tokens:  max(1, CountTokens(p)),
						page:    s.Page,
						heading: s.Heading,
						paraEnd: i == len(sents)-1 && j == len(pieces)-1,
					})
				}
			}
		}
	}
	return units
}

// sentences splits after '.', '!' or '?' followed by a space and an
// upper-case letter or digit, which avoids most abbreviation splits.
func sentences(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRuneInString(p[i:])
		i += size
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if i >= len(p) || p[i] != ' ' {
			continue
		}
		next, _ := utf8.DecodeRuneInString(p[i+1:])
		if unicode.IsUpper(next) || unicode.IsDigit(next) {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	if start < len(p) {
		out = append(out, p[start:])
	}
	return out
}

// hardSplit cuts a sentence longer than maxTokens at word boundaries (or
// inside an enormous word) into pieces of at most maxTokens.
func hardSplit(s string, maxTokens int) []string {
	if CountTokens(s) <= maxTokens {
		return []string{s}
	}
	var out []string
	var cur []string
	curTok := 0
	for _, w := range strings.Fields(s) {
		for CountTokens(w) > maxTokens { // pathological single "word"
			cut := cutAtTokens(w, maxTokens)
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, " "))
				cur, curTok = nil, 0
			}
			out = append(out, w[:cut])
			w = w[cut:]
		}
		t := CountTokens(w)
		if curTok+t > maxTokens && len(cur) > 0 {
			out = append(out, strings.Join(cur, " "))
			cur, curTok = nil, 0
		}
		cur = append(cur, w)
		curTok += t
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, " "))
	}
	return out
}

// cutAtTokens returns the byte offset (on a rune boundary) of the longest
// prefix of w holding at most maxTokens tokens, counting exactly as
// CountTokens does; always at least one rune.
func cutAtTokens(w string, maxTokens int) int {
	n, run := 0, 0
	for i, r := range w {
		add := 0
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if run%6 == 0 {
				add = 1
			}
			run++
		case unicode.IsSpace(r):
			run = 0
		default:
			run = 0
			add = 1
		}
		if n+add > maxTokens {
			if i == 0 {
				_, size := utf8.DecodeRuneInString(w)
				return size
			}
			return i
		}
		n += add
	}
	return len(w)
}
