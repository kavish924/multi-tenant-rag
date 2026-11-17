-- Spec 008 · GEN-REQ-007: token usage per tenant, the source of truth for
-- quotas and cost views (011) and token metrics (010).

-- +goose Up
CREATE TABLE usage_events (
  id            bigserial   PRIMARY KEY,
  tenant_id     uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  kind          text        NOT NULL CHECK (kind IN ('embed', 'generate', 'rerank')),
  model         text        NOT NULL CHECK (length(model) BETWEEN 1 AND 200),
  input_tokens  int         NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  output_tokens int         NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  -- How the call ended: ok, error or cancelled (client disconnect).
  outcome       text        NOT NULL DEFAULT 'ok' CHECK (outcome IN ('ok', 'error', 'cancelled')),
  request_id    text        CHECK (length(request_id) <= 100),
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX usage_events_tenant_created ON usage_events (tenant_id, created_at);

GRANT SELECT, INSERT ON usage_events TO app;
GRANT USAGE ON SEQUENCE usage_events_id_seq TO app;

ALTER TABLE usage_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_events FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON usage_events
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down
DROP TABLE usage_events;
