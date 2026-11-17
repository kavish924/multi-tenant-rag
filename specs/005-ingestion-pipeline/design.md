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
├── pipeline.go     # ingest job handler: start → extract → chunk → embed → index
├── extract/        # txt, docx (encoding/xml), pdf (github.com/ledongthuc/pdf) + Isolated helper runner
├── chunk/          # tokenizer + boundary-aware splitter (300–800 tok, ~12% overlap)
└── embed/          # batcher, dimension validator, bounded 429 retry
internal/llm        # EmbeddingProvider interface, EmbeddingDim = 768, deterministic Fake
```

Parser isolation: `extract.Isolated` re-executes the worker binary with `extract.HelperArg`
per file. The child gets a minimal environment (no database or cloud credentials), sets its own
Linux rlimits before parsing (`RLIMIT_AS` 1 GiB, `RLIMIT_CPU` 60 s, `RLIMIT_NOFILE` 32,
`RLIMIT_FSIZE` 0 — the worker always runs in a Linux container on Cloud Run or in Cloud Shell),
reads the file on stdin and writes JSON sections to stdout. The parent enforces a wall-clock
timeout (2 min) and caps on input, stdout and stderr. Exit codes map to `ErrLimit`
(`parser_limit_exceeded`), `ErrMalformed` (`extract_failed`) and `ErrUnsupported`; a crash or
rlimit signal counts as `parser_limit_exceeded`. The worker survives any parser death.
DOCX additionally caps zip entries and the inflated size of `word/document.xml` (bomb defense).

## 4. Data model changes

`chunks` table (`migrations/0006_chunks.sql`): per the shared schema — `UNIQUE (document_id, ordinal)`,
`vector(768)`, a composite FK `(tenant_id, document_id) → documents (tenant_id, id)` so a chunk cannot
point at another tenant's document, generated `tsv`, `embed_model`, HNSW + GIN + `(tenant_id, document_id)`
indexes. `EMBEDDING_DIM` is set from the chosen **Vertex AI** embedding model's output size and
must match the migration. pgvector HNSW supports `vector` up to 2,000 dims; above that use
`halfvec` (≤ 4,000) or request reduced output dimensionality (`docs/gcp-environment.md §8`).

## 5. API contract (internal)

Implemented as `(*ingestion.Pipeline).Handle(ctx, job, tenantTx)`, a `jobs.Handler`. Stage
failures carry a reason code and a permanent flag: permanent ones (parser limit, malformed,
no text, dimension mismatch, object missing) dead-letter the job at once; others retry.


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
- Tokenizer → **decided:** `chunk.CountTokens`, a dependency-free approximation (one token per
  started 6 letters/digits, one per other symbol). Consistent, not model-exact; sizes are approximate.
- Embedding model → **decided:** 768 dimensions (`llm.EmbeddingDim`, `vector(768)`), which fits
  Vertex `text-embedding-005` natively or `gemini-embedding-001` with `output_dimensionality = 768`.
  The Vertex provider lands with the GCP work; until then the worker uses `llm.Fake`.
- Outbox events (4.3) → wired when `006` adds the `outbox` table.
