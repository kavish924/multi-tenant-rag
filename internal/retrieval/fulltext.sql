-- Full-text leg (spec 007, RET-REQ-003). $1 tenant_id, $2 question text,
-- $3 candidates.
SELECT c.id, row_number() OVER (ORDER BY ts_rank_cd(c.tsv, q) DESC, c.id) AS rnk
FROM chunks c, websearch_to_tsquery('english', $2) AS q
WHERE c.tenant_id = $1
  AND c.tsv @@ q
  AND NOT EXISTS (SELECT 1 FROM documents d WHERE d.id = c.document_id AND d.deleted_at IS NOT NULL)
ORDER BY ts_rank_cd(c.tsv, q) DESC, c.id
LIMIT $3
