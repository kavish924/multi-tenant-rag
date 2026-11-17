//go:build integration

package cleanup_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/documents"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/cleanup"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/embed"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/messaging"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
)

var (
	env     *pgtest.Env
	objects *objectstore.FS
	objDir  string
	docs    *documents.Service
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	e, err := pgtest.Start(ctx)
	cancel()
	if err != nil {
		log.Fatalf("pgtest: %v", err)
	}
	env = e
	objDir, _ = os.MkdirTemp("", "objects-*")
	objects, _ = objectstore.NewFS(objDir)
	docs = documents.NewService(env.Store, objects, 0)
	code := m.Run()
	env.Close()
	_ = os.RemoveAll(objDir)
	os.Exit(code)
}

type inProcess struct{}

func (inProcess) Extract(_ context.Context, ctype string, r io.Reader) ([]extract.Section, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return extract.Extract(ctype, b, extract.DefaultLimits)
}

func pipeline() *ingestion.Pipeline {
	return &ingestion.Pipeline{Objects: objects, Extractor: inProcess{}, Embedder: &embed.Batcher{Provider: llm.Fake{}}}
}

func tx(tenant uuid.UUID) jobs.TenantTx { return jobs.StoreScoper(env.Store)(tenant) }

func body(i int) []byte {
	var b strings.Builder
	for p := 0; p < 30; p++ {
		fmt.Fprintf(&b, "Document %d paragraph %d about the refund policy and thirty day returns.\n\n", i, p)
	}
	return []byte(b.String())
}

// ingested uploads and fully ingests a document, returning its id.
func ingested(t *testing.T, tenant uuid.UUID, i int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	res, err := docs.Upload(ctx, documents.UploadInput{TenantID: tenant, Filename: "f.txt", DeclaredType: documents.TypeText, Body: bytes.NewReader(body(i))})
	if err != nil {
		t.Fatal(err)
	}
	q := jobs.NewQueue(env.Store, 0)
	q.SetTenantCap(1 << 20)
	j, err := q.Claim(ctx, "t", []string{ingestion.JobKind})
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	if err := pipeline().Handle(ctx, *j, tx(tenant)); err != nil {
		t.Fatal(err)
	}
	_ = q.Complete(ctx, *j, "t")
	return res.Document.ID
}

type docState struct {
	status string
	chunks int
	object bool
}

