//go:build integration

// Package integration exercises the HTTP API against a real PostgreSQL with
// migrations applied and the application connected as a member of `app`.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/api"
	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/conversations"
	"github.com/kavish924/multi-tenant-rag/internal/documents"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
)

var (
	env     *pgtest.Env
	srv     *httptest.Server
	objects *objectstore.FS
)

// maxUpload is small so the size-limit test stays fast.
const maxUpload = 1 << 20

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	e, err := pgtest.Start(ctx)
	cancel()
	if err != nil {
		log.Fatalf("pgtest: %v", err)
	}
	env = e

	tokens := auth.NewTokens([]byte("integration-test-key-0123456789abcdef"), "test", 15*time.Minute)
	users, err := auth.NewService(env.Store.Pool(), tokens)
	if err != nil {
		log.Fatal(err)
	}
	objDir, err := os.MkdirTemp("", "objects-*")
	if err != nil {
		log.Fatal(err)
	}
	objects, err = objectstore.NewFS(objDir)
	if err != nil {
		log.Fatal(err)
	}
	srv = httptest.NewServer(api.New(api.Deps{
		Users:     users,
		Tokens:    tokens,
		Tenants:   tenants.NewService(env.Store),
		Documents: documents.NewService(env.Store, objects, maxUpload),
		Retriever: retrieval.NewHybrid(env.Store, llm.Fake{}, retrieval.Options{}, nil),
		Ask: http.HandlerFunc((&conversations.Handler{
			Retriever:  retrieval.NewHybrid(env.Store, llm.Fake{}, retrieval.Options{}, nil),
			Generator:  llm.FakeGenerator{},
			Usage:      usage.NewRecorder(env.Store),
			EmbedModel: llm.Fake{}.Model(),
			Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		}).Ask),
		Ready: env.Store.Ping,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))

	code := m.Run()
	srv.Close()
	env.Close()
	_ = os.RemoveAll(objDir)
	os.Exit(code)
}

type resp struct {
	Status int
	Body   map[string]any
}

func call(t *testing.T, method, path, token string, body any, header ...string) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{Status: res.StatusCode}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.Body)
	}
	return out
}

func expect(t *testing.T, r resp, status int, what string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s: status %d, want %d (body %v)", what, r.Status, status, r.Body)
	}
}

const password = "a long enough password"

// user registers and logs in, returning (user id, token).
func user(t *testing.T, email string) (string, string) {
	t.Helper()
	r := call(t, "POST", "/auth/register", "", map[string]string{"email": email, "password": password})
	expect(t, r, http.StatusCreated, "register "+email)
	id := r.Body["id"].(string)
	r = call(t, "POST", "/auth/login", "", map[string]string{"email": email, "password": password})
	expect(t, r, http.StatusOK, "login "+email)
	return id, r.Body["access_token"].(string)
}

func createTenant(t *testing.T, token, name string) string {
	t.Helper()
	r := call(t, "POST", "/tenants", token, map[string]string{"name": name})
	expect(t, r, http.StatusCreated, "create tenant")
	return r.Body["id"].(string)
}

func memberEmails(t *testing.T, r resp) []string {
	t.Helper()
	var out []string
	for _, m := range r.Body["members"].([]any) {
		out = append(out, m.(map[string]any)["email"].(string))
	}
	return out
}

