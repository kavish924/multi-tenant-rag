//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
)

func setOverride(t *testing.T, tenantID, col string, v int64) {
	t.Helper()
	if _, err := env.Admin.Exec(context.Background(),
		`INSERT INTO tenant_quotas (tenant_id, `+col+`) VALUES ($1, $2)
		 ON CONFLICT (tenant_id) DO UPDATE SET `+col+` = EXCLUDED.`+col, tenantID, v); err != nil {
		t.Fatal(err)
	}
}

// QTA-AC-001: over questions/day the ask is refused with 429 before any
// provider call (no new embed usage row), with Retry-After and the code.
func TestAskQuestionQuota(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	tid := createTenant(t, owner, "A")
	setOverride(t, tid, "questions_day", 1)

	if status, _ := askSSE(t, "/ask", owner, "first question"); status != http.StatusOK {
		t.Fatalf("first ask: %d", status)
	}
	embeds := count(t, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND kind = 'embed'`, tid)
	r := call(t, "POST", "/ask", owner, map[string]string{"question": "second question"})
	expect(t, r, http.StatusTooManyRequests, "second ask")
	if e := r.Body["error"].(map[string]any); e["code"] != "quota_questions" || e["limit"] != float64(1) || e["used"] != float64(1) {
		t.Errorf("envelope %v", r.Body)
	}
	if n := count(t, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND kind = 'embed'`, tid); n != embeds {
		t.Errorf("rejected ask called the embedder (%d → %d rows)", embeds, n)
	}

	// QTA-AC-003: a token budget below the projection rejects with quota_tokens.
	setOverride(t, tid, "questions_day", 100)
	setOverride(t, tid, "generate_tokens_month", 100)
	r = call(t, "POST", "/ask", owner, map[string]string{"question": "third question"})
	expect(t, r, http.StatusTooManyRequests, "token budget")
	if r.Body["error"].(map[string]any)["code"] != "quota_tokens" {
		t.Errorf("envelope %v", r.Body)
	}
}

// QTA-AC-002: over uploads/day the upload is refused and nothing is stored;
// a duplicate upload does not consume quota.
func TestUploadQuota(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	tid := createTenant(t, owner, "A")
	setOverride(t, tid, "uploads_day", 1)

	expect(t, upload(t, owner, "", "a.txt", "text/plain", []byte(textBody), nil), http.StatusAccepted, "first upload")
	expect(t, upload(t, owner, "", "a.txt", "text/plain", []byte(textBody), nil), http.StatusTooManyRequests, "second upload")
	if n := count(t, `SELECT count(*) FROM documents WHERE tenant_id = $1`, tid); n != 1 {
		t.Errorf("documents = %d, want 1", n)
	}
	setOverride(t, tid, "uploads_day", 2)
	expect(t, upload(t, owner, "", "a.txt", "text/plain", []byte(textBody), nil), http.StatusOK, "duplicate")
	expect(t, upload(t, owner, "", "b.txt", "text/plain", []byte("Another document body.\n"), nil), http.StatusAccepted, "duplicate was refunded")
}

// QTA-REQ-006: the usage endpoint is admin+ and reports limits and counters.
func TestUsageEndpoint(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	_, member := user(t, "m@a.test")
	tid := createTenant(t, owner, "A")
	expect(t, call(t, "POST", "/tenants/"+tid+"/members", owner, map[string]string{"email": "m@a.test", "role": "member"}), http.StatusCreated, "add member")
	expect(t, upload(t, owner, "", "a.txt", "text/plain", []byte(textBody), nil), http.StatusAccepted, "upload")

	expect(t, call(t, "GET", "/tenants/"+tid+"/usage", member, nil), http.StatusForbidden, "member usage")
	r := call(t, "GET", "/tenants/"+tid+"/usage", owner, nil)
	expect(t, r, http.StatusOK, "owner usage")
	up := r.Body["dimensions"].(map[string]any)["uploads"].(map[string]any)
	if up["used"] != float64(1) || up["limit"] != float64(500) {
		t.Errorf("uploads %v", up)
	}
	if _, ok := r.Body["estimated_cost_usd"]; !ok {
		t.Errorf("report %v", r.Body)
	}
}
