// Package ingestion runs the ingest job: Extract → Chunk → Embed → Index
// (spec 005). The Index transaction emits document.indexed and a final
// failure emits document.failed (spec 006). Every stage is idempotent, so a crash and retry converges to
// the same rows; deletion is respected at every status write and, decisively,
// by the row lock taken in the Index transaction.
package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/kavish924/multi-tenant-rag/internal/ingestion/chunk"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/embed"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/messaging"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
)

// JobKind is the queue kind this pipeline handles.
const JobKind = "ingest"

// Failure reasons recorded in documents.error (ING-REQ-009).
const (
	ReasonParserLimit       = "parser_limit_exceeded"
	ReasonMalformed         = "extract_failed"
	ReasonUnsupported       = "unsupported_type"
	ReasonNoText            = "no_text_extracted"
	ReasonDimensionMismatch = "embedding_dimension_mismatch"
	ReasonEmbedding         = "embedding_failed"
	ReasonObjectMissing     = "object_missing"
	ReasonStorage           = "storage_error"
	ReasonInternal          = "internal_error"
)

// Extractor converts a stored file to sections (extract.Isolated in production).
type Extractor interface {
	Extract(ctx context.Context, contentType string, r io.Reader) ([]extract.Section, error)
}

// Pipeline is the ingest job handler.
type Pipeline struct {
	Objects   objectstore.ObjectStore
	Extractor Extractor
	Embedder  *embed.Batcher
	Chunking  chunk.Options // zero = chunk.Defaults
	Log       *slog.Logger

	// BeforeIndex, when set, runs just before the Index transaction takes
	// the document row lock. Tests use it to schedule interleavings
	// (ING-AC-009); production leaves it nil.
	BeforeIndex func(ctx context.Context, documentID uuid.UUID)
}

type payload struct {
	DocumentID uuid.UUID `json:"document_id"`
}

// stageError carries the reason to record and whether a retry can help.
type stageError struct {
	reason    string
	permanent bool
	err       error
}

func (e *stageError) Error() string { return e.reason + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func fail(reason string, permanent bool, err error) error {
	return &stageError{reason: reason, permanent: permanent, err: err}
}

// Handle implements jobs.Handler.
func (p *Pipeline) Handle(ctx context.Context, job jobs.Job, tx jobs.TenantTx) error {
	log := p.log().With("job_id", job.ID, "tenant_id", job.TenantID)
	var pl payload
	if err := json.Unmarshal(job.Payload, &pl); err != nil || pl.DocumentID == uuid.Nil {
		return jobs.Permanent(fmt.Errorf("ingest: bad payload: %s", job.Payload))
	}
	docID := pl.DocumentID
	log = log.With("document_id", docID)

	doc, ok, err := p.start(ctx, tx, docID)
	if err != nil {
		return err
	}
	if !ok {
		log.InfoContext(ctx, "document deleted or gone; ingest cancelled")
		return nil // completes as cancelled (ING-REQ-010)
	}

	n, cancelled, err := p.run(ctx, tx, job.TenantID, docID, doc)
	if err != nil {
		var se *stageError
		if !errors.As(err, &se) {
			se = &stageError{reason: ReasonInternal, err: err}
		}
		// document.failed is emitted only when ingestion gives up: a permanent
		// failure or the job's last attempt (ING-AC-008).
		final := se.permanent || job.Attempts >= job.MaxAttempts
		p.markFailed(ctx, tx, job.TenantID, docID, se, final)
		observability.M().IngestFailure(ctx, se.reason)
		log.WarnContext(ctx, "ingest failed", "reason", se.reason, "permanent", se.permanent, "err", se.err)
		if se.permanent {
			return jobs.Permanent(err)
		}
		return err
	}
	if cancelled {
		log.InfoContext(ctx, "document deleted before index; no chunks written")
		return nil
	}
	log.InfoContext(ctx, "document indexed", "chunks", n)
	return nil
}

type docInfo struct {
	objectKey   string
	contentType string
}

// start moves the document to processing. ok=false means it was deleted (or
// removed) and the job should finish without work.
func (p *Pipeline) start(ctx context.Context, tx jobs.TenantTx, id uuid.UUID) (docInfo, bool, error) {
	var d docInfo
	found := false
	err := tx(ctx, func(t pgx.Tx) error {
		err := t.QueryRow(ctx, `
			UPDATE documents SET status = 'processing', error = NULL
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING object_key, content_type`, id).Scan(&d.objectKey, &d.contentType)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return docInfo{}, false, fmt.Errorf("ingest: start: %w", err)
	}
	return d, found, nil
}

func (p *Pipeline) run(ctx context.Context, tx jobs.TenantTx, tenantID, docID uuid.UUID, d docInfo) (int, bool, error) {
	// Extract (isolated helper process).
	secs, err := p.extract(ctx, d)
	switch {
	case errors.Is(err, objectstore.ErrNotFound):
		return 0, false, fail(ReasonObjectMissing, true, err)
	case errors.Is(err, errStorage):
		return 0, false, fail(ReasonStorage, false, err)
	case errors.Is(err, extract.ErrLimit):
		return 0, false, fail(ReasonParserLimit, true, err)
	case errors.Is(err, extract.ErrMalformed):
		return 0, false, fail(ReasonMalformed, true, err)
	case errors.Is(err, extract.ErrUnsupported):
		return 0, false, fail(ReasonUnsupported, true, err)
	case err != nil:
		return 0, false, fail(ReasonInternal, false, err)
	}

	// Chunk.
	opts := p.Chunking
	if opts.MaxTokens == 0 {
		opts = chunk.Defaults
	}
	_, span := observability.Start(ctx, "chunk")
	chunks := chunk.Split(secs, opts)
	span.SetAttributes(attribute.Int("chunks", len(chunks)))
	span.End()
	if len(chunks) == 0 {
		return 0, false, fail(ReasonNoText, true, errors.New("no text could be extracted"))
	}

	// Embed: every vector validated before any write (ING-AC-003).
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}
	vecs, err := p.Embedder.Embed(ctx, texts)
	switch {
	case errors.Is(err, embed.ErrDimensionMismatch):
		return 0, false, fail(ReasonDimensionMismatch, true, err)
	case err != nil:
		return 0, false, fail(ReasonEmbedding, false, err)
	}

	// Index.
	if p.BeforeIndex != nil {
		p.BeforeIndex(ctx, docID)
	}
	ictx, span := observability.Start(ctx, "index write", attribute.Int("chunks", len(chunks)))
	cancelled, err := p.index(ictx, tx, tenantID, docID, chunks, vecs)
	observability.End(span, err)
	if err != nil {
		return 0, false, fail(ReasonInternal, false, err)
	}
	return len(chunks), cancelled, nil
}

var errStorage = errors.New("object storage error")

// extract reads the object and parses it in the helper process.
func (p *Pipeline) extract(ctx context.Context, d docInfo) (secs []extract.Section, err error) {
	ctx, span := observability.Start(ctx, "extract", attribute.String("content_type", d.contentType))
	defer func() { observability.End(span, err) }()
	body, err := p.Objects.Get(ctx, d.objectKey)
	if errors.Is(err, objectstore.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStorage, err)
	}
	defer func() { _ = body.Close() }()
	return p.Extractor.Extract(ctx, d.contentType, body)
}

