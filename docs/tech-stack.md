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
| Database         | Cloud SQL for PostgreSQL                     | Managed Postgres; private IP; IAM database auth.                 |
| Vectors          | PostgreSQL with `pgvector`                   | One datastore keeps tenancy, transactions, and deletion simple.  |
| Keyword search   | Postgres `tsvector` + GIN index              | Covers exact terms, identifiers, and rare words embeddings miss. |
| Files            | Cloud Storage (`cloud.google.com/go/storage`); `fake-gcs-server` in tests | Native client, V4 signed URLs, emulator for tests.  |
| Jobs             | PostgreSQL-backed queue first                | Transactional enqueue, no extra infrastructure. Add Pub/Sub only when you can justify it. |
| LLM & embeddings | Your own provider interfaces; Vertex AI implementations | Swap models without touching retrieval; service-account auth, no keys. |
| Runtime          | Cloud Run (service for `api`, worker pools for `worker`/`outbox-relay`, job for migrations) | Containers without cluster ops. |
| Telemetry        | OpenTelemetry → Cloud Trace, Managed Service for Prometheus, Cloud Monitoring | Vendor-neutral instrumentation, managed backends. |
| Dev & CI         | Cloud Shell, Cloud Build, Artifact Registry, Terraform | Nothing installed on the laptop ([gcp-environment.md](gcp-environment.md)). |

## Language and runtime

- **Go** — three binaries (`cmd/api`, `cmd/worker`, `cmd/outbox-relay`) built from one module,
  sharing `internal/` packages. Binaries scale and fail independently.
- **SQL** lives in versioned migration files (`migrations/*.sql`) and a few `.sql` files
  embedded in Go code (e.g. `internal/retrieval/hybrid.sql`; cross-tenant queries such as the job
  claim live in migrations as reviewed `SECURITY DEFINER` functions) so the
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

**Cloud Storage** (one bucket per environment; `fake-gcs-server` in tests) holds original
uploaded files under tenant-prefixed keys: `tenants/{tenant_id}/documents/{document_id}`.

**Cloud SQL caveat:** there is no true superuser, so nothing may depend on `BYPASSRLS`. The
cross-tenant system functions rely on policies scoped to the `app_system` role instead
([system-design.md §1](system-design.md#cross-tenant-system-paths)).

## Observability stack

- **OpenTelemetry** SDK in every binary (vendor neutral), exporting OTLP to a collector sidecar.
- **Cloud Trace** for traces; **Managed Service for Prometheus** for metrics (PromQL works).
- **Cloud Monitoring** dashboards and alert policies (latency histograms, queue depth/age, job
  failures, tokens and estimated cost per tenant), defined in Terraform.
- **Cloud Logging** for structured JSON logs correlated with traces.

See [system-design.md — Observability](system-design.md#7-observability) for what to capture
on each signal.

## Development environment

Nothing is installed on the laptop. Development happens in **Cloud Shell**; tests run
there and in **Cloud Build**. Full setup: [gcp-environment.md](gcp-environment.md).

In Cloud Shell, `deployments/compose.yaml` provides a disposable test stack:

| Service          | Image                     | Stands in for          |
| ---------------- | ------------------------- | ---------------------- |
| `postgres`       | `pgvector/pgvector` (same major version as Cloud SQL) | Cloud SQL |
| `gcs`            | `fsouza/fake-gcs-server`  | Cloud Storage          |
| `otel-collector` | `otel/opentelemetry-collector` | Collector sidecar (console exporter) |

The binaries themselves run with `go run` in Cloud Shell, or deployed to the dev project.

## What we deliberately do NOT run early

- Kafka / RabbitMQ / NATS (Postgres queue + outbox first)
- A dedicated vector database (pgvector first)
- Multiple regions, multiple vector databases, or a service mesh
- A polished front end (a minimal demo client is enough)
- Model training or fine-tuning

Each of these can be introduced later without re-architecting, because the boundaries
(database access, storage, queue, LLM providers) are already behind interfaces.

## Related docs

- [GCP Environment](gcp-environment.md) — Cloud Shell, CI/CD, Cloud Run, Cloud SQL roles, free-trial costs
- [Architecture Diagrams](diagrams/architecture.md) — visual reference: context, containers, sequence paths, state machines, deployment, trust boundaries
- [System Architecture](system-architecture.md) — binaries, paths, deployment view
- [System Design](system-design.md) — data model, tenancy, pipeline, retrieval, guardrails
