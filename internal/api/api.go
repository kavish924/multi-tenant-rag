// Package api wires the HTTP routes of cmd/api: the auth endpoints, tenant
// and membership endpoints, and the middleware chain
// Authenticate → ResolveTenant → Authorize(minRole) (TEN-REQ-005).
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/documents"
	"github.com/kavish924/multi-tenant-rag/internal/httpx"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

// Deps are the services the API needs.
type Deps struct {
	Users     *auth.Service
	Tokens    TokenVerifier
	Tenants   *tenants.Service
	Documents *documents.Service  // optional: document routes are mounted only when set
	Retriever retrieval.Retriever // optional: POST /search is mounted only when set
	Ready     func(context.Context) error
	Log       *slog.Logger
}

type server struct{ Deps }

// New returns the API handler.
func New(d Deps) http.Handler {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &server{d}
	authn := Authenticate(d.Tokens)
	resolve := ResolveTenant(d.Tenants, d.Log)
	tenantRoute := func(min tenants.Role, h http.HandlerFunc) http.Handler {
		return authn(resolve(Authorize(min)(h)))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)

	mux.HandleFunc("POST /auth/register", s.register)
	mux.HandleFunc("POST /auth/login", s.login)
	mux.Handle("GET /me", authn(http.HandlerFunc(s.me)))

	mux.Handle("POST /tenants", authn(http.HandlerFunc(s.createTenant)))
	mux.Handle("GET /tenants/{tenant}/members", tenantRoute(tenants.RoleAdmin, s.listMembers))
	mux.Handle("POST /tenants/{tenant}/members", tenantRoute(tenants.RoleAdmin, s.addMember))
	mux.Handle("PATCH /tenants/{tenant}/members/{uid}", tenantRoute(tenants.RoleOwner, s.changeRole))
	mux.Handle("DELETE /tenants/{tenant}/members/{uid}", tenantRoute(tenants.RoleOwner, s.removeMember))

	// Documents live in the tenant chosen by X-Tenant-ID (or the sole
	// membership); {id} is always a document id here.
	if d.Documents != nil {
		mux.Handle("POST /documents", tenantRoute(tenants.RoleMember, s.uploadDocument))
		mux.Handle("GET /documents", tenantRoute(tenants.RoleMember, s.listDocuments))
		mux.Handle("GET /documents/{id}", tenantRoute(tenants.RoleMember, s.getDocument))
		mux.Handle("DELETE /documents/{id}", tenantRoute(tenants.RoleMember, s.deleteDocument))
		mux.Handle("GET /documents/{id}/content", tenantRoute(tenants.RoleMember, s.documentContent))
	}
	if d.Retriever != nil {
		mux.Handle("POST /search", tenantRoute(tenants.RoleMember, s.search))
	}

	return logRequests(d.Log, mux)
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.Ready != nil {
		if err := s.Ready(ctx); err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "not_ready", "database unavailable")
			return
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !httpx.Decode(w, r, &in) {
		return
	}
	u, err := s.Users.Register(r.Context(), in.Email, in.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidEmail):
		httpx.Error(w, http.StatusBadRequest, "invalid_email", err.Error())
	case errors.Is(err, auth.ErrWeakPassword):
		httpx.Error(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, auth.ErrEmailTaken):
		httpx.Error(w, http.StatusConflict, "email_taken", err.Error())
	case err != nil:
		s.internal(w, r, "register", err)
	default:
		httpx.JSON(w, http.StatusCreated, u)
	}
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !httpx.Decode(w, r, &in) {
		return
	}
	token, exp, err := s.Users.Login(r.Context(), in.Email, in.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
	case err != nil:
		s.internal(w, r, "login", err)
	default:
		httpx.JSON(w, http.StatusOK, map[string]any{
			"access_token": token,
			"token_type":   "Bearer",
			"expires_at":   exp.UTC(),
		})
	}
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserFrom(r.Context())
	u, err := s.Users.Get(r.Context(), userID)
	if errors.Is(err, auth.ErrUserNotFound) {
		httpx.Unauthorized(w)
		return
	}
	if err != nil {
		s.internal(w, r, "me", err)
		return
	}
	ms, err := s.Tenants.MembershipsFor(r.Context(), userID)
	if err != nil {
		s.internal(w, r, "me memberships", err)
		return
	}
	if ms == nil {
		ms = []tenants.Membership{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": u, "memberships": ms})
}

func (s *server) createTenant(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	userID, _ := auth.UserFrom(r.Context())
	t, err := s.Tenants.Create(r.Context(), userID, in.Name)
	switch {
	case errors.Is(err, tenants.ErrInvalidName):
		httpx.Error(w, http.StatusBadRequest, "invalid_name", err.Error())
	case err != nil:
		s.internal(w, r, "create tenant", err)
	default:
		httpx.JSON(w, http.StatusCreated, t)
	}
}

func (s *server) listMembers(w http.ResponseWriter, r *http.Request) {
	m, _ := tenants.FromContext(r.Context())
	members, err := s.Tenants.ListMembers(r.Context(), m.TenantID)
	if err != nil {
		s.internal(w, r, "list members", err)
		return
	}
	if members == nil {
		members = []tenants.Member{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"members": members})
}

func (s *server) addMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	role, err := tenants.ParseRole(in.Role)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_role", err.Error())
		return
	}
	m, _ := tenants.FromContext(r.Context())
	member, err := s.Tenants.AddMember(r.Context(), m.TenantID, m.Role, in.Email, role)
	switch {
	case errors.Is(err, tenants.ErrOwnerGrant):
		httpx.Error(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, tenants.ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "user_not_found", "no user with that email")
	case errors.Is(err, tenants.ErrAlreadyMember):
		httpx.Error(w, http.StatusConflict, "already_member", err.Error())
	case err != nil:
		s.internal(w, r, "add member", err)
	default:
		httpx.JSON(w, http.StatusCreated, member)
	}
}

func (s *server) changeRole(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathUUID(r, "uid")
	if !ok {
		httpx.NotFound(w)
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	role, err := tenants.ParseRole(in.Role)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_role", err.Error())
		return
	}
	m, _ := tenants.FromContext(r.Context())
	member, err := s.Tenants.ChangeRole(r.Context(), m.TenantID, userID, role)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		httpx.NotFound(w)
	case errors.Is(err, tenants.ErrLastOwner):
		httpx.Error(w, http.StatusConflict, "last_owner", err.Error())
	case err != nil:
		s.internal(w, r, "change role", err)
	default:
		httpx.JSON(w, http.StatusOK, member)
	}
}

func (s *server) removeMember(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathUUID(r, "uid")
	if !ok {
		httpx.NotFound(w)
		return
	}
	m, _ := tenants.FromContext(r.Context())
	err := s.Tenants.RemoveMember(r.Context(), m.TenantID, userID)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		httpx.NotFound(w)
	case errors.Is(err, tenants.ErrLastOwner):
		httpx.Error(w, http.StatusConflict, "last_owner", err.Error())
	case err != nil:
		s.internal(w, r, "remove member", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) internal(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.Log.ErrorContext(r.Context(), op, "err", err)
	httpx.Internal(w)
}

func pathUUID(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	return id, err == nil && id != uuid.Nil
}
