# Multi-Tenant RAG Platform in Go

A production-grade, asynchronous, observable Retrieval-Augmented Generation (RAG) platform that
serves many tenants (companies, teams, customers) without leaking one tenant's data into
another's answers.

The hard parts are not the LLM call — they are **isolation, durable background processing,
retrieval quality, cost control, and being able to explain what the system did on any request**.

## Documentation

| Doc | Contents |
| --- | -------- |
| [docs/tech-stack.md](docs/tech-stack.md) | Technology choices, rationale, swap points |
| [docs/system-architecture.md](docs/system-architecture.md) | Binaries, write/read paths, deployment view |
| [docs/system-design.md](docs/system-design.md) | Tenancy (RLS), data model, pipeline, retrieval, guardrails |
| [docs/diagrams/architecture.md](docs/diagrams/architecture.md) | Mermaid diagrams: context, containers, sequences, states, trust boundaries |
| [Multi-Tenant RAG Platform in Go.pdf](Multi-Tenant%20RAG%20Platform%20in%20Go.pdf) | Source guide this project is built from |

## Specifications (spec-driven development)

Feature specs live in [`specs/`](specs/README.md) — one triad (requirements / design / tasks) per
feature, ordered by build milestone:

1. **Working Go API** — tenancy & identity, document management
2. **Async ingestion** — object storage, job queue, ingestion pipeline, outbox messaging
3. **RAG end to end** — hybrid retrieval (RRF), generation/streaming/citations, reranking
4. **Observable production** — observability, quotas & cost control
5. **Release-quality capstone** — guardrails, deletion & cleanup, evaluation, isolation tests & red teaming

Workflow: requirements → design → tasks → implement → verified by tests. No production code
before its spec is approved.

## Architecture at a glance

- **`cmd/api`** — HTTP API: auth, tenant resolution, document CRUD, search/ask, SSE streaming
- **`cmd/worker`** — job-driven ingestion: extract → chunk → embed → index; deletion cleanup
- **`cmd/outbox-relay`** — transactional outbox publisher (optional until a broker is justified)

One PostgreSQL (+ pgvector) for relational data, vectors, keyword search, and the job queue.
Tenant isolation defended in three layers: application authorization, row-level security,
tenant-prefixed object storage. Guardrails fail closed; the model is not a security boundary.

## Status

Planning/documentation phase — specs are `Draft`. Implementation follows
[`specs/README.md`](specs/README.md) milestone order, starting with
[spec 001 — Tenancy & Identity](specs/001-tenancy-identity/).
