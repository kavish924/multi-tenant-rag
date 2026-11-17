//go:build integration

package ingestion_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/documents"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/embed"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
)

var (
	env     *pgtest.Env
	objects *objectstore.FS
	docs    *documents.Service
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == extract.HelperArg {
		extract.HelperMain()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	e, err := pgtest.Start(ctx)
	cancel()
	if err != nil {
		log.Fatalf("pgtest: %v", err)
	}
	env = e
	dir, _ := os.MkdirTemp("", "objects-*")
	objects, _ = objectstore.NewFS(dir)
	docs = documents.NewService(env.Store, objects, 0)
	code := m.Run()
	env.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func pipeline(p llm.EmbeddingProvider) *ingestion.Pipeline {
	return &ingestion.Pipeline{
		Objects:   objects,
		Extractor: &extract.Isolated{Command: []string{os.Args[0], extract.HelperArg}},
		Embedder:  &embed.Batcher{Provider: p},
	}
}

// upload stores a document and returns its ingest job as claimed.
func upload(t *testing.T, tenantID uuid.UUID, ctype string, body []byte) (uuid.UUID, jobs.Job) {
	t.Helper()
	ctx := context.Background()
	res, err := docs.Upload(ctx, documents.UploadInput{TenantID: tenantID, Filename: "f", DeclaredType: ctype, Body: bytes.NewReader(body)})
	if err != nil {
		t.Fatal(err)
	}
	j, err := jobs.NewQueue(env.Store, 0).Claim(ctx, "test", []string{ingestion.JobKind})
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	return res.Document.ID, *j
}

func handle(t *testing.T, p *ingestion.Pipeline, j jobs.Job) error {
	t.Helper()
	return p.Handle(context.Background(), j, jobs.StoreScoper(env.Store)(j.TenantID))
}

type docState struct {
	status string
	errMsg *string
	chunks int
}

func state(t *testing.T, id uuid.UUID) docState {
	t.Helper()
	var s docState
	ctx := context.Background()
	if err := env.Admin.QueryRow(ctx, `SELECT status, error FROM documents WHERE id = $1`, id).Scan(&s.status, &s.errMsg); err != nil {
		t.Fatal(err)
	}
	_ = env.Admin.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE document_id = $1`, id).Scan(&s.chunks)
	return s
}

func longText(paragraphs int) []byte {
	var b strings.Builder
	b.WriteString("# Refund policy\n\n")
	for i := 0; i < paragraphs; i++ {
		fmt.Fprintf(&b, "Paragraph %d. Customers may return unopened items within thirty days of delivery. "+
			"Refunds go back to the original payment method within five business days. "+
			"Opened items can be exchanged for store credit at the manager's discretion.\n\n", i)
	}
	return []byte(b.String())
}

func docxBytes(paras ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	var body strings.Builder
	for i, p := range paras {
		style := ""
		if i == 0 {
			style = `<w:pPr><w:pStyle w:val="Heading1"/></w:pPr>`
		}
		fmt.Fprintf(&body, `<w:p>%s<w:r><w:t>%s</w:t></w:r></w:p>`, style, p)
	}
	fmt.Fprintf(w, `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>%s</w:body></w:document>`, body.String())
	_ = zw.Close()
	return buf.Bytes()
}

func pdfBytes(text string) []byte {
	stream := fmt.Sprintf("BT /F1 12 Tf 72 712 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [4 0 R] /Count 1 >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

// ING-AC-001, ING-AC-006 for TXT, DOCX and PDF.
func TestIngestFormats(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	p := pipeline(llm.Fake{})
	cases := map[string]struct {
		ctype string
		body  []byte
		want  string
	}{
		"txt":  {documents.TypeText, longText(30), "thirty days"},
		"docx": {documents.TypeDOCX, docxBytes("Leave policy", "You get twenty days of leave."), "twenty days"},
		"pdf":  {documents.TypePDF, pdfBytes("Expenses need a receipt"), "receipt"},
	}
	for name, c := range cases {
		id, j := upload(t, tid, c.ctype, c.body)
		if err := handle(t, p, j); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := state(t, id)
		if s.status != "indexed" || s.chunks == 0 || s.errMsg != nil {
			t.Fatalf("%s: state %+v", name, s)
		}
		var content, model string
		var tokens int
		var dims int
		err := env.Admin.QueryRow(context.Background(), `
			SELECT string_agg(content, ' '), min(embed_model), min(token_count), min(vector_dims(embedding))
			FROM chunks WHERE document_id = $1 AND tenant_id = $2`, id, tid).Scan(&content, &model, &tokens, &dims)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(content, c.want) || model != "fake-hash-v1" || tokens <= 0 || dims != llm.EmbeddingDim {
			t.Errorf("%s: content/model/tokens/dims = %q.. %s %d %d", name, content[:min(40, len(content))], model, tokens, dims)
		}
	}
	var heading *string
	_ = env.Admin.QueryRow(context.Background(), `SELECT heading FROM chunks WHERE content LIKE '%twenty days%'`).Scan(&heading)
	if heading == nil || *heading != "Leave policy" {
		t.Errorf("docx heading = %v", heading)
	}
	var page *int
	_ = env.Admin.QueryRow(context.Background(), `SELECT page FROM chunks WHERE content LIKE '%receipt%'`).Scan(&page)
	if page == nil || *page != 1 {
		t.Errorf("pdf page = %v", page)
	}
}

// ING-AC-004: a rerun upserts the same rows.
func TestIngestRerunIsIdempotent(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	p := pipeline(llm.Fake{})
	id, j := upload(t, tid, documents.TypeText, longText(40))
	if err := handle(t, p, j); err != nil {
		t.Fatal(err)
	}
	first := state(t, id)
	var ids1 []string
	rows, _ := env.Admin.Query(context.Background(), `SELECT id::text FROM chunks WHERE document_id = $1 ORDER BY ordinal`, id)
	ids1, _ = pgx.CollectRows(rows, pgx.RowTo[string])

	if err := handle(t, p, j); err != nil { // crash after embed → job rerun
		t.Fatal(err)
	}
	second := state(t, id)
	rows, _ = env.Admin.Query(context.Background(), `SELECT id::text FROM chunks WHERE document_id = $1 ORDER BY ordinal`, id)
	ids2, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	if first.chunks < 2 || first.chunks != second.chunks || strings.Join(ids1, ",") != strings.Join(ids2, ",") {
		t.Errorf("rerun changed rows: %d→%d", first.chunks, second.chunks)
	}
}

// ING-REQ-010: a job for an already-deleted document does nothing.
func TestIngestDeletedBeforeStart(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	id, j := upload(t, tid, documents.TypeText, longText(5))
	if _, err := docs.Delete(context.Background(), tid, id); err != nil {
		t.Fatal(err)
	}
	if err := handle(t, pipeline(llm.Fake{}), j); err != nil {
		t.Fatalf("cancelled job should succeed: %v", err)
	}
	if s := state(t, id); s.status != "deleting" || s.chunks != 0 {
		t.Errorf("state %+v", s)
	}
}

// ING-AC-005: delete commits after embedding, before the Index lock.
func TestIngestDeletedBeforeIndex(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	id, j := upload(t, tid, documents.TypeText, longText(10))
	p := pipeline(llm.Fake{})
	p.BeforeIndex = func(ctx context.Context, _ uuid.UUID) {
		if _, err := docs.Delete(ctx, tid, id); err != nil {
			t.Error(err)
		}
	}
	if err := handle(t, p, j); err != nil {
		t.Fatal(err)
	}
	if s := state(t, id); s.status != "deleting" || s.chunks != 0 {
		t.Errorf("state %+v", s)
	}
}

// ING-AC-009: delete holds the row lock while Index waits for it; after the
// delete commits, Index sees deleted_at and writes nothing.
func TestIngestDeleteWhileIndexWaitsOnLock(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	id, j := upload(t, tid, documents.TypeText, longText(10))
	ctx := context.Background()

	locked := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	p := pipeline(llm.Fake{})
	p.BeforeIndex = func(context.Context, uuid.UUID) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := env.Store.WithTenant(ctx, tid, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE documents SET deleted_at = now(), status = 'deleting' WHERE id = $1`, id); err != nil {
					return err
				}
				close(locked)
				<-release // hold the lock while Index blocks on FOR UPDATE
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
		<-locked
		go func() { time.Sleep(300 * time.Millisecond); close(release) }()
	}
	if err := handle(t, p, j); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if s := state(t, id); s.status != "deleting" || s.chunks != 0 {
		t.Errorf("state %+v", s)
	}
}

