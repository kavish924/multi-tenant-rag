-- Spec 001 · TEN-REQ-010, TEN-REQ-011: the `app_system` role and the first
-- cross-tenant system function.
--
-- Rules for every system function (see docs/system-design.md §1):
--   * SECURITY DEFINER, owned by app_system (NOLOGIN, no BYPASSRLS).
--   * SET search_path = pg_catalog, public, pg_temp. pg_temp must be listed
--     last: when it is omitted Postgres searches the caller's temp schema
--     first, so a caller could shadow `memberships` with a temp table and
--     forge its own memberships.
--   * Table names schema-qualified anyway.
--   * EXECUTE revoked from PUBLIC, granted to app only.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_system') THEN
    CREATE ROLE app_system NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
  END IF;
END
$$;
-- +goose StatementEnd

-- The migrating role (`migrator` on Cloud SQL) must be a member of app_system
-- to hand function ownership to it. `app` is never a member. On Cloud SQL the
-- membership already exists (scripts/cloudsql-bootstrap.sql); superusers are
-- implicitly members. Grant only when missing, so a re-grant never fails.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT pg_has_role(current_user, 'app_system', 'MEMBER') THEN
    EXECUTE format('GRANT app_system TO %I', current_user);
  END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO app_system;
GRANT SELECT ON memberships TO app_system;
CREATE POLICY system_access ON memberships TO app_system USING (true);

-- +goose StatementBegin
CREATE FUNCTION memberships_for_user(p_user uuid)
RETURNS TABLE (tenant_id uuid, role text)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  SELECT m.tenant_id, m.role
  FROM public.memberships m
  WHERE m.user_id = p_user
  ORDER BY m.tenant_id;
$$;
-- +goose StatementEnd

ALTER FUNCTION memberships_for_user(uuid) OWNER TO app_system;
REVOKE ALL ON FUNCTION memberships_for_user(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION memberships_for_user(uuid) TO app;

-- +goose Down
DROP FUNCTION memberships_for_user(uuid);
DROP POLICY system_access ON memberships;
REVOKE SELECT ON memberships FROM app_system;
REVOKE USAGE ON SCHEMA public FROM app_system;