func state(t *testing.T, tenant, id uuid.UUID) docState {
	t.Helper()
	ctx := context.Background()
	var s docState
	_ = env.Admin.QueryRow(ctx, `SELECT status FROM documents WHERE id = $1`, id).Scan(&s.status)
	_ = env.Admin.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE document_id = $1`, id).Scan(&s.chunks)
	_, err := os.Stat(filepath.Join(objDir, filepath.FromSlash(objectstore.DocumentKey(tenant, id))))
	s.object = err == nil
	return s
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := env.Admin.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func cleaner() *cleanup.Cleaner { return &cleanup.Cleaner{Objects: objects, BatchSize: 7} }

// DEL-AC-001 and DEL-AC-002: exclusion is immediate; cleanup leaves no
// chunks, no object, status deleted, and a document.deleted event.
func TestDeleteThenCleanup(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	id := ingested(t, tid, 1)
	if s := state(t, tid, id); s.status != "indexed" || s.chunks == 0 || !s.object {
		t.Fatalf("before delete %+v", s)
	}
	search := retrieval.NewHybrid(env.Store, llm.Fake{}, retrieval.Options{}, nil)

	start := time.Now()
	if _, err := docs.Delete(ctx, tid, id); err != nil {
		t.Fatal(err)
	}
	hits, err := search.Search(ctx, tid, "refund policy thirty day returns", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.DocumentID == id {
			t.Fatal("deleted document still searchable")
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("exclusion took %v (DEL-NFR-001)", d)
	}

	if err := cleaner().CleanDocument(ctx, tx(tid), tid, id); err != nil {
		t.Fatal(err)
	}
	if s := state(t, tid, id); s.status != "deleted" || s.chunks != 0 || s.object {
		t.Errorf("after cleanup %+v", s)
	}
	if n := count(t, `SELECT count(*) FROM outbox WHERE topic = 'document.deleted' AND payload->>'document_id' = $1`, id.String()); n != 1 {
		t.Errorf("document.deleted events = %d", n)
	}
	// Idempotent: a second run changes nothing and emits nothing new.
	if err := cleaner().CleanDocument(ctx, tx(tid), tid, id); err != nil {
		t.Fatal(err)
	}
	if n := count(t, `SELECT count(*) FROM outbox WHERE topic = 'document.deleted'`); n != 1 {
		t.Errorf("rerun emitted again (%d)", n)
	}
	// A live document is refused permanently.
	live := ingested(t, tid, 2)
	if err := cleaner().CleanDocument(ctx, tx(tid), tid, live); !jobs.IsPermanent(err) || state(t, tid, live).chunks == 0 {
		t.Errorf("live document: %v", err)
	}
}

// failingStore fails object deletes until healed.
type failingStore struct {
	*objectstore.FS
	fail atomic.Bool
}

func (f *failingStore) Delete(ctx context.Context, key string) error {
	if f.fail.Load() {
		return errors.New("storage unavailable")
	}
	return f.FS.Delete(ctx, key)
}

// DEL-AC-004: a failed object delete retries until the invariant holds.
func TestCleanupRetriesObjectFailure(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	id := ingested(t, tid, 1)
	if _, err := docs.Delete(ctx, tid, id); err != nil {
		t.Fatal(err)
	}
	fs := &failingStore{FS: objects}
	fs.fail.Store(true)
	c := &cleanup.Cleaner{Objects: fs}
	err := c.CleanDocument(ctx, tx(tid), tid, id)
	if err == nil || jobs.IsPermanent(err) {
		t.Fatalf("object failure should be retryable: %v", err)
	}
	if s := state(t, tid, id); s.status != "deleting" || s.chunks != 0 || !s.object {
		t.Errorf("partial state %+v", s)
	}
	fs.fail.Store(false)
	if err := c.CleanDocument(ctx, tx(tid), tid, id); err != nil {
		t.Fatal(err)
	}
	if s := state(t, tid, id); s.status != "deleted" || s.object {
		t.Errorf("after retry %+v", s)
	}
}

// DEL-AC-003: every interleaving ends with no chunks and the document never
// leaves deleting/deleted.
func TestDeletionRaces(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	q := jobs.NewQueue(env.Store, 0)
	q.SetTenantCap(1 << 20)
	claim := func() jobs.Job {
		j, err := q.Claim(ctx, "t", []string{ingestion.JobKind})
		if err != nil || j == nil {
			t.Fatalf("claim %v %v", j, err)
		}
		return *j
	}
	upload := func(i int) uuid.UUID {
		res, err := docs.Upload(ctx, documents.UploadInput{TenantID: tid, Filename: "f.txt", DeclaredType: documents.TypeText, Body: bytes.NewReader(body(i))})
		if err != nil {
			t.Fatal(err)
		}
		return res.Document.ID
	}
	finish := func(name string, id uuid.UUID) {
		t.Helper()
		if s := state(t, tid, id); s.status != "deleting" {
			t.Errorf("%s: status before cleanup %q", name, s.status)
		}
		if err := cleaner().CleanDocument(ctx, tx(tid), tid, id); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s := state(t, tid, id); s.status != "deleted" || s.chunks != 0 {
			t.Errorf("%s: final %+v", name, s)
		}
	}

	// Delete during extract/chunk/embed: Index sees deleted_at.
	id := upload(1)
	p := pipeline()
	p.BeforeIndex = func(ctx context.Context, _ uuid.UUID) {
		if _, err := docs.Delete(ctx, tid, id); err != nil {
			t.Error(err)
		}
	}
	if err := p.Handle(ctx, claim(), tx(tid)); err != nil {
		t.Fatal(err)
	}
	finish("delete during embed", id)

	// Index commits first (the outcome when Delete waits on Index's lock):
	// the chunks exist, then cleanup removes them.
	id = upload(2)
	if err := pipeline().Handle(ctx, claim(), tx(tid)); err != nil {
		t.Fatal(err)
	}
	if state(t, tid, id).chunks == 0 {
		t.Fatal("index wrote nothing")
	}
	if _, err := docs.Delete(ctx, tid, id); err != nil {
		t.Fatal(err)
	}
	finish("index then delete", id)

	// Delete holds the row lock while Index waits for it.
	id = upload(3)
	j := claim()
	p = pipeline()
	locked := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, err := env.App.Begin(ctx)
		if err != nil {
			t.Error(err)
			close(locked)
			return
		}
		defer func() { _ = conn.Rollback(ctx) }()
		_, _ = conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tid.String())
		_, err = conn.Exec(ctx, `UPDATE documents SET deleted_at = now(), status = 'deleting' WHERE id = $1`, id)
		if err != nil {
			t.Error(err)
		}
		close(locked)
		<-release
		_ = conn.Commit(ctx)
	}()
	p.BeforeIndex = func(context.Context, uuid.UUID) {
		<-locked
		go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	}
	if err := p.Handle(ctx, j, tx(tid)); err != nil {
		t.Fatal(err)
	}
	finish("index waits on delete", id)

	// Ingest retried after delete: completes as cancelled.
	id = upload(4)
	j = claim()
	if _, err := docs.Delete(ctx, tid, id); err != nil {
		t.Fatal(err)
	}
	if err := pipeline().Handle(ctx, j, tx(tid)); err != nil {
		t.Fatal(err)
	}
	finish("ingest after delete", id)
}

// DEL-AC-005: a duplicate document.deleting event has no second side effect.
func TestDeletingConsumerIdempotent(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	doc := uuid.New()
	ev := messaging.Event{ID: 4242, Topic: messaging.TopicDocumentDeleting, TenantID: tid, OccurredAt: time.Now(),
		Data: []byte(`{"document_id":"` + doc.String() + `"}`)}
	c := cleanup.DeletingConsumer{Store: env.Store}
	for i := 0; i < 3; i++ {
		if err := c.Publish(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, `SELECT count(*) FROM consumed_events WHERE consumer = 'cleanup' AND event_id = 4242`); n != 1 {
		t.Errorf("consumed rows %d", n)
	}
	if n := count(t, `SELECT count(*) FROM jobs WHERE kind = 'cleanup' AND payload->>'document_id' = $1`, doc.String()); n != 1 {
		t.Errorf("cleanup jobs %d", n)
	}
	// A second, distinct event for the same document is still harmless:
	// the job's idempotency key collapses it.
	ev.ID = 4243
	if err := c.Publish(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n := count(t, `SELECT count(*) FROM jobs WHERE kind = 'cleanup' AND payload->>'document_id' = $1`, doc.String()); n != 1 {
		t.Errorf("cleanup jobs after new event %d", n)
	}
}

// DEL-AC-006: after purge the tenant has no chunks, objects or usage rows;
// another tenant is untouched.
func TestPurge(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	a, b := env.SeedTenant(t, "A", nil), env.SeedTenant(t, "B", nil)
	aDocs := []uuid.UUID{ingested(t, a, 1), ingested(t, a, 2), ingested(t, a, 3)}
	bDoc := ingested(t, b, 4)
	if _, err := docs.Delete(ctx, a, aDocs[0]); err != nil { // one already deleting
		t.Fatal(err)
	}
	rec := usage.NewRecorder(env.Store)
	for _, tenant := range []uuid.UUID{a, b} {
		if err := rec.Record(ctx, usage.Event{TenantID: tenant, Kind: usage.KindGenerate, Model: "m", InputTokens: 10}); err != nil {
			t.Fatal(err)
		}
	}
	jobID, err := docs.RequestPurge(ctx, a, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	p := &cleanup.Purger{Cleaner: cleaner(), SoftDeleteBatch: 1}
	if err := p.Handle(ctx, jobs.Job{ID: jobID, TenantID: a, Kind: cleanup.JobPurge, Payload: []byte(`{}`)}, tx(a)); err != nil {
		t.Fatal(err)
	}
	for _, id := range aDocs {
		if s := state(t, a, id); s.status != "deleted" || s.chunks != 0 || s.object {
			t.Errorf("doc %s after purge %+v", id, s)
		}
	}
	if n := count(t, `SELECT count(*) FROM usage_events WHERE tenant_id = $1`, a); n != 0 {
		t.Errorf("tenant A usage rows %d", n)
	}
	if n := count(t, `SELECT count(*) FROM tenant_usage_counters WHERE tenant_id = $1`, a); n != 0 {
		t.Errorf("tenant A counters %d", n)
	}
	if s := state(t, b, bDoc); s.status != "indexed" || s.chunks == 0 || !s.object {
		t.Errorf("tenant B touched: %+v", s)
	}
	if n := count(t, `SELECT count(*) FROM usage_events WHERE tenant_id = $1`, b); n != 1 {
		t.Errorf("tenant B usage rows %d", n)
	}
	if n := count(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND topic = 'tenant.purged'`, a); n != 1 {
		t.Errorf("tenant.purged events %d", n)
	}
	if n := count(t, `SELECT count(*) FROM tenants WHERE id = $1`, a); n != 1 {
		t.Error("tenant row must be retained")
	}
	// Resumable / idempotent: running again is harmless.
	if err := p.Handle(ctx, jobs.Job{ID: jobID, TenantID: a, Kind: cleanup.JobPurge, Payload: []byte(`{}`)}, tx(a)); err != nil {
		t.Fatal(err)
	}
}

