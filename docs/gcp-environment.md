# GCP Environment

> Multi-Tenant RAG Platform in Go — where development, testing, and runtime live. **Nothing is
> installed on the developer's laptop**: code is written and tested in Cloud Shell, CI runs in
> Cloud Build, and every environment runs on Google Cloud. Sized to start on the **$300 free-trial
> credit** (§10).

## 1. Principles

- **Browser-only laptop.** The laptop needs a browser. Go, Docker, `gcloud`, Terraform, and `psql`
  come with Cloud Shell (§3).
- **One project per environment.** `<prefix>-dev`, `<prefix>-test`, `<prefix>-prod` (on the free
  trial, start with one project as dev; add test/prod when they are needed). Projects are
  the hard boundary for IAM, billing, quotas, and data — the same isolation-first instinct the
  platform applies to tenants.
- **Everything as code.** Terraform in `deployments/terraform/` creates every resource below;
  Cloud Build applies it. Console clicks are for looking, not for changing.
- **One region per environment.** Cloud SQL, Cloud Storage, Cloud Run, and Vertex AI in the same
  region to avoid cross-region latency and egress.

## 2. Resource map

| Concern | GCP service | Notes |
| ------- | ----------- | ----- |
| Development machine | **Cloud Shell** (fallback: small Compute Engine VM) | Free; Go, Docker, Terraform built in (§3) |
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

## 3. Development environment — Cloud Shell

**Why not Cloud Workstations:** a workstation cluster bills a control-plane fee of about $0.20 per
hour whether or not any workstation runs (~$144/month), plus $0.05 per vCPU-hour of management
fee. That alone would consume half the free-trial credit in two months. Cloud Shell gives the
same browser-only workflow at no cost.

**Cloud Shell** (console → `>_` icon, or full screen at `shell.cloud.google.com`):

- Free; Go, Docker, `gcloud`, Terraform, `psql`, and git are preinstalled. `go.mod` pins Go 1.25;
  with the default `GOTOOLCHAIN=auto`, an older preinstalled Go downloads 1.25 automatically.
- 5 GB persistent `$HOME` (repo clone and module cache); the VM itself is ephemeral, and there is
  a weekly usage quota. Docker images and containers do not survive a session restart.
- Built-in editor (Code OSS based) via **Open Editor**.
- Eligibility: Google Workspace accounts in a new or trial organization can be refused ("not
  eligible to use Cloud Shell") until the Workspace subscription has a processed payment. Fixes:
  open the console with a personal Google account that has the **Owner** role on the project, or
  use the fallback VM below.

**Fallback: devbox VM.** Compute Engine `e2-standard-2`, Debian 12, 30 GB disk, opened with the
console's **SSH** button and provisioned by [scripts/devbox-setup.sh](../scripts/devbox-setup.sh)
(Docker, Go, golangci-lint, Terraform, `psql`). About $0.07 per running hour; stop it when idle.

**Inner loop (Cloud Shell or devbox):**

```bash
make up                        # deployments/compose.yaml: postgres (pgvector image)
make test                      # go vet + go test -race ./...
make itest                     # integration tests (testcontainers, fresh Postgres per package)
make dev-login migrate run-api # API against the compose stack
```

The compose stack is the fast, disposable environment for tests and debugging; the deployed dev
environment (Cloud Run + Cloud SQL) is the integration target.

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
   Build steps; in Cloud Shell the same tests use testcontainers)
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

- Private IP only, reached through Direct VPC egress (Cloud Run). People use Cloud SQL Studio
  in the console; development tests never need the real instance.
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

**Dev cost control:** the dev instance uses the shared-core `db-f1-micro` tier and is stopped
outside working hours (§10).

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
- In Cloud Shell, the compose `otel-collector` exports to the console for debugging.

## 10. Cost guardrails — the $300 free-trial plan

The free trial gives $300 of credit for 90 days. The dev environment is sized to use a fraction of
it, leaving room for Vertex AI once retrieval and generation land (milestone 3).

| Item | Choice | Approx. monthly cost |
| ---- | ------ | -------------------- |
| Development | Cloud Shell (devbox VM only as fallback) | $0 (devbox: ~$0.07/h running) |
| Cloud SQL | `db-f1-micro`, 10 GB SSD, zonal, 3 backups; **stopped when not in use** | ~$9 if run 24/7, plus ~$2 disk |
| Cloud Run `api` | min instances 0, max 2 | $0 (free tier) |
| Cloud Build | default `e2-standard-2` machine | $0 (2,500 free build-minutes) |
| Artifact Registry | keep 5 versions per image | ~$0 (0.5 GB free) |
| Cloud Storage, Secret Manager, Logging | small volumes | ~$0 |
| Vertex AI | pay per use (from milestone 3) | a few dollars in dev |
| **Total** | | **~$10–15/month** before Vertex AI |

Not used, because they bill while idle: Cloud Workstations (~$144/month cluster fee), regional
(HA) Cloud SQL, Cloud NAT, min instances ≥ 1, GPUs, and a separate test project until needed.

Habits:

- **Budget alerts** at 50/90/100 % of a monthly budget (Billing → Budgets & alerts).
- Stop Cloud SQL when done for the day and start it before working:
  `gcloud sql instances patch dev-pg --activation-policy=NEVER` (or `ALWAYS`). A stopped instance
  bills only its disk.
- Check **Billing → Reports** weekly, grouped by service.
- Vertex spend is bounded by the platform's per-tenant quotas (`011`) and by project quotas.
- When the trial ends, resources stop unless the account is upgraded to a paid account; upgrade
  before day 90 to keep the environment.

## 11. Bootstrap order (first time)

Everything runs in **Cloud Shell** (or the devbox VM, §3); the laptop needs only a browser.

1. Link billing (free-trial credit) and set a budget alert. In Cloud Shell, enable the base APIs
   (`cloudresourcemanager`, `serviceusage`, `compute`, `cloudbuild`) and create the Terraform
   state bucket:
   `gcloud storage buckets create gs://<project>-tfstate --location=<region> --uniform-bucket-level-access --public-access-prevention`.
2. Clone the repo, run `make tidy`, commit `go.sum`, and run `make up && make test && make itest`.
3. Connect the GitHub repository to Cloud Build once in the console (Cloud Build → Triggers →
   Connect repository); the triggers in step 4 refer to it.
4. Apply `deployments/terraform/bootstrap` (APIs, VPC + private services access, Artifact
   Registry, `sa-build` + triggers):
   `terraform init -backend-config=bucket=<project>-tfstate -backend-config=prefix=bootstrap`, then
   `terraform apply` with a `terraform.tfvars` copied from the `.example` file.
5. Apply `deployments/terraform/env/dev` (Cloud SQL, bucket, runtime service accounts + IAM
   database users, JWT secret, Cloud Run `api` service and `migrate` job), with
   `-backend-config=prefix=env/dev`.
6. Run [scripts/cloudsql-bootstrap.sql](../scripts/cloudsql-bootstrap.sql) once in Cloud SQL Studio
   as `postgres` (roles for `migrator`, pgvector version check).
7. Merge to `main`: Cloud Build tests, pushes images, runs the migrate job, and deploys `api` to dev.
8. Later: repeat 4–7 for `test` and `prod` with their own projects and variables.

This sequence is the documented setup referenced by the Milestone 5 exit criterion.

## Related docs

- [Technology Stack](tech-stack.md) — tools, rationale, swap points
- [System Architecture](system-architecture.md) — binaries, paths, deployment view
- [System Design](system-design.md) — data model, tenancy, pipeline, retrieval, guardrails
