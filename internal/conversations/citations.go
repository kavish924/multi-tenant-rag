package conversations

import (
	"regexp"
	"strconv"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
)

// Citation resolves a [n] marker to a chunk retrieved for this request.
type Citation struct {
	N          int       `json:"n"`
	ChunkID    uuid.UUID `json:"chunk_id"`
	DocumentID uuid.UUID `json:"document_id"`
	Filename   string    `json:"filename"`
	Page       int       `json:"page,omitempty"`
	Heading    string    `json:"heading,omitempty"`
}

var marker = regexp.MustCompile(`\[(\d{1,4})\]`)

// ResolveCitations keeps only markers that point at sources of THIS
// request, in order of first appearance (GEN-REQ-005). Out-of-range,
// duplicate and malformed markers are dropped, so the model can never
// invent a source identifier (GEN-AC-003): ids come from sources only.
func ResolveCitations(answer string, sources []retrieval.Hit) []Citation {
	seen := map[int]bool{}
	out := []Citation{}
	for _, m := range marker.FindAllStringSubmatch(answer, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || n > len(sources) || seen[n] {
			continue
		}
		seen[n] = true
		h := sources[n-1]
		out = append(out, Citation{
			N: n, ChunkID: h.ChunkID, DocumentID: h.DocumentID,
			Filename: h.Filename, Page: h.Page, Heading: h.Heading,
		})
	}
	return out
}
