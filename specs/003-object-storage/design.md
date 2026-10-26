# 003 — Object Storage: Design

- **Spec ID:** 003 `STO` · **Milestone:** 2
- **Shared design:** [tech-stack.md Datastores](../../docs/tech-stack.md#datastores)

## 1. Context and goals

Isolation layer 3 lives here: tenant-prefixed keys + authorization before any signed URL.
Also the seam that keeps dev (MinIO) and prod (S3) identical.

## 2. Scope

**In:** `ObjectStore` interface, MinIO implementation, key builder, content endpoint wiring,
worker fetch path. **Out:** upload validation (`002`), cleanup jobs (`013`).

## 3. Components

- `internal/storage/objectstore` — interface + key builder:

```go
type ObjectStore interface {
    Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
    Get(ctx context.Context, key string) (io.ReadSeekCloser, error)
    Delete(ctx context.Context, key string) error // idempotent
    Presign(ctx context.Context, key string, ttl time.Duration) (string, error)
}

func DocumentKey(tenantID, documentID uuid.UUID) string {
    return fmt.Sprintf("tenants/%s/documents/%s", tenantID, documentID)
}
```

- MinIO impl via `minio-go`; S3 impl via same SDK (endpoint switch). Docker Compose service `minio`.

## 4. Data model changes

None (documents.object_key already stored by `002`; always produced by `DocumentKey`).

## 5. API contract

- `GET /documents/{id}/content` — membership-checked; streams `Content-Type` + `Content-Length`
  from DB metadata; `?redirect=true` → 302 to presigned URL (TTL ≤ 5 min, STO-AC-003).

## 6. Key flows

**Download:** handler loads document row via `WithTenant` (404 if not found → also covers
cross-tenant) → checks `deleted_at` (410) → server-side stream by default.
**Worker fetch:** `Get(DocumentKey(...))` with server credentials (STO-REQ-005).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Object missing but row exists | NoSuchKey | 404 + log (inconsistency signal) |
| Storage unreachable | client error | 502 |
| Presign failure | error | 500 |

## 8. Security and multi-tenancy

Single key-builder function (STO-AC-004) makes tenant-mismatched keys unrepresentable.
Bucket policy: no public access; presigned GET only. URLs are short-lived; reuse-across-tenant
test proves the boundary (STO-AC-005).

## 9. Testing strategy

- Integration (testcontainers MinIO): put/get/delete round-trip; idempotent delete (STO-AC-006);
  streaming large object (STO-NFR-001).
- Security: content endpoint authz matrix (STO-AC-001/002); presign TTL + membership precondition
  (STO-AC-003); key-builder unit test (STO-AC-004).

## 10. Open questions

- Multipart upload for very large files → **default:** not needed at 50 MB cap; revisit if cap raises.
