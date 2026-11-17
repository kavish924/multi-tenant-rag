-- Candidate details for fusion. $1 tenant_id, $2 candidate chunk ids.
-- The documents join is the final deleted filter (RET-REQ-005): deleted_at
-- is set atomically with 'deleting' and also covers 'deleted'.
SELECT c.id, c.document_id, d.filename, c.content, coalesce(c.page, 0), coalesce(c.heading, '')
FROM chunks c
JOIN documents d ON d.id = c.document_id AND d.tenant_id = c.tenant_id
WHERE c.tenant_id = $1
  AND d.tenant_id = $1
  AND c.id = ANY ($2::uuid[])
  AND d.deleted_at IS NULL
