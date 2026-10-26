# 003 — Object Storage: Tasks

## Group 1: Interface and key discipline

- [ ] 1.1 `internal/storage/objectstore`: `ObjectStore` interface + `DocumentKey` builder + unit test asserting key shape. (STO-REQ-001, STO-REQ-002, STO-AC-004)

## Group 2: Implementations

- [ ] 2.1 MinIO implementation (put/get/delete/presign) + docker-compose `minio` service + bucket bootstrap (create, private policy). (STO-REQ-003, STO-AC-005)
- [ ] 2.2 Idempotent delete (map NoSuchKey → success). (STO-REQ-006, STO-AC-006)

## Group 3: HTTP integration

- [ ] 3.1 `GET /documents/{id}/content`: authz → 410 if deleted → stream; optional `?redirect=true` presign path (TTL ≤ 5 min). (STO-REQ-004, STO-AC-001/002/003)
- [ ] 3.2 Worker fetch switched to `ObjectStore.Get` with server credentials. (STO-REQ-005)

## Group 4: Tests and verification

- [ ] 4.1 Integration (testcontainers): round-trip, idempotent delete, streaming large object.
- [ ] 4.2 Security: content authz matrix; presign TTL test; replayed presigned URL cross-tenant test (STO-AC-005).
