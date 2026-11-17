# 003 — Object Storage: Tasks

## Group 1: Interface and key discipline

- [x] 1.1 `internal/storage/objectstore`: `ObjectStore` interface + `DocumentKey` builder + unit test asserting key shape. (STO-REQ-001, STO-REQ-002, STO-AC-004)

## Group 2: Implementations

- [ ] 2.1 Cloud Storage implementation (put/get/delete/V4 signed URL via IAM `signBlob`) + compose `gcs` (`fake-gcs-server`) service in Cloud Shell. (STO-REQ-003)
- [ ] 2.1a *(written in `env/dev`, not applied)* Terraform: bucket per environment, uniform access, public access prevention, IAM for `sa-api`/`sa-worker` only. (STO-REQ-007, STO-AC-005)
- [ ] 2.2 *(done for the fs store; GCS pending)* Idempotent delete (map `ErrObjectNotExist` → success). (STO-REQ-006, STO-AC-006)

## Group 3: HTTP integration

- [x] 3.1 `GET /documents/{id}/content`: authz → 410 if deleted → stream; optional `?redirect=true` signed-URL path (TTL ≤ 5 min). (STO-REQ-004, STO-AC-001/002/003)
- [x] 3.2 *(`ingestion.Pipeline` reads through `ObjectStore.Get`)* Worker fetch switched to `ObjectStore.Get` with server credentials. (STO-REQ-005)

## Group 4: Tests and verification

- [ ] 4.1 Integration (testcontainers `fake-gcs-server`): round-trip, idempotent delete, streaming large object.
- [ ] 4.2 Security: content authz matrix; signed-URL TTL test; replay/tamper test against the real dev bucket in Cloud Build (STO-AC-005).
