// Package objectstore is the storage seam for original files (spec 003):
// one small interface, a single key builder (isolation layer 3), and
// implementations. The filesystem implementation serves local development
// and tests; the Cloud Storage implementation is added with the GCP work.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors returned by implementations.
var (
	ErrNotFound          = errors.New("objectstore: object not found")
	ErrPresignNotSupport = errors.New("objectstore: signed URLs not supported by this store")
	ErrInvalidKey        = errors.New("objectstore: key is not a document key")
)

// MaxPresignTTL bounds signed URL lifetime (STO-AC-003).
const MaxPresignTTL = 5 * time.Minute

// ObjectStore stores original documents by key (STO-REQ-001).
type ObjectStore interface {
	// Put stores r under key. size is -1 when unknown (streamed uploads).
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get streams an object; ErrNotFound when missing.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes an object; deleting a missing object succeeds (STO-REQ-006).
	Delete(ctx context.Context, key string) error
	// Presign returns a short-lived download URL (ttl ≤ MaxPresignTTL).
	Presign(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// DocumentKey is the only way to build an object key (STO-REQ-002,
// STO-AC-004): tenants/{tenant_id}/documents/{document_id}.
func DocumentKey(tenantID, documentID uuid.UUID) string {
	return fmt.Sprintf("tenants/%s/documents/%s", tenantID, documentID)
}

// ParseDocumentKey validates a key and returns its ids. Implementations call
// it before touching storage, so a malformed or traversal key never reaches
// the backend.
func ParseDocumentKey(key string) (tenantID, documentID uuid.UUID, err error) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != "tenants" || parts[2] != "documents" {
		return uuid.Nil, uuid.Nil, ErrInvalidKey
	}
	t, err1 := uuid.Parse(parts[1])
	d, err2 := uuid.Parse(parts[3])
	if err1 != nil || err2 != nil || t == uuid.Nil || d == uuid.Nil ||
		DocumentKey(t, d) != key { // canonical form only (lower-case, hyphenated)
		return uuid.Nil, uuid.Nil, ErrInvalidKey
	}
	return t, d, nil
}
