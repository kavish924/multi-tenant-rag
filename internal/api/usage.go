package api

import (
	"net/http"

	"github.com/kavish924/multi-tenant-rag/internal/httpx"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

// usage serves GET /tenants/{tenant}/usage (QTA-REQ-006): current-window
// counters and limits per dimension plus this month's estimated cost.
func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	m, _ := tenants.FromContext(r.Context())
	rep, err := s.Quotas.Report(r.Context(), m.TenantID)
	if err != nil {
		s.internal(w, r, "usage report", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}