// TEN-REQ-001, TEN-REQ-002.
func TestRegisterLoginMe(t *testing.T) {
	env.Truncate(t)

	r := call(t, "POST", "/auth/register", "", map[string]string{"email": "Ann@Example.com", "password": password})
	expect(t, r, http.StatusCreated, "register")
	if r.Body["email"] != "ann@example.com" {
		t.Errorf("email not normalized: %v", r.Body["email"])
	}
	if _, leaked := r.Body["password_hash"]; leaked {
		t.Error("password hash returned to client")
	}

	expect(t, call(t, "POST", "/auth/register", "", map[string]string{"email": "ann@example.com", "password": password}),
		http.StatusConflict, "duplicate register")
	expect(t, call(t, "POST", "/auth/register", "", map[string]string{"email": "bob@example.com", "password": "short"}),
		http.StatusBadRequest, "weak password")
	expect(t, call(t, "POST", "/auth/register", "", map[string]string{"email": "nope", "password": password}),
		http.StatusBadRequest, "bad email")

	expect(t, call(t, "POST", "/auth/login", "", map[string]string{"email": "ann@example.com", "password": "wrong password!!"}),
		http.StatusUnauthorized, "wrong password")
	expect(t, call(t, "POST", "/auth/login", "", map[string]string{"email": "ghost@example.com", "password": password}),
		http.StatusUnauthorized, "unknown user")

	r = call(t, "POST", "/auth/login", "", map[string]string{"email": "ANN@example.com", "password": password})
	expect(t, r, http.StatusOK, "login")
	token := r.Body["access_token"].(string)

	r = call(t, "GET", "/me", token, nil)
	expect(t, r, http.StatusOK, "me")
	if r.Body["user"].(map[string]any)["email"] != "ann@example.com" {
		t.Errorf("me: %v", r.Body)
	}
	if ms := r.Body["memberships"].([]any); len(ms) != 0 {
		t.Errorf("new user has memberships: %v", ms)
	}

	tid := createTenant(t, token, "Ann Co")
	r = call(t, "GET", "/me", token, nil)
	ms := r.Body["memberships"].([]any)
	if len(ms) != 1 || ms[0].(map[string]any)["tenant_id"] != tid || ms[0].(map[string]any)["role"] != "owner" {
		t.Errorf("me after create tenant: %v", ms)
	}
}

// TEN-AC-001, TEN-AC-002 end to end.
func TestUnauthenticated(t *testing.T) {
	env.Truncate(t)
	_, token := user(t, "owner@a.test")
	tid := createTenant(t, token, "A")

	for _, tok := range []string{"", "garbage", token + "x"} {
		expect(t, call(t, "GET", "/me", tok, nil), http.StatusUnauthorized, "me with "+tok)
		expect(t, call(t, "GET", "/tenants/"+tid+"/members", tok, nil), http.StatusUnauthorized, "members with bad token")
		expect(t, call(t, "POST", "/tenants", tok, map[string]string{"name": "x"}), http.StatusUnauthorized, "create with bad token")
	}
}

// TEN-REQ-003/004, TEN-AC-004: membership CRUD and the role matrix.
func TestMembershipRoleMatrix(t *testing.T) {
	env.Truncate(t)
	ownerID, owner := user(t, "owner@a.test")
	adminID, admin := user(t, "admin@a.test")
	memberID, member := user(t, "member@a.test")
	_, _ = user(t, "late@a.test")
	tid := createTenant(t, owner, "A")
	base := "/tenants/" + tid + "/members"

	expect(t, call(t, "POST", base, owner, map[string]string{"email": "admin@a.test", "role": "admin"}), http.StatusCreated, "owner adds admin")
	expect(t, call(t, "POST", base, admin, map[string]string{"email": "member@a.test", "role": "member"}), http.StatusCreated, "admin adds member")
	expect(t, call(t, "POST", base, admin, map[string]string{"email": "late@a.test", "role": "owner"}), http.StatusForbidden, "admin grants owner")
	expect(t, call(t, "POST", base, member, map[string]string{"email": "late@a.test", "role": "member"}), http.StatusForbidden, "member adds")
	expect(t, call(t, "POST", base, owner, map[string]string{"email": "member@a.test", "role": "member"}), http.StatusConflict, "duplicate member")
	expect(t, call(t, "POST", base, owner, map[string]string{"email": "nobody@a.test", "role": "member"}), http.StatusNotFound, "unknown email")
	expect(t, call(t, "POST", base, owner, map[string]string{"email": "late@a.test", "role": "root"}), http.StatusBadRequest, "invalid role")

	expect(t, call(t, "GET", base, member, nil), http.StatusForbidden, "member lists")
	r := call(t, "GET", base, admin, nil)
	expect(t, r, http.StatusOK, "admin lists")
	if got := memberEmails(t, r); len(got) != 3 {
		t.Errorf("members = %v, want 3", got)
	}

	expect(t, call(t, "PATCH", base+"/"+memberID, admin, map[string]string{"role": "admin"}), http.StatusForbidden, "admin changes role")
	expect(t, call(t, "DELETE", base+"/"+memberID, admin, nil), http.StatusForbidden, "admin removes")

	r = call(t, "PATCH", base+"/"+memberID, owner, map[string]string{"role": "admin"})
	expect(t, r, http.StatusOK, "owner promotes")
	if r.Body["role"] != "admin" {
		t.Errorf("role after promote = %v", r.Body["role"])
	}

	expect(t, call(t, "PATCH", base+"/"+ownerID, owner, map[string]string{"role": "admin"}), http.StatusConflict, "demote last owner")
	expect(t, call(t, "DELETE", base+"/"+ownerID, owner, nil), http.StatusConflict, "remove last owner")

	expect(t, call(t, "PATCH", base+"/"+adminID, owner, map[string]string{"role": "owner"}), http.StatusOK, "second owner")
	expect(t, call(t, "DELETE", base+"/"+ownerID, admin, nil), http.StatusNoContent, "new owner removes first owner")
	expect(t, call(t, "GET", base, owner, nil), http.StatusNotFound, "removed owner loses access")

	expect(t, call(t, "DELETE", base+"/"+memberID, admin, nil), http.StatusNoContent, "remove member")
	expect(t, call(t, "DELETE", base+"/"+memberID, admin, nil), http.StatusNotFound, "remove twice")
	expect(t, call(t, "GET", base, member, nil), http.StatusNotFound, "removed member loses access")
}

