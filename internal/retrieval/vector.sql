-- Vector leg (spec 007, RET-REQ-002). $1 tenant_id, $2 question embedding,
-- $3 candidates. MATERIALIZED + ORDER BY distance LIMIT keeps the HNSW index
-- scan; with hnsw.iterative_scan = relaxed_order the scan continues past rows
-- the tenant/deleted filters reject (RET-REQ-007), and row_number restores
-- exact distance order. Tenant scope is explicit (layer 1) and enforced again
-- by forced RLS (layer 2).
WITH vec AS MATERIALIZED (
  SELECT c.id, c.embedding <=> $2::vector AS dist
  FROM chunks c
  WHERE c.tenant_id = $1
    AND NOT EXISTS (SELECT 1 FROM documents d WHERE d.id = c.document_id AND d.deleted_at IS NOT NULL)
  ORDER BY c.embedding <=> $2::vector
  LIMIT $3
)
SELECT id, row_number() OVER (ORDER BY dist, id) AS rnk FROM vec
