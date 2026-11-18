// Package evaluation is the quality harness (spec 014): a golden question
// set, deterministic retrieval / citation / refusal metrics, an optional
// LLM judge for answer metrics, threshold gating, and A/B comparison.
// cmd/eval wires it to the real pipeline; everything here is pure Go and
// unit-tested.
package evaluation

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Case is one golden question (EVA-REQ-001).
type Case struct {
	ID       string `json:"id"`
	Tenant   string `json:"tenant"`
	Question string `json:"question"`
	// RelevantChunks are stable chunk labels "<tenant>/<file>#<ordinal>"
	// (chunk ordinals are stable across re-ingestion, ING-REQ-004; database
	// ids are not).
	RelevantChunks  []string `json:"relevant_chunks"`
	Refusal         bool     `json:"refusal"`
	ReferenceAnswer string   `json:"reference_answer,omitempty"`
	Type            string   `json:"type"` // lexical, semantic, multi_hop, out_of_scope, cross_tenant
	Fast            bool     `json:"fast,omitempty"`
}

// Case types.
const (
	TypeLexical     = "lexical"
	TypeSemantic    = "semantic"
	TypeMultiHop    = "multi_hop"
	TypeOutOfScope  = "out_of_scope"
	TypeCrossTenant = "cross_tenant"
)

// LoadCases reads a JSONL file; blank lines and lines starting with // are
// skipped.
func LoadCases(path string) ([]Case, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "//") {
			continue
		}
		var c Case
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, Validate(out)
}

// Validate checks the set's shape: ids unique, every case has a tenant and
// a question, refusal cases have no relevant chunks and answerable cases
// have at least one, and labels belong to the case's tenant.
func Validate(cases []Case) error {
	seen := map[string]bool{}
	var errs []error
	for _, c := range cases {
		switch {
		case c.ID == "" || seen[c.ID]:
			errs = append(errs, fmt.Errorf("case %q: missing or duplicate id", c.ID))
		case c.Tenant == "" || strings.TrimSpace(c.Question) == "":
			errs = append(errs, fmt.Errorf("case %s: tenant and question are required", c.ID))
		case c.Refusal && len(c.RelevantChunks) > 0:
			errs = append(errs, fmt.Errorf("case %s: a refusal case has no relevant chunks", c.ID))
		case !c.Refusal && len(c.RelevantChunks) == 0:
			errs = append(errs, fmt.Errorf("case %s: an answerable case needs relevant chunks", c.ID))
		}
		seen[c.ID] = true
		for _, l := range c.RelevantChunks {
			if !strings.HasPrefix(l, c.Tenant+"/") || !strings.Contains(l, "#") {
				errs = append(errs, fmt.Errorf("case %s: label %q is not <tenant>/<file>#<ordinal> of its tenant", c.ID, l))
			}
		}
	}
	return errors.Join(errs...)
}

// Filter keeps the fast subset when fast is true.
func Filter(cases []Case, fast bool) []Case {
	if !fast {
		return cases
	}
	var out []Case
	for _, c := range cases {
		if c.Fast {
			out = append(out, c)
		}
	}
	return out
}

// Label builds a chunk label.
func Label(tenant, file string, ordinal int) string {
	return fmt.Sprintf("%s/%s#%d", tenant, file, ordinal)
}
