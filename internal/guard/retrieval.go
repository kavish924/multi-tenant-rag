package guard

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
)

// Violation describes a broken retrieval invariant. It never carries
// document text.
type Violation struct {
	Kind    string // foreign_tenant, deleted_document, document_cap, duplicate_chunk
	ChunkID uuid.UUID
}

func (v Violation) Error() string {
	return fmt.Sprintf("retrieval hygiene: %s (chunk %s)", v.Kind, v.ChunkID)
}

// AssertHygiene checks the post-search invariants (GRD-REQ-003): every hit
// belongs to tenantID, none is from a deleted document, no document has
// more than perDocCap hits, and no chunk repeats. The SQL filters and RLS
// already guarantee these, so a violation means a bug: the ask is blocked
// and an alert is logged (GRD-AC-004).
func AssertHygiene(tenantID uuid.UUID, hits []retrieval.Hit, perDocCap int) error {
	perDoc := map[uuid.UUID]int{}
	seen := map[uuid.UUID]bool{}
	for _, h := range hits {
		switch {
		case h.TenantID != tenantID:
			return Violation{Kind: "foreign_tenant", ChunkID: h.ChunkID}
		case h.Deleted:
			return Violation{Kind: "deleted_document", ChunkID: h.ChunkID}
		case seen[h.ChunkID]:
			return Violation{Kind: "duplicate_chunk", ChunkID: h.ChunkID}
		}
		seen[h.ChunkID] = true
		perDoc[h.DocumentID]++
		if perDocCap > 0 && perDoc[h.DocumentID] > perDocCap {
			return Violation{Kind: "document_cap", ChunkID: h.ChunkID}
		}
	}
	return nil
}
