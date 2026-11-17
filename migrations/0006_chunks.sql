-- Spec 005 · ING-REQ-003/004/006/008: chunks with vectors and full text.
--
-- Embedding dimension: 768 (internal/llm.EmbeddingDim). Chosen so the
-- Vertex AI model can be text-embedding-005 (768 native) or
-- gemini-embedding-001 with output_dimensionality = 768, and stays well
-- below pgvector's 2,000-dimension HNSW limit for `vector`. Changing it
-- needs a new migration plus a re-embed of every chunk (embed_model).
--
-- On Cloud SQL the extension is created once by scripts/cloudsql-bootstrap.sql
-- (the migrator cannot create it); IF NOT EXISTS makes this a no-op there.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

-- Lets chunks reference (tenant_id, document_id) so a chunk can never point
-- at another tenant's document.
ALTER TABLE documents ADD CONSTRAINT documents_tenant_id_id_key UNIQUE (tenant_id, id);

CREATE TABLE chunks (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid        NOT NULL,
  document_id uuid        NOT NULL,
  ordinal     int         NOT NULL CHECK (ordinal >= 0),
  content     text        NOT NULL CHECK (length(content) > 0),
  token_count int         NOT NULL CHECK (token_count > 0),
  page        int         CHECK (page IS NULL OR page >= 1),
  heading     text,
  embed_model text        NOT NULL,
  embedding   vector(768) NOT NULL,
  tsv         tsvector    GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  -- Retries upsert the same rows (ING-REQ-004).
  UNIQUE (document_id, ordinal),
  FOREIGN KEY (tenant_id, document_id) REFERENCES documents (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX chunks_embedding_hnsw ON chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX chunks_tsv ON chunks USING gin (tsv);
CREATE INDEX chunks_tenant_document ON chunks (tenant_id, document_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON chunks TO app;

ALTER TABLE chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE chunks FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chunks
  USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down
DROP TABLE chunks;
ALTER TABLE documents DROP CONSTRAINT documents_tenant_id_id_key;
