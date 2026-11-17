package api

import (
	"net/http"

	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/httpx"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

// HeaderConfirmPurge must repeat the tenant id: a second, deliberate
// confirmation for an irreversible operation (spec 013 §5).
const HeaderConfirmPurge = "X-Confirm-Purge"

// purgeTenant serves POST /tenants/{tenant}/purge (owner only, DEL-REQ-005).
func (s *server) purgeTenant(w http.ResponseWriter, r *http.Request) {
	m, _ := tenants.FromContext(r.Context())
	if r.Header.Get(HeaderConfirmPurge) != m.TenantID.String() {
		httpx.Error(w, http.StatusBadRequest, "confirmation_required",
			"repeat the tenant id in the "+HeaderConfirmPurge+" header to confirm the purge")
		return
	}
	user, _ := auth.UserFrom(r.Context())
	id, err := s.Documents.RequestPurge(r.Context(), m.TenantID, user)
	if err != nil {
		s.internal(w, r, "purge", err)
		return
	}
	s.Log.WarnContext(r.Context(), "tenant purge requested", "tenant_id", m.TenantID, "user_id", user, "job_id", id)
	httpx.JSON(w, http.StatusAccepted, map[string]any{"job_id": id, "status": "queued"})
}
