# 007 — Hybrid Retrieval: Design

- **Spec ID:** 007 `RET` · **Milestone:** 3
- **Shared design:** [system-design.md §5](../../docs/system-design.md#5-hybrid-retrieval)

## 1. Context and goals

The retrieval half of the read path. Two complementary legs + normalization-free fusion.
Correctness boundary: **tenant-scoped, deleted-excluded, dominance-capped** results.

## 2. Scope

**In:** `internal/retrieval` (SQL, fusion, caps), question embedding call. **Out:** reranking
(`009`), generation/citations (`008`), index build (`005`).

## 3. Components

- `internal/retrieval`: `Retriever` interface, `hybrid.sql` (embedded), per-doc cap post-pass,
  `Hit` type.
- Question embedding via `llm.EmbeddingProvider` (same provider/config as ingestion).

## 4. Data model changes

None new (indexes defined in `005`). Tunables: HNSW build params; over-fetch factor
(vector leg LIMIT 50 × factor when iterative scans unavailable, RET-REQ-007).

## 5. API contract (internal)

```go
type Hit struct {
    ChunkID, DocumentID uuid.UUID
    Content string; Page int; Heading string
    Score float64; Rank int
}
Search(ctx, tenantID uuid.UUID, question string, limit int) ([]Hit, error)
```

SQL per shared design (two CTEs `vec`, `fts` with `row_number()`, LEFT JOINs, RRF
`COALESCE(1.0/(60+vec.rnk),0)+COALESCE(1.0/(60+fts.rnk),0)`), plus:

```sql
JOIN documents d ON d.id = c.document_id
WHERE c.tenant_id = $1 AND d.deleted_at IS NULL AND d.status <> 'deleting'
```

Per-doc cap: `row_number() OVER (PARTITION BY c.document_id ORDER BY score DESC) <= $cap`.

## 6. Key flows

**Search:** validate question → embed (span) → run hybrid.sql (span) → cap pass → return hits.
The whole function runs inside API's tenant context; a single statement sees only tenant rows
(RLS backstop even if SQL regressed).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Embedding provider down | error | 502 upstream (generation spec maps it) |
| Empty result set | zero hits | not an error — generation handles "insufficient context" |
| Question too long | length guard (`012` input layer) | 400 |

## 8. Security and multi-tenancy

Defense in depth: tenant filter in SQL (layer 1) + RLS (layer 2) + per-doc cap (guardrail
retrieval layer). Direct chunk-id probing tests prove cross-tenant invisibility (RET-AC-001).

## 9. Testing strategy

- Unit: RRF math (RET-AC-007); cap logic.
- Integration (seeded corpus): lexical-only hit (AC-004), semantic-only hit (AC-005), deleted
  exclusion (AC-002), multi-tenant corpus with adversarial probe (AC-001), ordering/limit edge (AC-008).
- Performance: latency benchmark vs RET-NFR-001; small-tenant recall check (RET-REQ-007).

## 10. Open questions

- Query expansion / HyPC for vector leg → **default:** none; evaluation (`014`) decides.
