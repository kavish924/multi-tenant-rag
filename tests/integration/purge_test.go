//go:build integration

package integration

import (
	"net/http"
	"testing"
)

// DEL-AC-007: purge is owner-only, needs the confirmation header, and a
// non-member cannot reach it; document delete stays member+ (002).
func TestPurgeAuthz(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	_, admin := user(t, "ad@a.test")
	_, member := user(t, "m@a.test")
	_, outsider := user(t, "x@b.test")
	tid := createTenant(t, owner, "A")
	createTenant(t, outsider, "B")
	for email, role := range map[string]string{"ad@a.test": "admin", "m@a.test": "member"} {
		expect(t, call(t, "POST", "/tenants/"+tid+"/members", owner, map[string]string{"email": email, "role": role}), http.StatusCreated, "add "+role)
	}
	path := "/tenants/" + tid + "/purge"
	confirm := []string{"X-Confirm-Purge", tid}

	expect(t, call(t, "POST", path, member, nil, confirm...), http.StatusForbidden, "member purge")
	expect(t, call(t, "POST", path, admin, nil, confirm...), http.StatusForbidden, "admin purge")
	if r := call(t, "POST", path, outsider, nil, confirm...); r.Status != http.StatusNotFound && r.Status != http.StatusForbidden {
		t.Errorf("outsider purge: %d", r.Status)
	}
	expect(t, call(t, "POST", path, owner, nil), http.StatusBadRequest, "owner without confirmation")
	expect(t, call(t, "POST", path, owner, nil, "X-Confirm-Purge", "wrong"), http.StatusBadRequest, "owner wrong confirmation")
	r := call(t, "POST", path, owner, nil, confirm...)
	expect(t, r, http.StatusAccepted, "owner purge")
	if n := count(t, `SELECT count(*) FROM jobs WHERE tenant_id = $1 AND kind = 'purge'`, tid); n != 1 || r.Body["job_id"] == nil {
		t.Errorf("purge jobs %d, body %v", n, r.Body)
	}
}
