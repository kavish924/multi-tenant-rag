# Technology Stack

> Multi-Tenant RAG Platform in Go — technology choices, rationale, and swap points.

## Guiding principle

**Pick boring, well-understood tools first.** Every choice below can be swapped later behind
an interface. The stack is intentionally minimal: one database (PostgreSQL) covers relational
data, vectors, keyword search, and the job queue, so tenancy, transactions, and deletion stay
simple. Infrastructure (brokers, extra databases, service meshes) is added only when the
workload justifies it.

## Stack at a glance

| Concern          | Start with                                   | Why                                                              |
| ---------------- | -------------------------------------------- | ---------------------------------------------------------------- |
| HTTP             | `net/http` with the standard mux, or `chi`   | Small surface, easy to test, plenty of middleware.               |
| Database access  | `pgx` (optionally with `sqlc`)               | Native Postgres features: transactions, `set_config`, pgvector.  |
| Migrations       | `goose` or `golang-migrate`                  | Versioned, reviewable schema changes.                            |
| Vectors          | PostgreSQL with `pgvector`                   | One datastore keeps tenancy, transactions, and deletion simple.  |
| Keyword search   | Postgres `tsvector` + GIN index              | Covers exact terms, identifiers, and rare words embeddings miss. |
| Files            | S3-compatible storage (MinIO locally)        | Same API in development and production.                          |
| Jobs             | PostgreSQL-backed queue first                | Transactional enqueue, no extra infrastructure. Add Kafka, RabbitMQ, or NATS only when you can justify it. |
| LLM & embeddings | Your own provider interfaces                 | Swap hosted and local models without touching retrieval.         |
| Telemetry        | OpenTelemetry, Prometheus, Grafana           | Vendor-neutral traces and metrics.                               |

## Language and runtime

- **Go** — three binaries (`cmd/api`, `cmd/worker`, `cmd/outbox-relay`) built from one module,
  sharing `internal/` packages. Binaries scale and fail independently.
- **SQL** lives in versioned migration files (`migrations/*.sql`) and a few `.sql` files
  embedded in Go code (e.g. `internal/jobs/claim.sql`, `internal/retrieval/hybrid.sql`) so the
  hot queries are reviewable as SQL, not hidden behind an ORM.

## Provider abstractions (the swap points)

The application depends on interfaces, never on a particular vendor SDK:

```go
package llm

type EmbeddingProvider interface {
    Embed(
        ctx context.Context,
        texts []string,
    ) ([][]float32, error)
}

type GenerationRequest struct {
    SystemPrompt string
    Question     string
    Context      []string
}

type TokenUsage struct {
    InputTokens  int
    OutputTokens int
}

type Generator interface {
    Generate(
        ctx context.Context,
        req GenerationRequest,
    ) (<-chan string, <-chan error)
}
```

Design notes:

- This is an illustrative interface. In a real streaming API, prefer a **single stream
  abstraction** that represents both token events and terminal errors, with explicit
  cancellation and final usage reporting.
- Validate that every returned embedding has the **expected dimension**.
- Record which embedding model produced each vector (`chunks.embed_model`) so you can
  re-embed later when you switch models.
- Benefit: switch between a hosted model and a local model **without rewriting the retrieval
  pipeline**.

## Datastores

**PostgreSQL (+ pgvector) is the only stateful core.** It holds:

- Tenant, user, membership, document, chunk, job, outbox, and usage tables.
- Vector index (`hnsw (embedding vector_cosine_ops)`) for semantic search.
- Full-text index (`gin (tsv)`) for keyword search.
- The job queue (`SELECT ... FOR UPDATE SKIP LOCKED`) with leases and retries.
- The transactional outbox for event publishing.

**Object storage (MinIO locally, S3 in production)** holds original uploaded files under
tenant-prefixed keys: `tenants/{tenant_id}/documents/{document_id}`.

## Observability stack

- **OpenTelemetry** for traces and metrics (vendor neutral).
- **Prometheus** for scraping and alerting rules.
- **Grafana** for dashboards (latency histograms, queue depth/age, job failures, tokens and
  estimated cost per tenant).

See [system-design.md — Observability](system-design.md#7-observability) for what to capture
on each signal.

## Local development

Docker Compose brings up the full environment:

| Service         | Image / build            | Purpose                          |
| --------------- | ------------------------ | -------------------------------- |
| `api`           | built from `cmd/api`     | HTTP API + SSE                   |
| `worker`        | built from `cmd/worker`  | Ingestion pipeline               |
| `outbox-relay`  | built from `cmd/outbox-relay` | Publishes outbox rows (optional until a broker exists) |
| `postgres`      | `pgvector/pgvector`      | Database with vector extension   |
| `minio`         | `minio/minio`            | S3-compatible object storage     |
| `otel-collector`| `otel/opentelemetry-collector` | Trace/metric collection    |
| `prometheus`    | `prom/prometheus`        | Metrics scraping                 |
| `grafana`       | `grafana/grafana`        | Dashboards                       |

## What we deliberately do NOT run early

- Kafka / RabbitMQ / NATS (Postgres queue + outbox first)
- A dedicated vector database (pgvector first)
- Multiple regions, multiple vector databases, or a service mesh
- A polished front end (a minimal demo client is enough)
- Model training or fine-tuning

Each of these can be introduced later without re-architecting, because the boundaries
(database access, storage, queue, LLM providers) are already behind interfaces.

## Related docs

- [Architecture Diagrams](diagrams/architecture.md) — visual reference: context, containers, sequence paths, state machines, deployment, trust boundaries
- [System Architecture](system-architecture.md) — binaries, paths, deployment view
- [System Design](system-design.md) — data model, tenancy, pipeline, retrieval, guardrails
