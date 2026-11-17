// Package documents implements document upload, metadata, listing and soft
// delete (spec 002) on top of the object store (003) and job queue (004).
package documents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
)

// Status values (documents.status CHECK).
const (
	StatusUploaded   = "uploaded"
	StatusQueued     = "queued"
	StatusProcessing = "processing"
	StatusIndexed    = "indexed"
	StatusFailed     = "failed"
	StatusDeleting   = "deleting"
	StatusDeleted    = "deleted"
)

// Job kinds this package enqueues.
const (
	JobIngest  = "ingest"
	JobCleanup = "cleanup"
)

// DefaultMaxUploadBytes is the default upload cap (DOC-REQ-003).
const DefaultMaxUploadBytes int64 = 50 << 20

// Errors mapped to HTTP statuses by the API.
var (
	ErrTooLarge        = errors.New("file exceeds the upload size limit")
	ErrUnsupportedType = errors.New("file type must be PDF, DOCX or UTF-8 text, and match the declared type")
	ErrNotFound        = errors.New("document not found")
	ErrGone            = errors.New("document has been deleted")
	ErrStorage         = errors.New("object storage unavailable")
	ErrBadCursor       = errors.New("invalid cursor")
	ErrBadStatus       = errors.New("invalid status filter")
)

// Document is document metadata. object_key and tenant_id stay internal.
type Document struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"-"`
	Filename    string     `json:"filename"`
	ContentType string     `json:"content_type"`
	SizeBytes   int64      `json:"size_bytes"`
	SHA256      string     `json:"sha256"`
	Status      string     `json:"status"`
	Error       *string    `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	ObjectKey   string     `json:"-"`
}

const cols = `id, tenant_id, filename, content_type, size_bytes, sha256, status, error,
	created_at, updated_at, deleted_at, object_key`

func scanDoc(r pgx.Row) (Document, error) {
	var d Document
	err := r.Scan(&d.ID, &d.TenantID, &d.Filename, &d.ContentType, &d.SizeBytes, &d.SHA256,
		&d.Status, &d.Error, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt, &d.ObjectKey)
	return d, err
}

// Service is the document service. Every query runs inside WithTenant.
type Service struct {
	store    *storage.Store
	objects  objectstore.ObjectStore
	maxBytes int64
}

// NewService builds the service; maxBytes <= 0 uses DefaultMaxUploadBytes.
func NewService(store *storage.Store, objects objectstore.ObjectStore, maxBytes int64) *Service {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxUploadBytes
	}
	return &Service{store: store, objects: objects, maxBytes: maxBytes}
}

// MaxBytes is the upload cap.
func (s *Service) MaxBytes() int64 { return s.maxBytes }

// UploadInput is one file to upload into the verified tenant.
type UploadInput struct {
	TenantID     uuid.UUID
	Filename     string
	DeclaredType string
	Body         io.Reader
}

// UploadResult reports the stored document, or the existing one on a
// per-tenant duplicate (DOC-AC-003).
type UploadResult struct {
	Document  Document
	Duplicate bool
}

