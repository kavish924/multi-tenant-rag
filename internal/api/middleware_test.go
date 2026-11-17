package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

type fakeTokens map[string]uuid.UUID

func (f fakeTokens) Verify(raw string) (uuid.UUID, error) {
	if id, ok := f[raw]; ok {
		return id, nil
	}
	return uuid.Nil, auth.ErrInvalidToken
}

type fakeMemberships map[uuid.UUID][]tenants.Membership

func (f fakeMemberships) MembershipsFor(_ context.Context, u uuid.UUID) ([]tenants.Membership, error) {
	return f[u], nil
}

var (
	userA    = uuid.New()
	userB    = uuid.New()
	tenantA  = uuid.New()
	tenantA2 = uuid.New()
	tenantB  = uuid.New()
)

// chain builds Authenticate → ResolveTenant → Authorize(min) around a
// handler that echoes the resolved tenant id, on mux routes with {tenant}
// and with a non-tenant {id}.
func chain(min tenants.Role) http.Handler {
	tokens := fakeTokens{"tok-a": userA, "tok-b": userB}
	ms := fakeMemberships{
		userA: {{TenantID: tenantA, Role: tenants.RoleMember}, {TenantID: tenantA2, Role: tenants.RoleOwner}},
		userB: {{TenantID: tenantB, Role: tenants.RoleAdmin}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, _ := tenants.FromContext(r.Context())
		_, _ = io.WriteString(w, m.TenantID.String())
	})
	h := Authenticate(tokens)(ResolveTenant(ms, log)(Authorize(min)(echo)))
	mux := http.NewServeMux()
	mux.Handle("GET /tenants/{tenant}/x", h)
	mux.Handle("GET /x", h)
	mux.Handle("GET /docs/{id}", h)
	return mux
}

func do(h http.Handler, path, token string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TEN-AC-001 / TEN-AC-002 at the middleware level.
func TestAuthenticate(t *testing.T) {
	h := chain(tenants.RoleMember)
	for name, hdr := range map[string]string{
		"missing":      "",
		"bad token":    "Bearer nope",
		"wrong scheme": "Basic tok-a",
		"no token":     "Bearer ",
	} {
		req := httptest.NewRequest(http.MethodGet, "/tenants/"+tenantA.String()+"/x", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
	}
}

// TEN-AC-003: non-member tenant, unknown tenant and junk ids all look alike.
func TestResolveTenantHidesNonMemberTenants(t *testing.T) {
	h := chain(tenants.RoleMember)
	for _, id := range []string{tenantB.String(), uuid.NewString(), "not-a-uuid"} {
		if rec := do(h, "/tenants/"+id+"/x", "tok-a", nil); rec.Code != http.StatusNotFound {
			t.Errorf("tenant %s: status %d, want 404", id, rec.Code)
		}
	}
}

// TEN-AC-005: a forged header never overrides the path, and never grants a
// tenant the user is not a member of.
func TestResolveTenantIgnoresForgedSelector(t *testing.T) {
	h := chain(tenants.RoleMember)

	rec := do(h, "/tenants/"+tenantA.String()+"/x", "tok-a", map[string]string{TenantHeader: tenantB.String()})
	if rec.Code != http.StatusOK || rec.Body.String() != tenantA.String() {
		t.Errorf("path route with forged header: %d %q, want 200 %s", rec.Code, rec.Body, tenantA)
	}

	rec = do(h, "/x", "tok-a", map[string]string{TenantHeader: tenantB.String()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("header naming another tenant: status %d, want 404", rec.Code)
	}

	rec = do(h, "/x?tenant_id="+tenantB.String(), "tok-b", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != tenantB.String() {
		t.Errorf("query param must be ignored: %d %q", rec.Code, rec.Body)
	}
}

func TestResolveTenantSelection(t *testing.T) {
	h := chain(tenants.RoleMember)

	if rec := do(h, "/x", "tok-b", nil); rec.Code != http.StatusOK || rec.Body.String() != tenantB.String() {
		t.Errorf("sole membership default: %d %q", rec.Code, rec.Body)
	}
	if rec := do(h, "/x", "tok-a", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("ambiguous without header: status %d, want 400", rec.Code)
	}
	if rec := do(h, "/x", "tok-a", map[string]string{TenantHeader: tenantA2.String()}); rec.Code != http.StatusOK || rec.Body.String() != tenantA2.String() {
		t.Errorf("header selects own tenant: %d %q", rec.Code, rec.Body)
	}
}

// TEN-AC-004: role matrix.
func TestAuthorize(t *testing.T) {
	cases := []struct {
		min    tenants.Role
		tenant uuid.UUID
		want   int
	}{
		{tenants.RoleMember, tenantA, http.StatusOK},
		{tenants.RoleAdmin, tenantA, http.StatusForbidden},
		{tenants.RoleOwner, tenantA, http.StatusForbidden},
		{tenants.RoleOwner, tenantA2, http.StatusOK},
	}
	for _, c := range cases {
		rec := do(chain(c.min), "/tenants/"+c.tenant.String()+"/x", "tok-a", nil)
		if rec.Code != c.want {
			t.Errorf("min %s on %s: status %d, want %d", c.min, c.tenant, rec.Code, c.want)
		}
	}
}

func TestAuthorizeFailsClosedWithoutTenant(t *testing.T) {
	h := Authorize(tenants.RoleMember)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler reached without a resolved tenant")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}

type errMemberships struct{}

func (errMemberships) MembershipsFor(context.Context, uuid.UUID) ([]tenants.Membership, error) {
	return nil, errors.New("db down")
}

func TestResolveTenantDBErrorIs500(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := ResolveTenant(errMemberships{}, log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler reached on lookup error")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(auth.WithUser(req.Context(), userA))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
}

// A non-tenant path id (for example a document id) is never used as the
// tenant selector.
func TestResolveTenantIgnoresNonTenantPathID(t *testing.T) {
	h := chain(tenants.RoleMember)
	rec := do(h, "/docs/"+tenantB.String(), "tok-b", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != tenantB.String() {
		t.Fatalf("sole membership: %d %q", rec.Code, rec.Body)
	}
	// userA has two tenants and no header: the {id} must not pick one.
	if rec := do(h, "/docs/"+tenantA.String(), "tok-a", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("document id used as tenant selector: status %d", rec.Code)
	}
}