// DEL-AC-008: the sweeper removes deleted metadata past retention and
// orphan objects older than the grace period, and nothing else.
func TestSweeper(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	gone := ingested(t, tid, 1)
	if _, err := docs.Delete(ctx, tid, gone); err != nil {
		t.Fatal(err)
	}
	if err := cleaner().CleanDocument(ctx, tx(tid), tid, gone); err != nil {
		t.Fatal(err)
	}
	deleting := ingested(t, tid, 2) // deleting, not yet cleaned: must stay
	if _, err := docs.Delete(ctx, tid, deleting); err != nil {
		t.Fatal(err)
	}
	live := ingested(t, tid, 3)

	put := func(key string, age time.Duration) {
		if err := objects.Put(ctx, key, strings.NewReader("orphan"), -1, "text/plain"); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(objDir, filepath.FromSlash(key))
		old := time.Now().Add(-age)
		_ = os.Chtimes(p, old, old)
	}
	oldOrphan := objectstore.DocumentKey(tid, uuid.New())
	newOrphan := objectstore.DocumentKey(tid, uuid.New())
	put(oldOrphan, 2*time.Hour)
	put(newOrphan, time.Minute)
	foreignOrphan := objectstore.DocumentKey(uuid.New(), uuid.New()) // tenant row gone
	put(foreignOrphan, 2*time.Hour)
	// Make the live document's object old too: it has a row, so it stays.
	livePath := filepath.Join(objDir, filepath.FromSlash(objectstore.DocumentKey(tid, live)))
	_ = os.Chtimes(livePath, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))

	sw := &cleanup.Sweeper{Pool: env.App, Store: storage.New(env.App), Objects: objects,
		Retention: time.Microsecond, OrphanGrace: time.Hour}
	time.Sleep(10 * time.Millisecond)
	rep, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Metadata != 1 || rep.Orphans != 2 {
		t.Errorf("report %+v, want 1 metadata and 2 orphans", rep)
	}
	if count(t, `SELECT count(*) FROM documents WHERE id = $1`, gone) != 0 {
		t.Error("expired deleted row kept")
	}
	if s := state(t, tid, deleting); s.status != "deleting" || !s.object {
		t.Errorf("uncleaned document touched: %+v", s)
	}
	if s := state(t, tid, live); s.status != "indexed" || !s.object {
		t.Errorf("live document touched: %+v", s)
	}
	exists := func(key string) bool {
		_, err := os.Stat(filepath.Join(objDir, filepath.FromSlash(key)))
		return err == nil
	}
	if exists(oldOrphan) || exists(foreignOrphan) || !exists(newOrphan) {
		t.Errorf("orphans: old %v foreign %v new %v", exists(oldOrphan), exists(foreignOrphan), exists(newOrphan))
	}
	// The app role cannot hard-delete a live or deleting document, even by
	// calling the system function directly.
	var ok bool
	for _, id := range []uuid.UUID{live, deleting} {
		if err := env.App.QueryRow(ctx, `SELECT sweep_delete_document($1, interval '0')`, id).Scan(&ok); err != nil || ok {
			t.Errorf("sweep_delete_document removed %s (%v)", id, err)
		}
	}
	err = env.Store.WithTenant(ctx, tid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM documents WHERE id = $1`, live)
		return err
	})
	if err == nil {
		t.Error("the app role can DELETE documents directly")
	}
}

// DEL-NFR-002: a 5,000-chunk document is cleaned up in under 60 s.
func TestCleanupPerformance(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	id := ingested(t, tid, 1)
	if _, err := env.Admin.Exec(ctx, `
		INSERT INTO chunks (tenant_id, document_id, ordinal, content, token_count, embed_model, embedding)
		SELECT $1, $2, 1000 + g, 'filler ' || g, 2, 'fake-hash-v1', (SELECT embedding FROM chunks WHERE document_id = $2 LIMIT 1)
		FROM generate_series(1, 5000) g`, tid, id); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.Delete(ctx, tid, id); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := (&cleanup.Cleaner{Objects: objects}).CleanDocument(ctx, tx(tid), tid, id); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 60*time.Second {
		t.Errorf("cleanup took %v", d)
	}
	if s := state(t, tid, id); s.chunks != 0 || s.status != "deleted" {
		t.Errorf("final %+v", s)
	}
}
