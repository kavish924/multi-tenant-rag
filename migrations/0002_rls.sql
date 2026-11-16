-- Spec 001 · TEN-REQ-008: forced row-level security and the dedicated `app` role.
--
-- The policy uses NULLIF(..., '') because once a pooled connection has run a
-- transaction with set_config('app.tenant_id', ..., true), the setting reverts
-- to '' (not NULL) afterwards, and ''::uuid would raise an error instead of
-- matching zero rows.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app') THEN
    CREATE ROLE app NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
  END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO app;
GRANT SELECT, INSERT                 ON users       TO app;
GRANT SELECT, INSERT                 ON tenants     TO app;
GRANT SELECT, INSERT, UPDATE, DELETE ON memberships TO app;

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenants
  USING      (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON memberships
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down
DROP POLICY tenant_isolation ON memberships;
ALTER TABLE memberships NO FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships DISABLE ROW LEVEL SECURITY;
DROP POLICY tenant_isolation ON tenants;
ALTER TABLE tenants NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON memberships, tenants, users FROM app;
REVOKE USAGE ON SCHEMA public FROM app;
