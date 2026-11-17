-- Spec 010 · OBS-REQ-004: queue depth and age for the jobs_queue_depth and
-- jobs_queue_age_seconds gauges. Cross-tenant, so a system function
-- (TEN-REQ-010); it returns counts per kind only, never ids or payloads.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION job_queue_stats()
RETURNS TABLE (kind text, depth bigint, oldest_age_seconds double precision)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  SELECT j.kind,
         count(*),
         coalesce(extract(epoch FROM now() - min(j.run_at) FILTER (WHERE j.status = 'queued' AND j.run_at <= now())), 0)::double precision
  FROM public.jobs j
  WHERE j.status IN ('queued', 'running')
  GROUP BY j.kind
  ORDER BY j.kind;
$$;
-- +goose StatementEnd

ALTER FUNCTION job_queue_stats() OWNER TO app_system;
REVOKE ALL ON FUNCTION job_queue_stats() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION job_queue_stats() TO app;

-- +goose Down
DROP FUNCTION job_queue_stats();
