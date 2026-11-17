//go:build integration

package integration

import (
	"net/http"
	"testing"
)

// POST /search (spec 007): validation, tenant resolution and response shape.
func TestSearchEndpoint(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	createTenant(t, owner, "A")

	expect(t, call(t, "POST", "/search", "", map[string]any{"question": "x"}), http.StatusUnauthorized, "no token")
	for _, q := range []string{"", "   "} {
		r := call(t, "POST", "/search", owner, map[string]any{"question": q})
		expect(t, r, http.StatusBadRequest, "empty question (RET-AC-006)")
	}
	expect(t, call(t, "POST", "/search", owner, map[string]any{"question": "x", "limit": 101}), http.StatusBadRequest, "limit too large")

	r := call(t, "POST", "/search", owner, map[string]any{"question": "refund policy", "limit": 8})
	expect(t, r, http.StatusOK, "search")
	if hits, ok := r.Body["hits"].([]any); !ok || len(hits) != 0 {
		t.Errorf("empty tenant hits: %v", r.Body)
	}
}
