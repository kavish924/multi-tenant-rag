# 005 — Ingestion Pipeline: Design

- **Spec ID:** 005 `ING` · **Milestone:** 2
- **Shared design:** [system-design.md §3](../../docs/system-design.md#3-ingestion-pipeline)

## 1. Context and goals

Turns a stored file into an indexed, tenant-scoped chunk set. Every stage idempotent; hostile
inputs contained; deletion respected mid-flight.

## 2. Scope

**In:** extract/chunk/embed/index stages, parser isolation, dimension validation, index tx with
deletion re-check, status transitions. **Out:** queue mechanics (`004`), events transport
(`006`), retrieval (`007`), physical deletion (`013`).

## 3. Components

```text
internal/ingestion/
├── pipeline.go     # stage orchestration, idempotent runner
├── extract/        # pdf, docx, txt extractors + subprocess runner (limits)
├── chunk/          # tokenizer + boundary-aware splitter (300–800 tok, 10–15% overlap)
└── embed/          # batcher, dimension validator, rate-limit-aware client
internal/llm        # EmbeddingProvider interface (provider implementations)
```

Parser isolation: `extract` spawns a helper subprocess per file (`go run`-built binary or
exec of the same binary with `-mode=extract`), with Linux resource limits on the child
(`setrlimit`/`prlimit`: address space, CPU time, open files — the worker always runs in a Linux
container on Cloud Run or the workstation), plus hard wall-clock timeout and output cap. Worker survives any
parser death.

## 4. Data model changes

`chunks` table (in `0001_init.sql`): per the shared schema — `UNIQUE (document_id, ordinal)`,
`vector(EMBEDDING_DIM)`, generated `tsv`, `embed_model`, HNSW + GIN + `(tenant_id, document_id)`
indexes. `EMBEDDING_DIM` is set from the chosen **Vertex AI** embedding model's output size and
must match the migration. pgvector HNSW supports `vector` up to 2,000 dims; above that use
`halfvec` (≤ 4,000) or request reduced output dimensionality (`docs/gcp-environment.md §8`).

## 5. API contract (internal)

```go
type Stage func(ctx context.Context, d *Document) error
type Pipeline struct{ Extract, Chunk, Embed, Index Stage }
Process(ctx, doc) // runs stages in order; job heartbeat between stages
```

Chunk record: `{ordinal, content, tokenCount, page, heading}`. Index transaction (inside
`WithTenant`), in this order:

```sql
SELECT deleted_at FROM documents WHERE id = $doc FOR UPDATE;   -- 1. lock; cancel if NOT NULL
INSERT INTO chunks (...) VALUES (...)                           -- 2. upsert (batched)
  ON CONFLICT (document_id, ordinal) DO UPDATE SET ...;
DELETE FROM chunks WHERE document_id = $doc AND ordinal >= $n;  -- 3. drop stale tail on re-run
UPDATE documents SET status = 'indexed', error = NULL           -- 4. guarded status write
  WHERE id = $doc AND deleted_at IS NULL;
```

The row lock is what makes deletion race-free: Delete (`002`) updates the same row, so the two
transactions serialize (reasoning in [system-design.md §3](../../docs/system-design.md#3-ingestion-pipeline)).
An unlocked `WHERE NOT EXISTS (... deleted_at IS NOT NULL)` guard was rejected: it reads a
snapshot and misses a Delete that commits mid-transaction. All other status writes
(`processing`, `failed`) also carry `AND deleted_at IS NULL` (ING-REQ-010).

## 6. Key flows

**Happy:** claim `ingest` job → status `processing` → Get object → Extract (subprocess) →
Chunk → Embed batches → Index tx → status `indexed` → outbox `document.indexed`.

**Retry after crash mid-embed:** job reclaimed → stages rerun from Extract (cheap) or from a
checkpoint (stage outputs are deterministic; default: full rerun — simplicity over micro-optimization);
upserts make it converge (ING-AC-004).

**Deleted mid-flight:** Index's locked read sees `deleted_at` → write nothing, job completes as
cancelled (ING-AC-005/009). If Index committed first, its chunks exist before Delete commits, and
the `013` cleanup job (enqueued by Delete) removes them.

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Parser limit exceeded | subprocess exit/timeout/oversize | fail job `parser_limit_exceeded` (ING-AC-002/007) |
| Wrong embedding dim | per-batch validation | fail `embedding_dimension_mismatch` (ING-AC-003) |
| Provider 429 | status code | in-stage bounded retry (ING-REQ-007) → else job retry ladder |
| Deleted during processing | locked read in Index tx | cancel (ING-AC-005/009) |
| Extract output empty | zero chunks | fail `no_text_extracted` |

## 8. Security and multi-tenancy

Parser isolation is the anti-attack-surface control (risk table: "Malicious uploaded files").
Chunks carry `tenant_id` from the document; all writes via `WithTenant` (RLS backstop).

## 9. Testing strategy

- Unit: chunker boundaries/ordinals stability (ING-REQ-004); dimension validator; tokenizer counts.
- Integration: PDF/DOCX/TXT fixtures end-to-end on Postgres + `fake-gcs-server` testcontainers with a fake `EmbeddingProvider` (ING-AC-001/006); one smoke test against real Vertex AI in the dev project.
- Failure: bomb/malformed fixtures (ING-AC-002/007); provider 429 simulation; wrong-dim mock (ING-AC-003);
  crash-between-stages rerun (ING-AC-004); delete-race (ING-AC-005).
- Load: NFR-001/NFR-002 profile.

## 10. Open questions

- Checkpointing stage outputs vs full rerun → **default:** full rerun (idempotent upserts; simpler).
- Tokenizer: `tiktoken`-style BPE via Go lib → **default:** any consistent tokenizer; document choice in code.
- Embedding model → **decide before migration 0001 lands**: pick the Vertex model, record its dimension in `EMBEDDING_DIM`, and choose `vector` vs `halfvec` accordingly.
