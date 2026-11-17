-- Spec 006 · OBX-REQ-001/003/006: the transactional outbox, the consumer
-- dedupe table, and the relay's system functions.

-- +goose Up
CREATE TABLE outbox (
  id             bigserial   PRIMARY KEY,
  tenant_id      uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  topic          text        NOT NULL CHECK (topic ~ '^[a-z]+(\.[a-z_]+)+$' AND length(topic) <= 100),
  payload        jsonb       NOT NULL DEFAULT '{}'::jsonb,
  created_at     timestamptz NOT NULL DEFAULT now(),
  published_at   timestamptz,
  -- Publish attempts the publisher rejected as permanent (design §7). A row
  -- that reaches the relay's limit is quarantined and no longer relayed.
  attempts       int         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  last_error     text,
  quarantined_at timestamptz,
  CHECK (published_at IS NULL OR quarantined_at IS NULL)
);

CREATE INDEX outbox_pending ON outbox (id) WHERE published_at IS NULL AND quarantined_at IS NULL;
CREATE INDEX outbox_tenant ON outbox (tenant_id, id);

-- Producers only insert (and read back their own tenant's rows); the relay
-- is the only writer of published_at, through the functions below.
GRANT SELECT, INSERT ON outbox TO app;
GRANT USAGE ON SEQUENCE outbox_id_seq TO app;

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, UPDATE ON outbox TO app_system;
CREATE POLICY system_access ON outbox TO app_system USING (true) WITH CHECK (true);

-- Idempotent consumers (OBX-REQ-006): a consumer records the event id in the
-- same tenant transaction as its side effect, so a redelivered event finds
-- the row and does nothing (OBX-AC-003).
CREATE TABLE consumed_events (
  consumer    text        NOT NULL CHECK (length(consumer) BETWEEN 1 AND 100),
  event_id    bigint      NOT NULL,
  tenant_id   uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  consumed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);

GRANT SELECT, INSERT ON consumed_events TO app;

ALTER TABLE consumed_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE consumed_events FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON consumed_events
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Oldest pending rows across all tenants, in id order (OBX-REQ-003).
-- +goose StatementBegin
CREATE FUNCTION outbox_next(p_limit int)
RETURNS TABLE (id bigint, tenant_id uuid, topic text, payload jsonb, created_at timestamptz, attempts int)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  SELECT o.id, o.tenant_id, o.topic, o.payload, o.created_at, o.attempts
  FROM public.outbox o
  WHERE o.published_at IS NULL AND o.quarantined_at IS NULL
  ORDER BY o.id
  LIMIT least(greatest(p_limit, 1), 1000);
$$;
-- +goose StatementEnd

-- Guarded ack: true only for the call that actually marked the row.
-- +goose StatementBegin
CREATE FUNCTION outbox_mark_published(p_id bigint)
RETURNS boolean
LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  WITH marked AS (
    UPDATE public.outbox SET published_at = now()
    WHERE id = p_id AND published_at IS NULL AND quarantined_at IS NULL
    RETURNING 1
  )
  SELECT EXISTS (SELECT 1 FROM marked);
$$;
-- +goose StatementEnd

-- Records a permanent publish failure; returns true when this failure
-- quarantined the row (attempts reached p_max_attempts).
-- +goose StatementBegin
CREATE FUNCTION outbox_mark_failed(p_id bigint, p_error text, p_max_attempts int)
RETURNS boolean
LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
  WITH failed AS (
    UPDATE public.outbox
    SET attempts = attempts + 1,
        last_error = left(p_error, 2000),
        quarantined_at = CASE WHEN attempts + 1 >= greatest(p_max_attempts, 1) THEN now() END
    WHERE id = p_id AND published_at IS NULL AND quarantined_at IS NULL
    RETURNING quarantined_at IS NOT NULL AS quarantined
  )
  SELECT coalesce((SELECT quarantined FROM failed), false);
$$;
-- +goose StatementEnd

ALTER FUNCTION outbox_next(int) OWNER TO app_system;
ALTER FUNCTION outbox_mark_published(bigint) OWNER TO app_system;
ALTER FUNCTION outbox_mark_failed(bigint, text, int) OWNER TO app_system;
REVOKE ALL ON FUNCTION outbox_next(int) FROM PUBLIC;
REVOKE ALL ON FUNCTION outbox_mark_published(bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION outbox_mark_failed(bigint, text, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION outbox_next(int) TO app;
GRANT EXECUTE ON FUNCTION outbox_mark_published(bigint) TO app;
GRANT EXECUTE ON FUNCTION outbox_mark_failed(bigint, text, int) TO app;

-- +goose Down
DROP FUNCTION outbox_mark_failed(bigint, text, int);
DROP FUNCTION outbox_mark_published(bigint);
DROP FUNCTION outbox_next(int);
DROP TABLE consumed_events;
DROP TABLE outbox;