// Upload validates, streams to object storage while hashing, then inserts
// the document row and its ingest job in one transaction (DOC-REQ-005).
// Nothing persists if any step fails; the stored object is deleted
// (DOC-REQ-006).
func (s *Service) Upload(ctx context.Context, in UploadInput) (UploadResult, error) {
	declared, ok := NormalizeDeclared(in.DeclaredType)
	if !ok {
		return UploadResult{}, ErrUnsupportedType
	}

	body := &limitReader{r: in.Body, remaining: s.maxBytes}
	head := make([]byte, SniffLen)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		if errors.Is(err, ErrTooLarge) {
			return UploadResult{}, ErrTooLarge
		}
		return UploadResult{}, fmt.Errorf("documents: read upload: %w", err)
	}
	head = head[:n]
	if Detect(head) != declared {
		return UploadResult{}, ErrUnsupportedType
	}

	doc := Document{
		ID:          uuid.New(),
		TenantID:    in.TenantID,
		Filename:    cleanFilename(in.Filename),
		ContentType: declared,
		Status:      StatusQueued,
	}
	doc.ObjectKey = objectstore.DocumentKey(doc.TenantID, doc.ID)

	hash := sha256.New()
	counter := &countingWriter{}
	stream := io.TeeReader(io.MultiReader(bytes.NewReader(head), body), io.MultiWriter(hash, counter))
	if err := s.objects.Put(ctx, doc.ObjectKey, stream, -1, declared); err != nil {
		s.discard(doc.ObjectKey)
		if errors.Is(err, ErrTooLarge) {
			return UploadResult{}, ErrTooLarge
		}
		return UploadResult{}, fmt.Errorf("%w: %w", ErrStorage, err)
	}
	doc.SizeBytes = counter.n
	doc.SHA256 = hex.EncodeToString(hash.Sum(nil))

	var res UploadResult
	err = s.store.WithTenant(ctx, doc.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO documents (id, tenant_id, filename, content_type, size_bytes, object_key, sha256, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, sha256) WHERE deleted_at IS NULL DO NOTHING
			RETURNING `+cols,
			doc.ID, doc.TenantID, doc.Filename, doc.ContentType, doc.SizeBytes, doc.ObjectKey, doc.SHA256, doc.Status)
		created, err := scanDoc(row)
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err := scanDoc(tx.QueryRow(ctx,
				`SELECT `+cols+` FROM documents WHERE tenant_id = $1 AND sha256 = $2 AND deleted_at IS NULL`,
				doc.TenantID, doc.SHA256))
			if err != nil {
				return fmt.Errorf("documents: load duplicate: %w", err)
			}
			res = UploadResult{Document: existing, Duplicate: true}
			return nil
		}
		if err != nil {
			return fmt.Errorf("documents: insert: %w", err)
		}
		if _, _, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{
			TenantID:       doc.TenantID,
			Kind:           JobIngest,
			Payload:        map[string]string{"document_id": doc.ID.String()},
			IdempotencyKey: JobIngest + ":" + doc.ID.String(),
		}); err != nil {
			return err
		}
		res = UploadResult{Document: created}
		return nil
	})
	if err != nil {
		s.discard(doc.ObjectKey)
		return UploadResult{}, err
	}
	if res.Duplicate {
		s.discard(doc.ObjectKey) // the existing document keeps its own object
	}
	return res, nil
}

// discard is the best-effort compensating delete (DOC-REQ-006). A failure
// leaves an orphan object for the retention sweeper (013).
func (s *Service) discard(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.objects.Delete(ctx, key)
}

// Get returns a document of the tenant (deleted ones included).
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Document, error) {
	var d Document
	err := s.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		d, err = scanDoc(tx.QueryRow(ctx, `SELECT `+cols+` FROM documents WHERE id = $1 AND tenant_id = $2`, id, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("documents: get: %w", err)
	}
	return d, nil
}

// ListParams filters and pages a listing (DOC-REQ-007).
type ListParams struct {
	Limit          int
	Cursor         string
	Status         string
	IncludeDeleted bool
}

// MaxListLimit bounds page size.
const MaxListLimit = 100

// List returns one page, newest first, and the cursor for the next page ("" at the end).
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, p ListParams) ([]Document, string, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > MaxListLimit {
		p.Limit = MaxListLimit
	}
	if p.Status != "" && !validStatus(p.Status) {
		return nil, "", ErrBadStatus
	}
	var curTime *time.Time
	var curID *uuid.UUID
	if p.Cursor != "" {
		t, id, err := decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", ErrBadCursor
		}
		curTime, curID = &t, &id
	}
	var status *string
	if p.Status != "" {
		status = &p.Status
	}

	var out []Document
	err := s.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+cols+` FROM documents
			WHERE tenant_id = $1
			  AND ($2 OR deleted_at IS NULL)
			  AND ($3::text IS NULL OR status = $3)
			  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5::uuid))
			ORDER BY created_at DESC, id DESC
			LIMIT $6`,
			tenantID, p.IncludeDeleted, status, curTime, curID, p.Limit+1)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Document, error) { return scanDoc(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("documents: list: %w", err)
	}
	next := ""
	if len(out) > p.Limit {
		out = out[:p.Limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	if out == nil {
		out = []Document{}
	}
	return out, next, nil
}

// Delete soft-deletes a document and enqueues its cleanup job in one
// transaction (DOC-REQ-008). The UPDATE takes the row lock that serializes
// with the ingestion Index transaction (005). Deleting an already deleted
// document is a no-op that returns it.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID) (Document, error) {
	var d Document
	err := s.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		d, err = scanDoc(tx.QueryRow(ctx, `
			UPDATE documents SET deleted_at = now(), status = 'deleting'
			WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
			RETURNING `+cols, id, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			d, err = scanDoc(tx.QueryRow(ctx, `SELECT `+cols+` FROM documents WHERE id = $1 AND tenant_id = $2`, id, tenantID))
			return err // already deleting/deleted, or not found
		}
		if err != nil {
			return err
		}
		_, _, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{
			TenantID:       tenantID,
			Kind:           JobCleanup,
			Payload:        map[string]string{"document_id": id.String()},
			IdempotencyKey: JobCleanup + ":" + id.String(),
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("documents: delete: %w", err)
	}
	return d, nil
}

// Open returns the document and a stream of its original content. Deleted
// documents are refused with ErrGone (DOC-AC-008).
func (s *Service) Open(ctx context.Context, tenantID, id uuid.UUID) (Document, io.ReadCloser, error) {
	d, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return Document{}, nil, err
	}
	if d.DeletedAt != nil {
		return Document{}, nil, ErrGone
	}
	r, err := s.objects.Get(ctx, d.ObjectKey)
	if errors.Is(err, objectstore.ErrNotFound) {
		return Document{}, nil, fmt.Errorf("documents: object missing for %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Document{}, nil, fmt.Errorf("%w: %w", ErrStorage, err)
	}
	return d, r, nil
}

// Presign returns a short-lived download URL after the same checks as Open.
func (s *Service) Presign(ctx context.Context, tenantID, id uuid.UUID) (string, error) {
	d, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return "", err
	}
	if d.DeletedAt != nil {
		return "", ErrGone
	}
	return s.objects.Presign(ctx, d.ObjectKey, objectstore.MaxPresignTTL)
}

func validStatus(s string) bool {
	switch s {
	case StatusUploaded, StatusQueued, StatusProcessing, StatusIndexed, StatusFailed, StatusDeleting, StatusDeleted:
		return true
	}
	return false
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	ts, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(idStr)
	return t, id, err
}

// cleanFilename keeps only a safe base name for display.
func cleanFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" || name == ".." {
		name = "upload"
	}
	for utf8.RuneCountInString(name) > 255 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// limitReader fails with ErrTooLarge once more than remaining bytes are read,
// so oversize uploads abort mid-stream without buffering (DOC-REQ-003).
type limitReader struct {
	r         io.Reader
	remaining int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.remaining < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
