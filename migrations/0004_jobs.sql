-- Spec 004 · JOB-REQ-001/002/007/011: the Postgres job queue and the
-- claim_job system function.

-- +goose Up
CREATE TABLE jobs (
  id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  kind             text        NOT NULL CHECK (length(kind) BETWEEN 1 AND 64),
  payload          jsonb       NOT NULL DEFAULT '{}'::jsonb,
  status           text        NOT NULL DEFAULT 'queued'
                               CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'dead')),
  attempts         int         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  max_attempts     int         NOT NULL DEFAULT 5 CHECK (max_attempts >= 1),
  run_at           timestamptz NOT NULL DEFAULT now(),
  lease_expires_at timestamptz,
  locked_by        text,
  idempotency_key  text        CHECK (length(idempotency_key) BETWEEN 1 AND 200),
  last_error       text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  CHECK (status <> 'running' OR (locked_by IS NOT NULL AND lease_expires_at IS NOT NULL))
);

CREATE UNIQUE INDEX jobs_tenant_idempotency_key
  ON jobs (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX jobs_status_run_at ON jobs (status, run_at);
CREATE INDEX jobs_running_lease ON jobs (lease_expires_at) WHERE status = 'running';

GRANT SELECT, INSERT, UPDATE ON jobs TO app;

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON jobs
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- claim_job scans every tenant, so it runs as app_system through this policy.
GRANT SELECT, UPDATE ON jobs TO app_system;
CREATE POLICY system_access ON jobs TO app_system USING (true) WITH CHECK (true);

-- One statement: (1) dead-letter jobs whose lease expired on their final
-- attempt (a job that kills its worker never reaches Fail), then (2) claim
-- one runnable job of the requested kinds. The two predicates are disjoint
-- (attempts >= max_attempts vs attempts < max_attempts).
-- +goose StatementBegin
CREATE FUNCTION claim_job(p_worker text, p_lease interval, p_kinds text[])
RETURNS TABLE (id uuid, tenant_id uuid, kind text, payload jsonb, attempts int, max_attempts int)
LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  WITH reaped AS (
    UPDATE public.jobs
    SET status = 'dead', locked_by = NULL, lease_expires_at = NULL, updated_at = now(),
        last_error = coalesce(last_error, 'lease expired on final attempt')
    WHERE status = 'running' AND lease_expires_at < now() AND attempts >= max_attempts
  )
  UPDATE public.jobs j
  SET status = 'running', locked_by = p_worker, lease_expires_at = now() + p_lease,
      attempts = j.attempts + 1, updated_at = now()
  WHERE j.id = (
    SELECT c.id FROM public.jobs c
    WHERE c.kind = ANY (p_kinds)
      AND ((c.status = 'queued' AND c.run_at <= now())
        OR (c.status = 'running' AND c.lease_expires_at < now() AND c.attempts < c.max_attempts))
    ORDER BY c.run_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
  )
  RETURNING j.id, j.tenant_id, j.kind, j.payload, j.attempts, j.max_attempts;
$$;
-- +goose StatementEnd

ALTER FUNCTION claim_job(text, interval, text[]) OWNER TO app_system;
REVOKE ALL ON FUNCTION claim_job(text, interval, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_job(text, interval, text[]) TO app;

-- +goose Down
DROP FUNCTION claim_job(text, interval, text[]);
DROP TABLE jobs;