type wrongDim struct{ llm.Fake }

func (wrongDim) Dim() int { return 384 }
func (w wrongDim) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return llm.Fake{Dimension: 384}.Embed(ctx, texts)
}

// ING-AC-003 and malformed input: failed with a reason, no chunks,
// permanent job failure.
func TestIngestFailures(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)

	id, j := upload(t, tid, documents.TypeText, longText(3))
	err := handle(t, pipeline(wrongDim{}), j)
	if !jobs.IsPermanent(err) {
		t.Errorf("dimension mismatch not permanent: %v", err)
	}
	if s := state(t, id); s.status != "failed" || s.chunks != 0 || s.errMsg == nil || !strings.HasPrefix(*s.errMsg, ingestion.ReasonDimensionMismatch) {
		t.Errorf("dim mismatch state %+v", s)
	}

	id, j = upload(t, tid, documents.TypeDOCX, []byte("PK\x03\x04 broken docx"))
	err = handle(t, pipeline(llm.Fake{}), j)
	if !jobs.IsPermanent(err) || errors.Is(err, context.Canceled) {
		t.Errorf("malformed docx: %v", err)
	}
	if s := state(t, id); s.status != "failed" || s.errMsg == nil || !strings.HasPrefix(*s.errMsg, ingestion.ReasonMalformed) {
		t.Errorf("malformed state %+v", s)
	}
}

