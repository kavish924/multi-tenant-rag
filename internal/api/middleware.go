package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/httpx"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

// TenantHeader is the optional tenant selector for routes without a tenant
// id in the path. It is honored only after membership verification.
const TenantHeader = "X-Tenant-ID"

// TokenVerifier verifies access tokens.
type TokenVerifier interface {
	Verify(raw string) (uuid.UUID, error)
}

// MembershipLister lists a user's memberships (memberships_for_user).
type MembershipLister interface {
	MembershipsFor(ctx context.Context, userID uuid.UUID) ([]tenants.Membership, error)
}

// Authenticate rejects requests without a valid bearer token with 401 before
// any tenant data is touched (TEN-AC-001, TEN-AC-002).
func Authenticate(tokens TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
				httpx.Unauthorized(w)
				return
			}
			userID, err := tokens.Verify(strings.TrimSpace(raw))
			if err != nil {
				httpx.Unauthorized(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), userID)))
		})
	}
}

// ResolveTenant picks the request's tenant from the verified membership list
// (TEN-REQ-006). The selector is the {id} path value when the route has one,
// otherwise the X-Tenant-ID header, otherwise the user's sole membership.
// A selector the user is not a member of is answered 404, hiding whether the
// tenant exists (TEN-AC-003). Client input only selects among the user's own
// memberships; it never grants access (TEN-AC-005).
func ResolveTenant(ms MembershipLister, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := auth.UserFrom(r.Context())
			if !ok {
				httpx.Unauthorized(w)
				return
			}
			memberships, err := ms.MembershipsFor(r.Context(), userID)
			if err != nil {
				log.ErrorContext(r.Context(), "resolve tenant", "err", err)
				httpx.Internal(w)
				return
			}

			selector := r.PathValue("id")
			if selector == "" {
				selector = strings.TrimSpace(r.Header.Get(TenantHeader))
			}

			var chosen tenants.Membership
			switch {
			case selector != "":
				id, err := uuid.Parse(selector)
				if err != nil {
					httpx.NotFound(w)
					return
				}
				found := false
				for _, m := range memberships {
					if m.TenantID == id {
						chosen, found = m, true
						break
					}
				}
				if !found {
					httpx.NotFound(w)
					return
				}
			case len(memberships) == 1:
				chosen = memberships[0]
			case len(memberships) == 0:
				httpx.NotFound(w)
				return
			default:
				httpx.Error(w, http.StatusBadRequest, "tenant_required",
					"you belong to several tenants; send the "+TenantHeader+" header")
				return
			}
			next.ServeHTTP(w, r.WithContext(tenants.WithMembership(r.Context(), chosen)))
		})
	}
}

// Authorize enforces the minimum role for a route (TEN-AC-004).
func Authorize(min tenants.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m, ok := tenants.FromContext(r.Context())
			if !ok {
				// ResolveTenant must run first; failing closed hides the route.
				httpx.NotFound(w)
				return
			}
			if !m.Role.AtLeast(min) {
				httpx.Forbidden(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests logs one line per request and recovers from handler panics.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				log.ErrorContext(r.Context(), "panic", "panic", p, "path", r.URL.Path)
				httpx.Internal(rec)
			}
			log.InfoContext(r.Context(), "request",
				"method", r.Method,
				"route", r.Pattern,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}
