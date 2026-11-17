-- Spec 002 · DOC-REQ-004/008: document metadata.

-- +goose Up
CREATE TABLE documents (
  id           uuid        PRIMARY KEY,
  tenant_id    uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  filename     text        NOT NULL CHECK (length(filename) BETWEEN 1 AND 255),
  content_type text        NOT NULL,
  size_bytes   bigint      NOT NULL CHECK (size_bytes >= 0),
  object_key   text        NOT NULL,
  sha256       text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  status       text        NOT NULL DEFAULT 'uploaded'
               CHECK (status IN ('uploaded', 'queued', 'processing', 'indexed', 'failed', 'deleting', 'deleted')),
  error        text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  deleted_at   timestamptz,
  -- Isolation layer 3 enforced in the schema too: the key always names the
  -- row's own tenant and id (STO-REQ-002).
  CHECK (object_key = 'tenants/' || tenant_id::text || '/documents/' || id::text),
  CHECK (deleted_at IS NULL OR status IN ('deleting', 'deleted'))
);

-- Per-tenant dedup among live documents; a deleted file may be re-uploaded.
CREATE UNIQUE INDEX documents_tenant_sha256 ON documents (tenant_id, sha256) WHERE deleted_at IS NULL;
CREATE INDEX documents_tenant_created ON documents (tenant_id, created_at DESC, id DESC);

-- deleted_at is sticky: once set, no later write may clear it or move the
-- status out of deleting/deleted (system-design §3).
-- +goose StatementBegin
CREATE FUNCTION documents_deleted_is_sticky() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF OLD.deleted_at IS NOT NULL AND (NEW.deleted_at IS NULL OR NEW.deleted_at <> OLD.deleted_at) THEN
    RAISE EXCEPTION 'documents.deleted_at is sticky (document %)', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER documents_deleted_is_sticky
  BEFORE UPDATE ON documents
  FOR EACH ROW EXECUTE FUNCTION documents_deleted_is_sticky();

-- No DELETE for app: rows are soft-deleted; the retention sweeper (013)
-- hard-deletes through a system function.
GRANT SELECT, INSERT, UPDATE ON documents TO app;

ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON documents
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down
DROP TABLE documents;
DROP FUNCTION documents_deleted_is_sticky();
