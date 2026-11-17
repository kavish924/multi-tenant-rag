package chunk

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
)

func TestCountTokens(t *testing.T) {
	cases := map[string]int{
		"":                     0,
		"hello world":          2,
		"Hello, world!":        4,
		"internationalization": 4, // 20 letters → 4 pieces of ≤6
		"a-b":                  3,
		"   ":                  0,
		"30 days.":             3,
	}
	for in, want := range cases {
		if got := CountTokens(in); got != want {
			t.Errorf("CountTokens(%q) = %d, want %d", in, got, want)
		}
	}
}

// sentence of exactly n tokens ("w1 w2 ... wn." counts n words + 1 period).
func sentence(r *rand.Rand, n int) string {
	words := make([]string, n-1)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", r.IntN(1000))
	}
	return "S" + strings.Join(words, " ") + "."
}

func doc(seed uint64, paragraphs, sentencesPer, tokensPer int) []extract.Section {
	r := rand.New(rand.NewPCG(seed, seed))
	var paras []string
	for p := 0; p < paragraphs; p++ {
		var s []string
		for i := 0; i < sentencesPer; i++ {
			s = append(s, sentence(r, tokensPer))
		}
		paras = append(paras, strings.Join(s, " "))
	}
	return []extract.Section{{Page: 1, Heading: "H", Text: strings.Join(paras, "\n\n")}}
}

// ING-REQ-003: sizes stay in range and every chunk respects the ceiling.
func TestSplitSizes(t *testing.T) {
	chunks := Split(doc(1, 40, 6, 25), Defaults)
	if len(chunks) < 5 {
		t.Fatalf("only %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if c.TokenCount > Defaults.MaxTokens {
			t.Errorf("chunk %d has %d tokens > max", i, c.TokenCount)
		}
		if i < len(chunks)-1 && c.TokenCount < Defaults.MinTokens {
			t.Errorf("chunk %d has %d tokens < min", i, c.TokenCount)
		}
		if c.Ordinal != i || c.Page != 1 || c.Heading != "H" {
			t.Errorf("chunk %d metadata %+v", i, c)
		}
		if c.TokenCount != CountTokens(c.Content) {
			t.Errorf("chunk %d token_count %d != CountTokens(content) %d", i, c.TokenCount, CountTokens(c.Content))
		}
	}
}

// ING-REQ-003: consecutive chunks overlap by roughly 10–15%.
func TestSplitOverlap(t *testing.T) {
	chunks := Split(doc(2, 40, 6, 20), Defaults)
	for i := 1; i < len(chunks); i++ {
		prev, next := chunks[i-1].Content, chunks[i].Content
		overlap := 0
		for k := min(len(prev), len(next)); k > 0; k-- {
			if strings.HasSuffix(prev, next[:k]) {
				overlap = CountTokens(next[:k])
				break
			}
		}
		ratio := float64(overlap) / float64(chunks[i-1].TokenCount)
		if ratio == 0 || ratio > 0.15 {
			t.Errorf("chunk %d overlap %.2f outside (0, 0.15]", i, ratio)
		}
	}
}

// ING-REQ-004: identical input → identical chunks and ordinals.
func TestSplitIsDeterministic(t *testing.T) {
	a := Split(doc(3, 30, 5, 22), Defaults)
	b := Split(doc(3, 30, 5, 22), Defaults)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same input produced different chunks")
	}
	// A change near the end leaves earlier ordinals untouched.
	secs := doc(3, 30, 5, 22)
	secs[0].Text += "\n\nAppended closing paragraph."
	c := Split(secs, Defaults)
	for i := 0; i < len(a)-1; i++ {
		if a[i] != c[i] {
			t.Fatalf("ordinal %d changed after an append at the end", i)
		}
	}
}

// Headings start a new chunk once the current one is big enough, and text
// is never carried across a heading.
func TestSplitRespectsHeadings(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 4))
	var secs []extract.Section
	for h := 0; h < 3; h++ {
		var s []string
		for i := 0; i < 20; i++ {
			s = append(s, sentence(r, 20))
		}
		secs = append(secs, extract.Section{Page: h + 1, Heading: fmt.Sprintf("Part %d", h), Text: strings.Join(s, " ")})
	}
	chunks := Split(secs, Defaults)
	seen := map[string]bool{}
	for _, c := range chunks {
		seen[c.Heading] = true
		for h := 0; h < 3; h++ {
			if c.Heading != fmt.Sprintf("Part %d", h) && strings.Contains(c.Content, secs[h].Text[:40]) {
				t.Errorf("chunk under %q contains the start of Part %d", c.Heading, h)
			}
		}
	}
	if len(seen) != 3 {
		t.Errorf("headings seen %v", seen)
	}
}

func TestSplitHugeSentenceAndWord(t *testing.T) {
	giant := strings.Repeat("a", 6*2000)                     // one 2000-token "word"
	long := strings.TrimSpace(strings.Repeat("word ", 3000)) // one 3000-token sentence
	chunks := Split([]extract.Section{{Text: giant + "\n\n" + long}}, Defaults)
	total := 0
	for _, c := range chunks {
		if c.TokenCount > Defaults.MaxTokens {
			t.Fatalf("chunk with %d tokens", c.TokenCount)
		}
		total += c.TokenCount
	}
	if total < 5000 {
		t.Errorf("content lost: %d tokens across chunks", total)
	}
}

func TestSplitSmallAndEmpty(t *testing.T) {
	if got := Split(nil, Defaults); len(got) != 0 {
		t.Errorf("nil input → %d chunks", len(got))
	}
	got := Split([]extract.Section{{Page: 3, Heading: "Only", Text: "Short note."}}, Defaults)
	if len(got) != 1 || got[0].Content != "Short note." || got[0].Page != 3 || got[0].Heading != "Only" {
		t.Errorf("%+v", got)
	}
}
