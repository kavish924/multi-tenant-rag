# Implementation Checklist

> The ordered path from an empty repo to a release-quality platform on Google Cloud. Tick items as
> they land. Each feature item points at its spec's `tasks.md`, which holds the task-level detail
> and requirement IDs. This file is the order of work and its gates, not a copy of every task.

**Rules that apply to every item** (from [specs/README.md](../specs/README.md)):

- No production code for a feature before its spec is **Approved**; update the status in
  [specs/README.md](../specs/README.md) as each one moves `Draft → Approved → In progress → Complete`.
- An acceptance criterion is done only when a test verifies it.
- A milestone is done only when its **exit gate** passes.

---

## Phase 0 — Decisions before any code

- [ ] GCP region for all environments (Cloud SQL, Cloud Storage, Cloud Run, Vertex AI together)
- [ ] Project naming prefix → `<prefix>-dev`, `<prefix>-test`, `<prefix>-prod`
- [x] **Vertex AI embedding model** chosen → `EMBEDDING_DIM` fixed; `vector` (≤ 2,000 dims) vs
      `halfvec` (≤ 4,000) decided → 768 dims, `vector(768)` (`text-embedding-005` or `gemini-embedding-001` at 768) ([005 design §10](../specs/005-ingestion-pipeline/design.md))
- [ ] Vertex AI generation model chosen
- [x] Go version pinned; HTTP router (`net/http` mux or `chi`); `sqlc` yes/no → Go 1.27 toolchain (`go 1.26.0` in go.mod, set by dependencies), standard `net/http` mux, no `sqlc` (hand-written pgx)
- [x] Migration tool (`goose` or `golang-migrate`) → `goose` v3, migrations embedded and run by `cmd/migrate`
- [x] JWT algorithm (HS256 or EdDSA) — key lives in Secret Manager → HS256, `JWT_SIGNING_KEY` ≥ 32 bytes
- [x] Tokenizer library for chunking → built-in approximate tokenizer (`chunk.CountTokens`) ([005 design §10](../specs/005-ingestion-pipeline/design.md))
- [ ] Specs **001** and **002** reviewed and marked **Approved**

## Phase 1 — GCP foundation

Reference: [gcp-environment.md](gcp-environment.md) §2–§4, §10 (free-trial cost plan) and §11.

- [ ] Dev project created, billing linked, **budget + alerts** set
- [ ] Terraform state bucket; `deployments/terraform/bootstrap` applied
- [ ] Artifact Registry repository *(Terraform written: `bootstrap`)*
- [ ] Cloud Shell opens (or devbox VM via `scripts/devbox-setup.sh`); repo cloned; `make tidy` → `go.sum` committed
- [ ] Cloud Build trigger on push / pull request *(Terraform written: `bootstrap`)*
- [ ] **Spike:** one testcontainers Postgres test passes in Cloud Build (pin the networking
      setup that works) *(replaced: Cloud Build runs a named Postgres container + `TEST_DATABASE_URL`)*
- [ ] Check Cloud SQL's available `pgvector` version (≥ 0.8 needed for iterative scans,
      [007](../specs/007-hybrid-retrieval/requirements.md) RET-REQ-007)

## Phase 2 — Repository skeleton

