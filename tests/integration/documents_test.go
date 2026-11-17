//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/api"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
)

const textBody = "Refund policy\nCustomers may return unopened items within 30 days.\n"

// upload posts a multipart file. extra adds form fields (to try forging).
func upload(t *testing.T, token, tenantHeader, filename, ctype string, body []byte, extra map[string]string) resp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	h.Set("Content-Type", ctype)
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write(body)
	_ = mw.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/documents", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	if tenantHeader != "" {
		req.Header.Set(api.TenantHeader, tenantHeader)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{Status: res.StatusCode}
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func docOf(r resp) map[string]any {
	d, _ := r.Body["document"].(map[string]any)
	return d
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := env.Admin.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func objectExists(t *testing.T, tenantID, docID string) bool {
	t.Helper()
	key := objectstore.DocumentKey(uuid.MustParse(tenantID), uuid.MustParse(docID))
	r, err := objects.Get(context.Background(), key)
	if err != nil {
		return false
	}
	_ = r.Close()
	return true
}

// DOC-AC-005: document row and ingest job are created together; content
// streams back unchanged.
func TestUploadHappyPath(t *testing.T) {
	env.Truncate(t)
	_, token := user(t, "a@a.test")
	tid := createTenant(t, token, "A")

	r := upload(t, token, "", "policy.txt", "text/plain; charset=utf-8", []byte(textBody), nil)
	expect(t, r, http.StatusAccepted, "upload")
	d := docOf(r)
	id := d["id"].(string)
	if d["status"] != "queued" || d["content_type"] != "text/plain" || d["size_bytes"].(float64) != float64(len(textBody)) {
		t.Errorf("document: %v", d)
	}
	if n := count(t, `SELECT count(*) FROM jobs WHERE tenant_id = $1 AND kind = 'ingest' AND payload->>'document_id' = $2`, tid, id); n != 1 {
		t.Errorf("ingest jobs = %d, want 1", n)
	}
	if !objectExists(t, tid, id) {
		t.Error("object not stored")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/documents/"+id+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(got) != textBody || res.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("content: %d %q %s", res.StatusCode, got, res.Header.Get("Content-Type"))
	}

	expect(t, call(t, "GET", "/documents/"+id, token, nil), http.StatusOK, "get")
	r = call(t, "GET", "/documents", token, nil)
	expect(t, r, http.StatusOK, "list")
	if items := r.Body["items"].([]any); len(items) != 1 {
		t.Errorf("list items = %d", len(items))
	}
}

// DOC-AC-001, DOC-AC-002: oversize and mismatched types store nothing.
func TestUploadValidation(t *testing.T) {
	env.Truncate(t)
	_, token := user(t, "a@a.test")
	createTenant(t, token, "A")

	big := bytes.Repeat([]byte("a"), maxUpload+1)
	expect(t, upload(t, token, "", "big.txt", "text/plain", big, nil), http.StatusRequestEntityTooLarge, "too large")
	expect(t, upload(t, token, "", "fake.pdf", "application/pdf", []byte(textBody), nil), http.StatusUnsupportedMediaType, "text declared pdf")
	expect(t, upload(t, token, "", "x.txt", "text/plain", []byte("\x00\x01binary"), nil), http.StatusUnsupportedMediaType, "binary declared text")
	expect(t, upload(t, token, "", "x.html", "text/html", []byte("<p>hi</p>"), nil), http.StatusUnsupportedMediaType, "html")
	expect(t, upload(t, token, "", "x.txt", "text/plain", nil, nil), http.StatusUnsupportedMediaType, "empty")

	if n := count(t, `SELECT count(*) FROM documents`); n != 0 {
		t.Errorf("%d documents after rejected uploads", n)
	}
	if n := count(t, `SELECT count(*) FROM jobs`); n != 0 {
		t.Errorf("%d jobs after rejected uploads", n)
	}
}

// DOC-AC-003, DOC-AC-004: dedup is per tenant.
func TestUploadDedup(t *testing.T) {
	env.Truncate(t)
	_, tokA := user(t, "a@a.test")
	_, tokB := user(t, "b@b.test")
	createTenant(t, tokA, "A")
	createTenant(t, tokB, "B")

	first := upload(t, tokA, "", "p.txt", "text/plain", []byte(textBody), nil)
	expect(t, first, http.StatusAccepted, "first")
	again := upload(t, tokA, "", "renamed.txt", "text/plain", []byte(textBody), nil)
	expect(t, again, http.StatusOK, "duplicate")
	if again.Body["duplicate"] != true || docOf(again)["id"] != docOf(first)["id"] {
		t.Errorf("duplicate response: %v", again.Body)
	}
	other := upload(t, tokB, "", "p.txt", "text/plain", []byte(textBody), nil)
	expect(t, other, http.StatusAccepted, "other tenant same content")
	if docOf(other)["id"] == docOf(first)["id"] {
		t.Error("dedup crossed tenants")
	}
	if n := count(t, `SELECT count(*) FROM jobs WHERE kind = 'ingest'`); n != 2 {
		t.Errorf("ingest jobs = %d, want 2", n)
	}
}

// Task 5.3: concurrent identical uploads produce exactly one document and job.
func TestConcurrentDuplicateUploads(t *testing.T) {
	env.Truncate(t)
	_, token := user(t, "a@a.test")
	createTenant(t, token, "A")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := upload(t, token, "", "p.txt", "text/plain", []byte(textBody), nil)
			if r.Status != http.StatusAccepted && r.Status != http.StatusOK {
				t.Errorf("status %d %v", r.Status, r.Body)
			}
		}()
	}
	wg.Wait()
	if n := count(t, `SELECT count(*) FROM documents`); n != 1 {
		t.Errorf("documents = %d, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM jobs`); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
}

// DOC-AC-007, DOC-AC-009: other tenants see 404; forged fields are ignored.
func TestDocumentCrossTenant(t *testing.T) {
	env.Truncate(t)
	_, tokA := user(t, "a@a.test")
	_, tokB := user(t, "b@b.test")
	tenantA := createTenant(t, tokA, "A")
	tenantB := createTenant(t, tokB, "B")

	id := docOf(upload(t, tokA, "", "p.txt", "text/plain", []byte(textBody), nil))["id"].(string)
	for _, path := range []string{"/documents/" + id, "/documents/" + id + "/content"} {
		expect(t, call(t, "GET", path, tokB, nil), http.StatusNotFound, "B GET "+path)
		expect(t, call(t, "GET", path, tokB, nil, api.TenantHeader, tenantA), http.StatusNotFound, "B forged header "+path)
	}
	expect(t, call(t, "DELETE", "/documents/"+id, tokB, nil), http.StatusNotFound, "B DELETE")
	if n := count(t, `SELECT count(*) FROM documents WHERE id = $1 AND deleted_at IS NULL`, id); n != 1 {
		t.Error("B's delete affected A's document")
	}

	r := upload(t, tokB, "", "forged.txt", "text/plain", []byte("forged content\n"),
		map[string]string{"tenant_id": tenantA, "tenant": tenantA})
	expect(t, r, http.StatusAccepted, "forged fields")
	if n := count(t, `SELECT count(*) FROM documents WHERE id = $1 AND tenant_id = $2`, docOf(r)["id"], tenantB); n != 1 {
		t.Error("forged upload not scoped to the verified tenant")
	}
	expect(t, upload(t, tokB, tenantA, "x.txt", "text/plain", []byte("x\n"), nil), http.StatusNotFound, "forged header upload")
}

// DOC-AC-008: soft delete keeps metadata, refuses content, enqueues cleanup
// once, and is idempotent; deleted docs are hidden from listings by default.
func TestSoftDelete(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	_, member := user(t, "m@a.test")
	tid := createTenant(t, owner, "A")
	expect(t, call(t, "POST", "/tenants/"+tid+"/members", owner, map[string]string{"email": "m@a.test", "role": "member"}), http.StatusCreated, "add member")

	id := docOf(upload(t, member, "", "p.txt", "text/plain", []byte(textBody), nil))["id"].(string)
	r := call(t, "DELETE", "/documents/"+id, member, nil)
	expect(t, r, http.StatusAccepted, "delete")
	if r.Body["status"] != "deleting" {
		t.Errorf("status after delete: %v", r.Body["status"])
	}
	expect(t, call(t, "DELETE", "/documents/"+id, member, nil), http.StatusAccepted, "delete again")
	if n := count(t, `SELECT count(*) FROM jobs WHERE kind = 'cleanup' AND payload->>'document_id' = $1`, id); n != 1 {
		t.Errorf("cleanup jobs = %d, want 1", n)
	}

	r = call(t, "GET", "/documents/"+id, member, nil)
	expect(t, r, http.StatusOK, "metadata after delete")
	if r.Body["status"] != "deleting" || r.Body["deleted_at"] == nil {
		t.Errorf("metadata: %v", r.Body)
	}
	expect(t, call(t, "GET", "/documents/"+id+"/content", member, nil), http.StatusGone, "content after delete")

	r = call(t, "GET", "/documents", member, nil)
	if items := r.Body["items"].([]any); len(items) != 0 {
		t.Errorf("deleted doc listed by default: %v", items)
	}
	expect(t, call(t, "GET", "/documents?include_deleted=true", member, nil), http.StatusForbidden, "member include_deleted")
	r = call(t, "GET", "/documents?include_deleted=true", owner, nil)
	expect(t, r, http.StatusOK, "owner include_deleted")
	if items := r.Body["items"].([]any); len(items) != 1 {
		t.Errorf("include_deleted items = %d", len(items))
	}

	// The same content can be uploaded again after delete.
	expect(t, upload(t, member, "", "p.txt", "text/plain", []byte(textBody), nil), http.StatusAccepted, "re-upload after delete")

	// deleted_at is sticky even for direct writes.
	if _, err := env.Admin.Exec(context.Background(), `UPDATE documents SET deleted_at = NULL, status = 'indexed' WHERE id = $1`, id); err == nil {
		t.Error("deleted_at could be cleared")
	}
}

func TestListPagination(t *testing.T) {
	env.Truncate(t)
	_, token := user(t, "a@a.test")
	createTenant(t, token, "A")
	for i := 0; i < 5; i++ {
		expect(t, upload(t, token, "", "f.txt", "text/plain", []byte(strings.Repeat("x", i+1)+"\n"), nil), http.StatusAccepted, "seed")
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/documents?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := call(t, "GET", path, token, nil)
		expect(t, r, http.StatusOK, "page")
		for _, it := range r.Body["items"].([]any) {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Errorf("document %s on two pages", id)
			}
			seen[id] = true
		}
		pages++
		cursor, _ = r.Body["next_cursor"].(string)
		if cursor == "" || pages > 5 {
			break
		}
	}
	if len(seen) != 5 || pages != 3 {
		t.Errorf("saw %d docs over %d pages, want 5 over 3", len(seen), pages)
	}
	expect(t, call(t, "GET", "/documents?cursor=garbage", token, nil), http.StatusBadRequest, "bad cursor")
	expect(t, call(t, "GET", "/documents?status=bogus", token, nil), http.StatusBadRequest, "bad status")
}

func TestContentRedirectUnsupportedOnFS(t *testing.T) {
	env.Truncate(t)
	_, token := user(t, "a@a.test")
	createTenant(t, token, "A")
	id := docOf(upload(t, token, "", "p.txt", "text/plain", []byte(textBody), nil))["id"].(string)
	expect(t, call(t, "GET", "/documents/"+id+"/content?redirect=true", token, nil), http.StatusNotImplemented, "redirect on fs store")
}