// index writes chunks and marks the document indexed in one transaction
// that first locks the document row (ING-REQ-008). A Delete that committed
// before the lock, or while we waited on it, is seen here, and nothing is
// written (ING-AC-005/009).
func (p *Pipeline) index(ctx context.Context, tx jobs.TenantTx, tenantID, docID uuid.UUID, chunks []chunk.Chunk, vecs [][]float32) (cancelled bool, err error) {
	model := p.Embedder.Provider.Model()
	err = tx(ctx, func(t pgx.Tx) error {
		var deleted bool
		if err := t.QueryRow(ctx,
			`SELECT deleted_at IS NOT NULL FROM documents WHERE id = $1 FOR UPDATE`, docID).Scan(&deleted); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				cancelled = true
				return nil
			}
			return err
		}
		if deleted {
			cancelled = true
			return nil
		}

		const batchSize = 200
		for start := 0; start < len(chunks); start += batchSize {
			b := &pgx.Batch{}
			for i := start; i < min(start+batchSize, len(chunks)); i++ {
				c := chunks[i]
				b.Queue(`
					INSERT INTO chunks (tenant_id, document_id, ordinal, content, token_count, page, heading, embed_model, embedding)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::vector)
					ON CONFLICT (document_id, ordinal) DO UPDATE SET
					  content = EXCLUDED.content, token_count = EXCLUDED.token_count,
					  page = EXCLUDED.page, heading = EXCLUDED.heading,
					  embed_model = EXCLUDED.embed_model, embedding = EXCLUDED.embedding,
					  updated_at = now()`,
					tenantID, docID, c.Ordinal, c.Content, c.TokenCount,
					nullInt(c.Page), nullString(c.Heading), model, storage.VectorLiteral(vecs[i]))
			}
			if err := t.SendBatch(ctx, b).Close(); err != nil {
				return fmt.Errorf("upsert chunks: %w", err)
			}
		}
		// A shorter re-run leaves no stale tail (ING-REQ-010 idempotency).
		if _, err := t.Exec(ctx, `DELETE FROM chunks WHERE document_id = $1 AND ordinal >= $2`, docID, len(chunks)); err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `
			UPDATE documents SET status = 'indexed', error = NULL
			WHERE id = $1 AND deleted_at IS NULL`, docID); err != nil {
			return err
		}
		_, err := messaging.Emit(ctx, t, tenantID, messaging.DocumentIndexed{
			DocumentID: docID, Chunks: len(chunks), EmbedModel: model,
		})
		return err
	})
	return cancelled, err
}

// markFailed records the reason, never overwriting a deletion (ING-REQ-010),
// and with final set emits document.failed in the same transaction. It is
// best effort: the job's own failure handling still applies.
func (p *Pipeline) markFailed(ctx context.Context, tx jobs.TenantTx, tenantID, id uuid.UUID, se *stageError, final bool) {
	msg := se.reason + ": " + se.err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	ctx = context.WithoutCancel(ctx) // record the failure even when the job is being cancelled
	err := tx(ctx, func(t pgx.Tx) error {
		tag, err := t.Exec(ctx, `
			UPDATE documents SET status = 'failed', error = $2
			WHERE id = $1 AND deleted_at IS NULL`, id, msg)
		if err != nil || !final || tag.RowsAffected() == 0 {
			return err // a deleted document gets no failure event
		}
		_, err = messaging.Emit(ctx, t, tenantID, messaging.DocumentFailed{DocumentID: id, Reason: se.reason})
		return err
	})
	if err != nil {
		p.log().Error("mark document failed", "document_id", id, "err", err)
	}
}

func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func nullInt(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
