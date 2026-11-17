-- Hybrid retrieval candidates (spec 007, docs/system-design.md §5).
--
-- $1 tenant_id, $2 question embedding (vector), $3 question text,
-- $4 candidates per leg (50).
--
-- Each leg ranks the tenant's chunks of live documents; the outer query
-- returns the union with both leg ranks, and Go fuses them with RRF and
-- applies the per-document cap (fuse.go), which keeps the math unit-tested.
-- Tenant scoping is explicit here (layer 1) and enforced again by forced RLS
-- (layer 2), since the query runs inside WithTenant.
WITH vec AS MATERIALIZED (
  -- MATERIALIZED + ORDER BY distance LIMIT keeps the HNSW index scan; with
  -- hnsw.iterative_scan = relaxed_order the scan continues past rows the
  -- tenant/deleted filters reject (RET-REQ-007), and the outer row_number
  -- restores exact distance order.
  SELECT c.id, c.embedding <=> $2::vector AS dist
  FROM chunks c
  WHERE c.tenant_id = $1
    AND NOT EXISTS (SELECT 1 FROM documents d WHERE d.id = c.document_id AND d.deleted_at IS NOT NULL)
  ORDER BY c.embedding <=> $2::vector
  LIMIT $4
),
vec_ranked AS (
  SELECT id, row_number() OVER (ORDER BY dist, id) AS rnk FROM vec
),
fts AS (
  SELECT c.id, row_number() OVER (ORDER BY ts_rank_cd(c.tsv, q) DESC, c.id) AS rnk
  FROM chunks c, websearch_to_tsquery('english', $3) AS q
  WHERE c.tenant_id = $1
    AND c.tsv @@ q
    AND NOT EXISTS (SELECT 1 FROM documents d WHERE d.id = c.document_id AND d.deleted_at IS NOT NULL)
  ORDER BY ts_rank_cd(c.tsv, q) DESC, c.id
  LIMIT $4
),
candidates AS (
  SELECT id FROM vec_ranked
  UNION
  SELECT id FROM fts
)
SELECT c.id, c.document_id, d.filename, c.content, coalesce(c.page, 0), coalesce(c.heading, ''),
       coalesce(v.rnk, 0), coalesce(f.rnk, 0)
FROM candidates k
JOIN chunks c ON c.id = k.id
JOIN documents d ON d.id = c.document_id AND d.tenant_id = c.tenant_id
LEFT JOIN vec_ranked v ON v.id = c.id
LEFT JOIN fts f ON f.id = c.id
WHERE c.tenant_id = $1
  AND d.tenant_id = $1
  AND d.deleted_at IS NULL -- set atomically with 'deleting'; also covers 'deleted' (RET-REQ-005)