// topics lists the outbox topics emitted for a document, in id order.
func topics(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	rows, err := env.Admin.Query(context.Background(),
		`SELECT topic FROM outbox WHERE payload->>'document_id' = $1 ORDER BY id`, id.String())
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type flaky struct{ llm.Fake }

func (flaky) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedding service unavailable")
}

// OBX-REQ-002 / ING-AC-008: indexed, failed and deleting events are emitted
// in the transaction of the state change they report.
func TestIngestEvents(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	ctx := context.Background()

	id, j := upload(t, tid, documents.TypeText, longText(5))
	if err := handle(t, pipeline(llm.Fake{}), j); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(topics(t, id), ","); got != "document.created,document.indexed" {
		t.Errorf("indexed: %s", got)
	}
	var chunks int
	_ = env.Admin.QueryRow(ctx, `SELECT (payload->>'chunks')::int FROM outbox WHERE topic = 'document.indexed' AND payload->>'document_id' = $1`, id.String()).Scan(&chunks)
	if chunks != state(t, id).chunks {
		t.Errorf("indexed event chunks %d, rows %d", chunks, state(t, id).chunks)
	}

	// Retryable failure: no event until the last attempt.
	id, j = upload(t, tid, documents.TypeText, longText(4))
	if err := handle(t, pipeline(flaky{}), j); err == nil || jobs.IsPermanent(err) {
		t.Fatalf("flaky embed: %v", err)
	}
	if got := strings.Join(topics(t, id), ","); got != "document.created" {
		t.Errorf("non-final failure emitted: %s", got)
	}
	j.Attempts = j.MaxAttempts
	_ = handle(t, pipeline(flaky{}), j)
	if got := strings.Join(topics(t, id), ","); got != "document.created,document.failed" {
		t.Errorf("final failure: %s", got)
	}
	var reason string
	_ = env.Admin.QueryRow(ctx, `SELECT payload->>'reason' FROM outbox WHERE topic = 'document.failed' AND payload->>'document_id' = $1`, id.String()).Scan(&reason)
	if reason != ingestion.ReasonEmbedding {
		t.Errorf("failed reason %q", reason)
	}

	// Deleted before Index: deleting, never indexed.
	id, j = upload(t, tid, documents.TypeText, longText(6))
	p := pipeline(llm.Fake{})
	p.BeforeIndex = func(ctx context.Context, _ uuid.UUID) {
		if _, err := docs.Delete(ctx, tid, id); err != nil {
			t.Error(err)
		}
	}
	if err := handle(t, p, j); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(topics(t, id), ","); got != "document.created,document.deleting" {
		t.Errorf("deleted before index: %s", got)
	}
}