// TEN-AC-003, TEN-AC-005, TEN-AC-008: cross-tenant access is invisible.
func TestCrossTenantAccess(t *testing.T) {
	env.Truncate(t)
	ownerAID, ownerA := user(t, "owner@a.test")
	_, ownerB := user(t, "owner@b.test")
	tenantA := createTenant(t, ownerA, "A")
	tenantB := createTenant(t, ownerB, "B")

	// Same resource id, two requesters: only the member gets data.
	expect(t, call(t, "GET", "/tenants/"+tenantA+"/members", ownerA, nil), http.StatusOK, "A reads A")
	expect(t, call(t, "GET", "/tenants/"+tenantA+"/members", ownerB, nil), http.StatusNotFound, "B reads A")

	// Unknown tenant looks exactly like someone else's tenant.
	other := call(t, "GET", "/tenants/"+tenantA+"/members", ownerB, nil)
	unknown := call(t, "GET", "/tenants/00000000-0000-4000-8000-000000000000/members", ownerB, nil)
	if fmt.Sprint(other) != fmt.Sprint(unknown) {
		t.Errorf("existence leak: other=%v unknown=%v", other, unknown)
	}

	// Forged selector header is ignored; the path tenant (verified) wins.
	r := call(t, "GET", "/tenants/"+tenantB+"/members", ownerB, nil, api.TenantHeader, tenantA)
	expect(t, r, http.StatusOK, "B with forged header")
	if got := memberEmails(t, r); len(got) != 1 || got[0] != "owner@b.test" {
		t.Errorf("forged header leaked members: %v", got)
	}

	// Acting on another tenant's member through one's own tenant: not found,
	// and nothing changes.
	expect(t, call(t, "DELETE", "/tenants/"+tenantB+"/members/"+ownerAID, ownerB, nil), http.StatusNotFound, "B removes A's owner via B")
	expect(t, call(t, "PATCH", "/tenants/"+tenantB+"/members/"+ownerAID, ownerB, map[string]string{"role": "member"}), http.StatusNotFound, "B demotes A's owner via B")
	r = call(t, "GET", "/tenants/"+tenantA+"/members", ownerA, nil)
	if got := memberEmails(t, r); len(got) != 1 || got[0] != "owner@a.test" {
		t.Errorf("tenant A changed: %v", got)
	}
}
