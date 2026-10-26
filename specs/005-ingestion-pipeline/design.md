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
exec of the same binary with `-mode=extract`), with `ulimit`-style limits via OS job objects/
cgroups where available, plus hard wall-clock timeout and output cap. Worker survives any
parser death.

## 4. Data model changes

`chunks` table (in `0001_init.sql`): per the shared schema — `UNIQUE (document_id, ordinal)`,
`vector(1536)`, generated `tsv`, `embed_model`, HNSW + GIN + `(tenant_id, document_id)` indexes.
Embedding dimension is config (`EMBEDDING_DIM`, default 1536) and must match migration.

## 5. API contract (internal)

```go
type Stage func(ctx context.Context, d *Document) error
type Pipeline struct{ Extract, Chunk, Embed, Index Stage }
Process(ctx, doc) // runs stages in order; job heartbeat between stages
```

Chunk record: `{ordinal, content, tokenCount, page, heading}`. Index write: `INSERT ... ON
CONFLICT (document_id, ordinal) DO UPDATE` inside `WithTenant`, with
`WHERE NOT EXISTS (SELECT 1 FROM documents WHERE id=$doc AND deleted_at IS NOT NULL)` guard —
or pre-check `deleted_at IS NULL` + row lock.

## 6. Key flows

**Happy:** claim `ingest` job → status `processing` → Get object → Extract (subprocess) →
Chunk → Embed batches → Index tx → status `indexed` → outbox `document.indexed`.

**Retry after crash mid-embed:** job reclaimed → stages rerun from Extract (cheap) or from a
checkpoint (stage outputs are deterministic; default: full rerun — simplicity over micro-optimization);
upserts make it converge (ING-AC-004).

**Deleted mid-flight:** Index pre-check sees `deleted_at` → cancel (ING-AC-005); `013` cleanup
removes any partial rows.

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Parser limit exceeded | subprocess exit/timeout/oversize | fail job `parser_limit_exceeded` (ING-AC-002/007) |
| Wrong embedding dim | per-batch validation | fail `embedding_dimension_mismatch` (ING-AC-003) |
| Provider 429 | status code | in-stage bounded retry (ING-REQ-007) → else job retry ladder |
| Deleted during processing | Index pre-check | cancel (ING-AC-005) |
| Extract output empty | zero chunks | fail `no_text_extracted` |

## 8. Security and multi-tenancy

Parser isolation is the anti-attack-surface control (risk table: "Malicious uploaded files").
Chunks carry `tenant_id` from the document; all writes via `WithTenant` (RLS backstop).

## 9. Testing strategy

- Unit: chunker boundaries/ordinals stability (ING-REQ-004); dimension validator; tokenizer counts.
- Integration: PDF/DOCX/TXT fixtures end-to-end on testcontainers (ING-AC-001/006).
- Failure: bomb/malformed fixtures (ING-AC-002/007); provider 429 simulation; wrong-dim mock (ING-AC-003);
  crash-between-stages rerun (ING-AC-004); delete-race (ING-AC-005).
- Load: NFR-001/NFR-002 profile.

## 10. Open questions

- Checkpointing stage outputs vs full rerun → **default:** full rerun (idempotent upserts; simpler).
- Tokenizer: `tiktoken`-style BPE via Go lib → **default:** any consistent tokenizer; document choice in code.
