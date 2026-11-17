-- Spec 011 · QTA-REQ-001/002/008: per-tenant quota overrides, fixed-window
-- usage counters, ask concurrency slots, and the per-tenant job cap in
-- claim_job.

-- +goose Up
-- Overrides only: a NULL column (or no row) means the platform default.
-- Written by the operator tool (cmd/quotactl), read by the app.
CREATE TABLE tenant_quotas (
  tenant_id             uuid        PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  uploads_day           int         CHECK (uploads_day >= 0),
  questions_day         int         CHECK (questions_day >= 0),
  embed_tokens_month    bigint      CHECK (embed_tokens_month >= 0),
  generate_tokens_month bigint      CHECK (generate_tokens_month >= 0),
  concurrent_asks       int         CHECK (concurrent_asks >= 0),
  concurrent_jobs       int         CHECK (concurrent_jobs >= 0),
  updated_at            timestamptz NOT NULL DEFAULT now()
);

-- Fixed windows (UTC day or month), keyed by window start.
CREATE TABLE tenant_usage_counters (
  tenant_id    uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  dimension    text        NOT NULL CHECK (dimension IN ('uploads', 'questions', 'embed_tokens', 'generate_tokens')),
  window_start timestamptz NOT NULL,
  used         bigint      NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, dimension, window_start)
);

-- One row per in-flight ask stream. Rows expire (a crashed instance cannot
-- leak a slot for longer than the stream timeout).
CREATE TABLE ask_slots (
  id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL
);
CREATE INDEX ask_slots_tenant ON ask_slots (tenant_id, expires_at);

GRANT SELECT ON tenant_quotas TO app;
GRANT SELECT, INSERT, UPDATE ON tenant_usage_counters TO app;
GRANT SELECT, INSERT, DELETE ON ask_slots TO app;

ALTER TABLE tenant_quotas ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_quotas FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_quotas
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE tenant_usage_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_usage_counters FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_usage_counters
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE ask_slots ENABLE ROW LEVEL SECURITY;
ALTER TABLE ask_slots FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ask_slots
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- claim_job reads each tenant's job cap.
GRANT SELECT ON tenant_quotas TO app_system;
CREATE POLICY system_access ON tenant_quotas TO app_system USING (true);

CREATE INDEX jobs_running_tenant ON jobs (tenant_id) WHERE status = 'running';

-- claim_job gains p_tenant_cap: a job is claimable only while its tenant has
-- fewer live running jobs than its cap (tenant_quotas.concurrent_jobs, else
-- p_tenant_cap). Concurrent claimers can each pass the count before either
-- commits, so the cap may be exceeded by at most (workers − 1) briefly; the
-- design documents this tolerance.
DROP FUNCTION claim_job(text, interval, text[]);

-- +goose StatementBegin
CREATE FUNCTION claim_job(p_worker text, p_lease interval, p_kinds text[], p_tenant_cap int)
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
      AND (SELECT count(*) FROM public.jobs r
           WHERE r.tenant_id = c.tenant_id AND r.status = 'running' AND r.lease_expires_at >= now())
          < coalesce((SELECT q.concurrent_jobs FROM public.tenant_quotas q WHERE q.tenant_id = c.tenant_id),
                     greatest(p_tenant_cap, 1))
    ORDER BY c.run_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
  )
  RETURNING j.id, j.tenant_id, j.kind, j.payload, j.attempts, j.max_attempts;
$$;
-- +goose StatementEnd

ALTER FUNCTION claim_job(text, interval, text[], int) OWNER TO app_system;
REVOKE ALL ON FUNCTION claim_job(text, interval, text[], int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_job(text, interval, text[], int) TO app;

-- +goose Down
DROP FUNCTION claim_job(text, interval, text[], int);
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
DROP INDEX jobs_running_tenant;
DROP POLICY system_access ON tenant_quotas;
DROP TABLE ask_slots;
DROP TABLE tenant_usage_counters;
DROP TABLE tenant_quotas;
