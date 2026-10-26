# Feature Specifications — Spec-Driven Development

Specs are the source of truth for **what** gets built and **how it is verified**. Shared
context lives in [`docs/`](../docs/); each spec contains only feature-specific requirements,
design deltas, and tasks.

## Workflow

1. **Requirements** — user stories + EARS acceptance criteria. Review before anything else.
2. **Design** — technical solution for the approved requirements. Review before tasks.
3. **Tasks** — implementation checklist; every task cites the requirement IDs it satisfies.
4. **Implement** — task-by-task. An acceptance criterion is "done" only when a test verifies it.
5. **Status** — `Draft → Approved → In progress → Complete` (update the index below).

Rules:

- No production code for a feature before its spec reaches *Approved*.
- Every acceptance criterion maps to at least one test (unit, integration, security, or eval).
- Cross-cutting invariants (tenancy, observability, guardrails) are specified once — specs
  reference them, they do not restate them.

## Spec template

Copy [`_template/`](./_template/) when creating a new spec directory.

## Index & status

| Spec | Feature | Milestone | Status |
| ---- | ------- | --------- | ------ |
| [001](./001-tenancy-identity/) | Tenancy & identity | 1 — Working Go API | Draft |
| [002](./002-document-management/) | Document management | 1 — Working Go API | Draft |
| [003](./003-object-storage/) | Object storage | 2 — Async ingestion | Draft |
| [004](./004-job-queue/) | Job queue (PostgreSQL) | 2 — Async ingestion | Draft |
| [005](./005-ingestion-pipeline/) | Ingestion pipeline | 2 — Async ingestion | Draft |
| [006](./006-outbox-messaging/) | Outbox messaging | 2 — Async ingestion | Draft |
| [007](./007-hybrid-retrieval/) | Hybrid retrieval (RRF) | 3 — RAG end to end | Draft |
| [008](./008-generation-citations/) | Generation, streaming & citations | 3 — RAG end to end | Draft |
| [009](./009-reranking/) | Reranking (optional stage) | 3 — RAG end to end | Draft |
| [010](./010-observability/) | Observability | 4 — Observable production | Draft |
| [011](./011-quotas-cost-control/) | Quotas & cost control | 4 — Observable production | Draft |
| [012](./012-guardrails/) | Guardrails (fail-closed pipeline) | 5 — Release-quality capstone | Draft |
| [013](./013-deletion-cleanup/) | Deletion & cleanup | 5 — Release-quality capstone | Draft |
| [014](./014-evaluation/) | Evaluation harness | 5 — Release-quality capstone | Draft |
| [015](./015-isolation-redteam/) | Isolation tests & red teaming | 5 — Release-quality capstone | Draft |

## Milestone exit criteria (from the platform guide)

| Milestone | Exit criterion |
| --------- | -------------- |
| 1 — Working Go API | Tenant-scoped API tests pass. |
| 2 — Async ingestion | Restarting a worker does not corrupt document state. |
| 3 — RAG end to end | A test dataset demonstrates measurable retrieval and answer quality. |
| 4 — Observable production | You can explain the latency and failure of a request using its telemetry. |
| 5 — Release-quality capstone | Another developer can run the project using the documented setup. |

## Requirement ID prefixes

| Spec | Prefix | Spec | Prefix |
| ---- | ------ | ---- | ------ |
| 001 | `TEN` | 009 | `RER` |
| 002 | `DOC` | 010 | `OBS` |
| 003 | `STO` | 011 | `QTA` |
| 004 | `JOB` | 012 | `GRD` |
| 005 | `ING` | 013 | `DEL` |
| 006 | `OBX` | 014 | `EVA` |
| 007 | `RET` | 015 | `RT` |
| 008 | `GEN` |      |        |
