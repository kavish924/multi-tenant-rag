# 005 — Ingestion Pipeline: Tasks

## Group 1: Extract

- [ ] 1.1 TXT extractor (charset-safe). (ING-REQ-001)
- [ ] 1.2 PDF extractor + DOCX extractor in isolated subprocess runner with Linux rlimits (memory/CPU/files) + wall-clock and output limits. (ING-REQ-001/002, ING-AC-002)
- [ ] 1.3 Fixture corpus incl. hostile files (bomb, malformed PDF, huge docx). (ING-AC-007)

## Group 2: Chunk

- [ ] 2.1 Tokenizer + paragraph/heading boundary splitter (300–800 tok, 10–15% overlap); metadata capture. (ING-REQ-003, ING-AC-001)
- [ ] 2.2 Ordinal-stability property test: same input → identical ordinals; changed input → deterministic re-ordinaling. (ING-REQ-004)

## Group 3: Embed

- [ ] 3.1 Batcher (configurable size/concurrency) over `EmbeddingProvider`; Vertex AI implementation (service-account auth) + deterministic fake for tests. (ING-REQ-005)
- [ ] 3.2 Dimension validation per batch → typed failure. (ING-REQ-005, ING-AC-003)
- [ ] 3.3 `embed_model` stamping + provider 429 bounded retry. (ING-REQ-006/007)

## Group 4: Index + status

- [ ] 4.1 Index tx: `SELECT ... FOR UPDATE` on the document (cancel if deleted), upsert chunks, drop stale tail ordinals, guarded status → `indexed`. (ING-REQ-008, ING-AC-005/006)
- [ ] 4.2 Failure path: status `failed` + `error` reason mapping table; every status write guarded by `deleted_at IS NULL`; job on an already-deleted document completes as cancelled. (ING-REQ-009, ING-REQ-010, ING-AC-006)
- [ ] 4.2a Interleaving test: hold the Index tx before its locked read, commit Delete on a second connection, release → no chunks, status still `deleting`. (ING-AC-009)
- [ ] 4.3 Outbox emission points `document.indexed` / `document.failed` (contract with `006`). (ING-AC-008)

## Group 5: Tests and verification

- [ ] 5.1 Idempotent rerun test: crash after embed → rerun → no duplicate rows (ING-AC-004).
- [ ] 5.2 End-to-end fixture tests on Postgres + `fake-gcs-server` testcontainers. (ING-AC-001/006)
- [ ] 5.3 Load profile: 50 MB PDF p95; worker RSS cap. (ING-NFR-001/002)
- [ ] 5.4 **Milestone 2 exit contribution:** kill worker mid-job → document state remains consistent.
