# GCP Environment

> Multi-Tenant RAG Platform in Go — where development, testing, and runtime live. **Nothing is
> installed on the developer's laptop**: code is written in Cloud Workstations, tests run there
> and in Cloud Build, and every environment runs on Google Cloud.

## 1. Principles

- **Browser-only laptop.** The laptop needs a browser (or VS Code with the Cloud Workstations
  extension). Go, Docker, `gcloud`, Terraform, and `psql` live in the workstation image.
- **One project per environment.** `<prefix>-dev`, `<prefix>-test`, `<prefix>-prod`. Projects are
  the hard boundary for IAM, billing, quotas, and data — the same isolation-first instinct the
  platform applies to tenants.
- **Everything as code.** Terraform in `deployments/terraform/` creates every resource below;
  Cloud Build applies it. Console clicks are for looking, not for changing.
- **One region per environment.** Cloud SQL, Cloud Storage, Cloud Run, and Vertex AI in the same
  region to avoid cross-region latency and egress.

## 2. Resource map

| Concern | GCP service | Notes |
| ------- | ----------- | ----- |
| Development machine | **Cloud Workstations** | Custom image (§3); idle timeout; persistent home disk |
| Source + CI/CD | GitHub + **Cloud Build** | Trigger on push / PR (§4) |
| Container images | **Artifact Registry** | One Docker repository per project |
| `cmd/api` | **Cloud Run service** | SSE streaming; request timeout raised for long answers |
| `cmd/worker` | **Cloud Run worker pool** (fallback: Cloud Run service with min instances ≥ 1 and instance-based billing, or GKE Autopilot) | Pull-based polling loop, not request-driven |
| `cmd/outbox-relay` | Same as worker, exactly one instance | Publishes to **Pub/Sub** once a broker is justified |
| Migrations | **Cloud Run job** (`cmd/migrate` or the migration tool's image) | Runs inside the VPC, so it reaches Cloud SQL's private IP |
| Database | **Cloud SQL for PostgreSQL** + `pgvector` | Private IP only; IAM database authentication (§6) |
| Files | **Cloud Storage** | One bucket per environment; uniform bucket-level access; public access prevention enforced |
| LLM + embeddings | **Vertex AI** | Called with the service account's identity — no API keys |
| Secrets | **Secret Manager** | JWT signing key and any third-party keys |
| Traces | **Cloud Trace** | Via OpenTelemetry (§8) |
| Metrics + dashboards | **Managed Service for Prometheus** + **Cloud Monitoring** | Dashboards and alert policies defined in Terraform |
| Logs | **Cloud Logging** | Structured JSON on stdout |
| Network | VPC with **Direct VPC egress** from Cloud Run | Private path to Cloud SQL |

## 3. Development environment — Cloud Workstations

**Workstation configuration** (Terraform, in the dev project):

- Base image: Google's Code OSS (or IntelliJ) workstation image, extended by
  `deployments/workstation/Dockerfile` with Go (pinned version), `golangci-lint`, `gcloud`,
  Terraform, `psql`, and the migration CLI. Docker is available inside the workstation.
- Machine: `e2-standard-4` to start (Go builds + a Postgres container fit comfortably).
- **Idle timeout ~30 min** and a running-time cap, so a forgotten workstation stops billing.
- Persistent home disk for the clone, Go module cache, and editor settings.
- The workstation runs in the dev VPC, so it can reach the dev Cloud SQL private IP.

**Inner loop on the workstation:**

```bash
make up        # deployments/compose.yaml: postgres (pgvector image), fake-gcs-server, otel-collector
make migrate   # applies migrations to the compose Postgres
make test      # go test ./... && go test -race ./...
make itest     # integration tests (testcontainers — fresh Postgres per package)
make run-api   # API against the compose stack
```

The compose stack exists **only inside the workstation**. It is the fast, disposable environment
for tests and debugging; the deployed dev environment (Cloud Run + Cloud SQL) is the integration
target.

**Emulators used in tests:**

| Real service | Test double | How |
| ------------ | ----------- | --- |
| Cloud SQL | `pgvector/pgvector` container (same major Postgres version as Cloud SQL) | testcontainers |
| Cloud Storage | `fake-gcs-server` container | Go client honors `STORAGE_EMULATOR_HOST` |
| Vertex AI | In-process fakes behind `EmbeddingProvider` / `Generator` | Deterministic vectors and token streams |
| Pub/Sub (later) | Pub/Sub emulator container | `PUBSUB_EMULATOR_HOST` |

## 4. CI/CD — Cloud Build

`cloudbuild.yaml`, run by two triggers: **branches** (every push except `main`; checks only) and
**main** (checks, then push, migrate and deploy):

1. `go vet ./...`, lint
2. `go test ./...` and `go test -race ./...`
3. Integration tests against a Postgres container started on the `cloudbuild` network and reached
   by name through `TEST_DATABASE_URL` (testcontainers' port mapping is unreliable inside Cloud
   Build steps; on the workstation the same tests use testcontainers)
4. Build images → Artifact Registry (tagged with the commit SHA)
5. *(main branch only)* Run migrations as a Cloud Run job in **dev**
6. *(main branch only)* Deploy `api`, `worker`, `outbox-relay` to **dev**

Promotion to **test** / **prod** deploys the **same image digest** — never a rebuild. Evaluation
(`014`) and red-team (`015`) suites run against the **test** project, never prod.

## 5. Runtime — Cloud Run

| Binary | Shape | Key settings |
| ------ | ----- | ------------ |
| `api` | Cloud Run service | Concurrency tuned for SSE (long-lived connections count against it); request timeout long enough for streaming; min instances 0 in dev, ≥ 1 in prod |
| `worker` | Cloud Run worker pool | Instance count scaled on queue depth/age; `SIGTERM` → stop claiming, finish in-flight jobs within the shutdown grace period (lease expiry covers anything abandoned) |
| `outbox-relay` | Cloud Run worker pool | Exactly one instance |
| `migrate` | Cloud Run job | Run before each deploy |

All run on Linux containers, so parser isolation (`005`) uses Linux resource limits
(`setrlimit`/`prlimit` on the extractor subprocess) plus a wall-clock timeout.

**Service accounts (one per binary, least privilege):**

| Service account | Roles |
| --------------- | ----- |
| `sa-api` | Cloud SQL Client + Instance User; Storage object admin on its bucket only; Vertex AI User; Secret Manager accessor (JWT key); Token Creator **on itself** (to sign Cloud Storage V4 URLs via IAM `signBlob`) |
| `sa-worker` | Cloud SQL Client + Instance User; Storage object admin on its bucket; Vertex AI User |
| `sa-relay` | Cloud SQL Client + Instance User; Pub/Sub publisher (when introduced) |
| `sa-migrate` | Cloud SQL Client + Instance User (database role `migrator`) |
| `sa-build` | Artifact Registry writer; Cloud Run developer; act-as the runtime service accounts |

## 6. Cloud SQL and database roles

- Private IP only, reached through Direct VPC egress (Cloud Run) or the dev VPC (workstation).
- Connect with the **Cloud SQL Go connector** using **IAM database authentication** — no
  database passwords.
- Confirm the available `pgvector` version (`SELECT extversion FROM pg_extension WHERE extname =
  'vector'`). Iterative index scans (`RET-REQ-007`) need ≥ 0.8; otherwise use the over-fetch
  mitigation.

| Database role | Login | Owns / can do |
| ------------- | ----- | ------------- |
| `migrator` | IAM user of `sa-migrate` | Owns tables and system functions; runs migrations |
| `app` | Group role granted to the IAM users of `sa-api`, `sa-worker`, `sa-relay` | DML on tenant tables under forced RLS; `EXECUTE` on system functions only |
| `app_system` | `NOLOGIN`, **no** `BYPASSRLS` | Owns the `SECURITY DEFINER` system functions; sees all tenants only through policies scoped `TO app_system` |

Cloud SQL does not hand out true superuser, so the platform must not depend on `BYPASSRLS`. The
cross-tenant path uses role-scoped policies instead (see
[system-design.md §1](system-design.md#cross-tenant-system-paths)). `app` must never be a member of
`app_system` — the isolation suite checks it.

Cloud SQL IAM users cannot create roles, so a one-time script run as the built-in `postgres` user
([scripts/cloudsql-bootstrap.sql](../scripts/cloudsql-bootstrap.sql)) creates `app` and `app_system` and
grants both to `migrator` `WITH ADMIN OPTION`. The migrations then skip role creation, and the migrate
job adds the IAM users listed in `APP_DB_LOGIN_ROLES` (api, worker, relay) to `app`.

**Dev cost control:** the dev instance uses a small machine tier and can be stopped outside
working hours (Terraform variable or a scheduled job).

## 7. Cloud Storage

- Bucket per environment, e.g. `<prefix>-dev-documents`; uniform bucket-level access; public access
  prevention enforced; optional object versioning for accidental-deletion recovery (`013`).
- Object keys unchanged: `tenants/{tenant_id}/documents/{document_id}`.
- Access through the `ObjectStore` interface (`003`) implemented with `cloud.google.com/go/storage`.
- Download links are **V4 signed URLs** with short expiry, issued only after the tenant/access check.

## 8. Models — Vertex AI

- `EmbeddingProvider` and `Generator` gain Vertex implementations; the rest of the code is
  unaware of the vendor.
- **Embedding dimension follows the chosen model.** Set `EMBEDDING_DIM` and the `chunks.embedding`
  column type from the selected Vertex embedding model's output size. pgvector's HNSW index
  supports `vector` up to 2,000 dimensions; above that use `halfvec` (up to 4,000) or request a
  reduced output dimensionality if the model supports it. Check the current Vertex model list
  when choosing.
- Rate limits and quotas are per project and region — the circuit breaker (`011`) treats
  Vertex `429`/`RESOURCE_EXHAUSTED` as provider back-pressure.

## 9. Observability

- Each Cloud Run service runs an **OpenTelemetry collector sidecar** receiving OTLP from the app
  and exporting traces to Cloud Trace and metrics to Managed Service for Prometheus.
- Logs: structured JSON to stdout with `trace` correlation fields, so Cloud Logging links log
  lines to traces. The never-log-document-text rule (`010`) is unchanged.
- Dashboards (latency, queue depth/age, job failures, tokens and cost per tenant) and alert
  policies live in Terraform as Cloud Monitoring resources.
- On the workstation, the compose `otel-collector` exports to the console for debugging.

## 10. Cost guardrails

- **Billing budget + alerts** on every project from day one.
- Workstation idle timeout; Cloud SQL dev instance stoppable; `api` min instances 0 in dev.
- Vertex spend is bounded by the platform's own per-tenant quotas (`011`) and by project quotas.

## 11. Bootstrap order (first time)

Steps 1–4 run in **Cloud Shell** (browser; it already has `gcloud` and Terraform), so the laptop
still needs nothing installed.

1. Create the dev project, link billing, set a budget alert. Create the Terraform state bucket:
   `gcloud storage buckets create gs://<project>-tfstate --location=<region> --uniform-bucket-level-access --public-access-prevention`.
2. Connect the GitHub repository to Cloud Build once in the console (Cloud Build → Triggers →
   Connect repository); the triggers in step 3 refer to it.
3. Apply `deployments/terraform/bootstrap` (APIs, VPC + private services access, Artifact
   Registry, `sa-build` + triggers, workstation cluster + config):
   `terraform init -backend-config=bucket=<project>-tfstate -backend-config=prefix=bootstrap`, then
   `terraform apply` with a `terraform.tfvars` copied from the `.example` file. The VPC lives here,
   not in `env/dev`, because the workstation and Cloud SQL must share it.
4. Build the workstation image:
   `gcloud builds submit --config deployments/workstation/cloudbuild.yaml --substitutions=_REGION=<region> deployments/workstation`.
5. Start a workstation from the `go-dev` config, clone the repo, run `make tidy` and commit
   `go.sum`, then `make up && make test && make itest`.
6. Apply `deployments/terraform/env/dev` (Cloud SQL, bucket, runtime service accounts + IAM
   database users, JWT secret, Cloud Run `api` service and `migrate` job), with
   `-backend-config=prefix=env/dev`.
7. Run [scripts/cloudsql-bootstrap.sql](../scripts/cloudsql-bootstrap.sql) once in Cloud SQL Studio
   as `postgres` (roles for `migrator`, pgvector version check).
8. Push to `main` → Cloud Build tests, pushes images, runs the migrate job, and deploys `api` to dev.
9. Repeat 3, 6–8 for `test` and `prod` with their own projects and variables.

This sequence is the documented setup referenced by the Milestone 5 exit criterion.

## Related docs

- [Technology Stack](tech-stack.md) — tools, rationale, swap points
- [System Architecture](system-architecture.md) — binaries, paths, deployment view
- [System Design](system-design.md) — data model, tenancy, pipeline, retrieval, guardrails
