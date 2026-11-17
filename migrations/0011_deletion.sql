-- Spec 013 · DEL-REQ-005/007: tenant purge grants and the retention
-- sweeper's system functions.

-- +goose Up
-- Purge (DEL-REQ-005) removes a tenant's usage rows inside that tenant's
-- own transaction; RLS still limits it to the tenant.
GRANT DELETE ON usage_events TO app;
GRANT DELETE ON tenant_usage_counters TO app;

-- The app role still cannot DELETE documents: rows are soft-deleted, and
-- only the sweeper hard-deletes them, through sweep_delete_document, and
-- only once they are 'deleted' (cleaned) and past retention.
GRANT SELECT, DELETE ON documents TO app_system;
CREATE POLICY system_access ON documents TO app_system USING (true);

-- Documents whose cleanup finished more than p_retention ago, oldest first.
-- +goose StatementBegin
CREATE FUNCTION sweep_candidates(p_retention interval, p_limit int)
RETURNS TABLE (tenant_id uuid, document_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  SELECT d.tenant_id, d.id
  FROM public.documents d
  WHERE d.status = 'deleted' AND d.deleted_at < now() - p_retention
  ORDER BY d.deleted_at
  LIMIT least(greatest(p_limit, 1), 1000);
$$;
-- +goose StatementEnd

-- Hard-deletes one expired document row (chunks cascade; there are none
-- left after cleanup). The guard in the WHERE clause means a caller can
-- never remove a live, deleting or recently deleted document.
-- +goose StatementBegin
CREATE FUNCTION sweep_delete_document(p_document uuid, p_retention interval)
RETURNS boolean
LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  WITH gone AS (
    DELETE FROM public.documents d
    WHERE d.id = p_document AND d.status = 'deleted' AND d.deleted_at < now() - p_retention
    RETURNING 1
  )
  SELECT EXISTS (SELECT 1 FROM gone);
$$;
-- +goose StatementEnd

CREATE INDEX documents_deleted_sweep ON documents (deleted_at) WHERE status = 'deleted';

ALTER FUNCTION sweep_candidates(interval, int) OWNER TO app_system;
ALTER FUNCTION sweep_delete_document(uuid, interval) OWNER TO app_system;
REVOKE ALL ON FUNCTION sweep_candidates(interval, int) FROM PUBLIC;
REVOKE ALL ON FUNCTION sweep_delete_document(uuid, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION sweep_candidates(interval, int) TO app;
GRANT EXECUTE ON FUNCTION sweep_delete_document(uuid, interval) TO app;

-- +goose Down
DROP INDEX documents_deleted_sweep;
DROP FUNCTION sweep_delete_document(uuid, interval);
DROP FUNCTION sweep_candidates(interval, int);
DROP POLICY system_access ON documents;
REVOKE SELECT, DELETE ON documents FROM app_system;
REVOKE DELETE ON tenant_usage_counters FROM app;
REVOKE DELETE ON usage_events FROM app;