- [x] `go.mod`; `cmd/api`, `cmd/worker`, `cmd/outbox-relay`, `cmd/migrate` with `main.go` stubs
- [ ] `internal/` package layout per [system-architecture.md](system-architecture.md#internal-package-layout)
- [x] Config loading from env; health endpoint on `api`
- [x] `Makefile`: `up`, `down`, `migrate`, `test`, `itest`, `lint`, `run-api`, `run-worker`
- [ ] `deployments/compose.yaml` (Cloud Shell dev stack): `pgvector/pgvector`, `fake-gcs-server`,
      `otel-collector` *(postgres and otel-collector done; fake-gcs-server lands with the Cloud Storage store)*
- [x] Dockerfiles (minimal base image) for each binary
- [x] `cloudbuild.yaml`: vet → lint → `go test` → `go test -race` → integration → build → push
- [x] Linter config; `migrations/` directory wired to the chosen tool
- [ ] *(written, not applied)* `deployments/terraform/env/dev`: VPC, Cloud SQL (private IP, IAM auth), documents bucket,
      service accounts (`sa-api`, `sa-worker`, `sa-relay`, `sa-migrate`, `sa-build`), Secret
      Manager JWT key, Cloud Run `api` service, `migrate` job
- [ ] *(written, not run)* Cloud Build on `main`: migrate job → deploy `api` to dev

---

## Milestone 1 — Working Go API

002 calls the `ObjectStore` interface (003) and `Queue.Enqueue` (004), so those two pieces are
pulled forward here.

- [ ] **[001 Tenancy & identity](../specs/001-tenancy-identity/tasks.md)**
  - [x] Schema + forced RLS + `app` role (Group 1.1–1.3)
  - [x] `app_system` role, `system_access` policy, `memberships_for_user` (1.4)
  - [ ] Cloud SQL IAM role mapping in Terraform (1.5) *(grant mechanism in `cmd/migrate` done; Terraform IAM users written, not applied)*
  - [x] Auth: argon2id, JWT, register / login / me (Group 2)
  - [x] Middleware chain: Authenticate → ResolveTenant → Authorize (Group 3)
  - [ ] Security tests incl. no-context zero rows and the closed system-function list (Group 5) *(written, not yet run)*
- [ ] **Pulled forward:** [003](../specs/003-object-storage/tasks.md) Group 1 (interface + `DocumentKey`) and 2.1 (Cloud Storage impl) *(interface, key builder and local fs store done; Cloud Storage impl pending GCP)*
- [x] **Pulled forward:** [004](../specs/004-job-queue/tasks.md) 1.1 (`jobs` table) and 2.1 (`Enqueue`) *(whole queue + worker runtime done; DB tests not yet run)*
- [ ] **[002 Document management](../specs/002-document-management/tasks.md)**
  - [x] `documents` table, partial unique dedup index (Group 1)
  - [x] Signature sniffer + size-limited reader (Group 2)
  - [x] Upload: store object → document row + ingest job in one tx; dedup; compensation (Group 3)
  - [x] List / get / soft delete (row-locking `UPDATE`) / 410 on deleted content (Group 4)
  - [ ] Integration, cross-tenant, and concurrent-duplicate tests (Group 5) *(written with the fs store, not yet run)*
- [ ] Deployed to dev; smoke test: register → create tenant → upload → list

**Exit gate:** tenant-scoped API tests pass (TEN-AC-001…010, DOC tests green in Cloud Build).

---

## Milestone 2 — Async ingestion

- [ ] Specs 003–006 **Approved**
- [ ] **[003 Object storage](../specs/003-object-storage/tasks.md)** — remainder
  - [ ] Bucket Terraform: uniform access, public access prevention, scoped IAM (2.1a) *(written, not applied)*
  - [ ] Idempotent delete; content endpoint with V4 signed URLs via IAM `signBlob` (2.2, Group 3) *(fs store + streaming content endpoint done; GCS delete and signed URLs pending GCP)*
  - [ ] Signed-URL replay / tamper test against the real dev bucket (4.2)
- [ ] **[004 Job queue](../specs/004-job-queue/tasks.md)** — remainder
  - [x] `claim_job` system function (reap-then-claim) + `system_access` on `jobs` (2.2)
  - [x] Heartbeat / Complete / Fail guarded by `locked_by`; backoff (2.3–2.4)
  - [x] Worker loop: semaphore before claim, SIGTERM shutdown, lease takeover (Group 3)
  - [ ] Concurrency, crash, retry-ladder, **poison-pill**, RLS, load tests (Group 4) *(concurrency, retry-ladder, poison-pill and RLS written, not yet run; SIGKILL crash and load tests not written)*
- [ ] **[005 Ingestion pipeline](../specs/005-ingestion-pipeline/tasks.md)**
  - [x] Extractors in a subprocess with Linux rlimits + hostile fixture corpus (Group 1)
  - [x] Chunker + ordinal-stability test (Group 2)
  - [ ] Vertex AI embedding provider + fake; dimension validation; 429 retry (Group 3) *(all but the Vertex provider done)*
  - [x] Index tx with `FOR UPDATE`, stale-tail delete, guarded status writes (4.1–4.2)
  - [ ] **Delete-vs-Index interleaving test** (4.2a) *(written, not yet run)*
  - [ ] Rerun idempotency, end-to-end fixtures, load profile (Group 5)
- [ ] **[006 Outbox messaging](../specs/006-outbox-messaging/tasks.md)**
  - [x] `outbox` table + `Emit`; `outbox_next` / `outbox_mark_published` / `outbox_mark_failed` system functions
  - [x] `LogPublisher` + relay binary; poison-row quarantine
  - [ ] Emission points wired in 002 / 005 / 013; atomicity, redelivery, ordering tests *(002/005 wired; tests written, not yet run)*
- [ ] Terraform + Cloud Build: `worker` and `outbox-relay` worker pools deployed to dev
- [ ] Drill on dev: stop a worker instance mid-job → job recovers, document consistent

**Exit gate:** restarting or killing a worker does not corrupt document state.

---

## Milestone 3 — RAG end to end

- [ ] Specs 007–009 and **014** **Approved** (the exit gate needs the golden dataset)
- [ ] **Start early:** [014](../specs/014-evaluation/tasks.md) 1.1 — `golden.jsonl` (≥ 50 cases, 2+ tenants) + fixture corpus + thresholds
- [ ] **[007 Hybrid retrieval](../specs/007-hybrid-retrieval/tasks.md)**
  - [x] Hybrid query (`vector.sql` + `fulltext.sql` + `hydrate.sql`, RRF in Go), deleted-document filter, per-document cap (Group 1)
  - [ ] Iterative scan or over-fetch decision, tested with the **smallest tenant** (2.1) *(decided and coded; test not yet run)*
  - [ ] RRF unit tests, seeded multi-tenant suite, latency benchmark (Group 3) *(unit tests pass; suite written, not run; benchmark pending)*
- [ ] **[008 Generation & citations](../specs/008-generation-citations/tasks.md)**
  - [x] Grounded prompt builder with untrusted-data delimiters (1.1)
  - [ ] Vertex AI `Generator` stream (tokens + terminal error + usage) + mock (1.2) *(stream abstraction + fake done; Vertex pending GCP)*
  - [x] SSE handler — events end with a blank line (1.3)
  - [x] `ResolveCitations`, usage rows, disconnect propagation, citation re-authorization (Group 2)
  - [ ] SSE, citation, zero-hit, and failure-path tests (Group 3) *(unit tests pass; end-to-end DB suite written, not yet run)*
- [ ] **[009 Reranking](../specs/009-reranking/tasks.md)** (optional stage, off by default)
  - [ ] Interface + implementation(s) + timeout / fail-open (Groups 1–2) *(all done in code; cross-encoder credentials pending GCP; usage DB test not yet run)*
  - [ ] Keep only if the A/B eval shows it earns its latency and cost (3.2)
- [ ] [014](../specs/014-evaluation/tasks.md) harness core: run, retrieval metrics, judge, citations, refusals, gate (2.1–2.6)

**Exit gate:** the golden dataset shows measurable retrieval and answer quality (report artifact).

---

## Milestone 4 — Observable production

- [ ] Specs 010–011 **Approved**
- [ ] **[010 Observability](../specs/010-observability/tasks.md)**
  - [ ] OTel setup; collector sidecar → Cloud Trace + Managed Prometheus (1.1) *(code + Terraform written; not applied)*
  - [x] Request ids; trace context carried through job payloads (1.2–1.3)
  - [x] Query and ingestion spans; metrics with the label allowlist + CI lint (Group 2)
  - [x] slog JSON with Cloud Logging trace fields; no-document-text log scan (3.1)
  - [ ] Cloud Monitoring dashboards in Terraform; collector-down resilience (3.2–3.3) *(dashboard written, not applied; resilience test passes)*
- [ ] **[011 Quotas & cost control](../specs/011-quotas-cost-control/tasks.md)**
  - [ ] Quota tables + limiter (Group 1)
  - [ ] Ask / upload / worker gates; reconciliation from `usage_events` (Group 2)
  - [ ] Per-provider circuit breakers (Vertex `429` / `RESOURCE_EXHAUSTED`); usage endpoint; overrides (Group 3)
  - [ ] Race, breaker-transition, and latency tests (Group 4)
- [ ] Alert policies: queue age, job dead-letters, provider error rate, spend per tenant

**Exit gate:** explain the latency and the failure of a real request from its telemetry alone
(one slow + one failed request, documented).

---

## Milestone 5 — Release-quality capstone

- [ ] Specs 012, 013, 015 **Approved**
- [ ] **[012 Guardrails](../specs/012-guardrails/tasks.md)**
  - [ ] `guard.Run` fail-closed core (Group 1)
  - [ ] Input, retrieval-hygiene, prompt, tools (deny-all), output, faithfulness, budget stages (Group 2)
  - [ ] Ask-path wiring with `windowed` / `strict` output modes; verdict metrics (Group 3)
  - [ ] Semantics, stage, injection, overhead tests (Group 4)
- [ ] **[013 Deletion & cleanup](../specs/013-deletion-cleanup/tasks.md)**
  - [ ] `cleanup` job: explicit chunk delete + invariant + object delete → `deleted` (Group 1)
  - [ ] Tenant purge; retention sweeper via `sweep_candidates` (Group 2)
  - [ ] All four interleaving tests; exclusion timing (Group 3)
  - [ ] Authz matrix, performance, red-team deletion-race pass (Group 4)
- [ ] **[014 Evaluation](../specs/014-evaluation/tasks.md)** — remainder: A/B mode, judge-down mode, CI (fast set per PR, full nightly), first recorded tuning decision
- [ ] **[015 Isolation & red team](../specs/015-isolation-redteam/tasks.md)**
  - [ ] Test project `<prefix>-test` provisioned; canary seeding; capture harness; target guard (Group 1)
  - [ ] Isolation suite on every PR (Group 2)
  - [ ] Red-team categories nightly / per release (Group 3)
  - [ ] `docs/threat-model.md`, red-team report, CI wiring (Group 4)

**Exit gate:** another developer, starting from a browser and a GCP account, gets the project
running by following [gcp-environment.md §11](gcp-environment.md#11-bootstrap-order-first-time).

---

## Production readiness

- [ ] `<prefix>-prod` project via the same Terraform; budget + alerts
- [ ] Same image digest promoted test → prod (no rebuilds)
- [ ] Cloud SQL: automated backups + point-in-time recovery; restore tested once
- [ ] Cloud Storage: object versioning / retention decision recorded ([013](../specs/013-deletion-cleanup/requirements.md))
- [ ] `api` min instances ≥ 1; worker pool sizing from load tests
- [ ] Trace sampling set for prod (spec 010 default: 10% head + keep errors)
- [ ] Secrets only in Secret Manager; no service-account keys exist anywhere
- [ ] Isolation suite green; latest red-team report reviewed; no open critical findings
- [ ] Runbook: dead-letter handling, stuck queue, provider outage, tenant purge, restore

## Definition of done (every pull request)

- [ ] Each acceptance criterion touched has a test that cites its ID
- [ ] `go test ./...` and `go test -race ./...` green in Cloud Build
- [ ] All tenant data access goes through `WithTenant` or a listed system function
- [ ] No document text, prompts, answers, or secrets in logs or metric labels
- [ ] Spec `tasks.md` boxes ticked; spec status updated if it changed
