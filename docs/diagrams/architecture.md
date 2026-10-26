# Architecture Diagrams

> Multi-Tenant RAG Platform in Go — visual architecture reference.
> All diagrams are [Mermaid](https://mermaid.js.org/) and render natively on GitHub.
>
> Companion docs: [System Architecture](../system-architecture.md) ·
> [System Design](../system-design.md) · [Technology Stack](../tech-stack.md)

## Contents

1. [System context (C4 — level 1)](#1-system-context-c4--level-1)
2. [Container diagram (C4 — level 2)](#2-container-diagram-c4--level-2)
3. [Write path — asynchronous ingestion (sequence)](#3-write-path--asynchronous-ingestion-sequence)
4. [Read path — ask with SSE streaming (sequence)](#4-read-path--ask-with-sse-streaming-sequence)
5. [State machines — document status and job lifecycle](#5-state-machines--document-status-and-job-lifecycle)
6. [Deployment view (Docker Compose)](#6-deployment-view-docker-compose)
7. [Trust boundaries and guard layers](#7-trust-boundaries-and-guard-layers)

---

## 1. System context (C4 — level 1)

Who uses the platform and which external systems it depends on. Everything inside the
platform box enforces tenant isolation server-side.

```mermaid
flowchart TB
    subgraph clients["Clients"]
        MEMBER["Tenant member<br/>uploads documents · asks questions"]
        ADMIN["Tenant admin<br/>manages members · documents"]
        OPS["Platform operator<br/>watches dashboards · cost · failures"]
    end

    RAG["Multi-Tenant RAG Platform<br/>Go API · Go worker · outbox relay<br/>tenant-isolated ingestion and grounded answers"]

    subgraph external["External systems"]
        EMB["Embedding provider<br/>hosted or local"]
        LLM["LLM provider<br/>hosted or local"]
        TEL["Telemetry backend<br/>Prometheus · Grafana · trace store"]
    end

    MEMBER -->|"HTTPS upload · search · ask SSE"| RAG
    ADMIN -->|"HTTPS manage users · documents"| RAG
    OPS -->|"dashboards · alerts"| TEL
    RAG -->|"embed question and chunks"| EMB
    RAG -->|"grounded generation, streamed"| LLM
    RAG -.->|"traces · metrics"| TEL
```

---

## 2. Container diagram (C4 — level 2)

Three Go binaries share `internal/` packages and scale/fail independently. PostgreSQL is the
only stateful core: relational data, vectors, keyword search, job queue, and outbox.

```mermaid
flowchart LR
    C["Client"]

    subgraph platform["Go services (stateless)"]
        API["cmd/api<br/>auth · tenant resolution · document CRUD<br/>search · ask · SSE streaming"]
        W["cmd/worker<br/>extract · chunk · embed · index<br/>deletion cleanup · bounded pool"]
        RELAY["cmd/outbox-relay<br/>publishes outbox rows to a broker<br/>optional until a broker exists"]
    end

    subgraph state["State"]
        PG[("PostgreSQL + pgvector<br/>tenants · users · memberships<br/>documents · chunks<br/>jobs · outbox · usage_events")]
        OBJ[("Object storage MinIO / S3<br/>tenant-prefixed keys")]
    end

    subgraph providers["Model providers (behind interfaces)"]
        EMB["Embedding provider"]
        LLM["LLM provider"]
    end

    subgraph telemetry["Telemetry"]
        OTC["OTel collector"]
        PROM["Prometheus"]
        GRAF["Grafana"]
    end

    C -->|"HTTPS upload · ask"| API
    API -->|"store file"| OBJ
    API -->|"document + job + outbox, one tx"| PG
    API -->|"embed question"| EMB
    API -->|"stream tokens"| LLM
    W -->|"claim job, lease, heartbeat"| PG
    W -->|"fetch original file"| OBJ
    W -->|"embed chunks in batches"| EMB
    W -->|"chunks + vectors, one tx"| PG
    RELAY -->|"read rows, mark published"| PG
    API -.->|"traces · metrics"| OTC
    W -.->|"traces · metrics"| OTC
    OTC --> PROM --> GRAF
```

---

## 3. Write path — asynchronous ingestion (sequence)

Everything left of the index is about getting trustworthy data in. Ingestion is an idempotent
state machine: a crash and retry cannot corrupt a document.

```mermaid
sequenceDiagram
    autonumber
    participant CL as Client
    participant API as cmd/api
    participant OBJ as Object storage
    participant PG as PostgreSQL
    participant WK as cmd/worker
    participant EMB as Embedding provider

    CL->>API: POST /documents (file)
    API->>API: auth · membership check · size limit · MIME + signature validation
    API->>OBJ: PUT file under tenant-prefixed key
    API->>API: sha256 → per-tenant duplicate check
    API->>PG: BEGIN · INSERT document · INSERT job · COMMIT
    API-->>CL: 202 Accepted (document id, status queued)

    loop bounded worker pool
        WK->>PG: claim job — FOR UPDATE SKIP LOCKED, 60s lease, attempts+1
        WK->>OBJ: GET file
        WK->>WK: extract (isolated, limits) → chunk 300-800 tok, 10-15% overlap
        WK->>EMB: embed batches (validate dimensions, record model)
        WK->>PG: tx: upsert chunks + vectors → status indexed
        Note over WK,PG: on stage error: status failed + reason,<br/>bounded retries with backoff + jitter,<br/>lease expiry recovers crashed workers
    end
```

---

## 4. Read path — ask with SSE streaming (sequence)

Everything right of the index is about getting a grounded, attributable answer out. The tenant
always comes from the authenticated identity — never from a request field.

```mermaid
sequenceDiagram
    autonumber
    participant CL as Client
    participant API as cmd/api
    participant EMB as Embedding provider
    participant PG as PostgreSQL
    participant RR as Reranker (optional)
    participant LLM as LLM provider

    CL->>API: POST /ask (question) — SSE
    API->>API: authenticate → resolve tenant from identity
    API->>EMB: embed question
    API->>PG: hybrid search, tenant-scoped: vector top-50 + full-text top-50
    Note over API,PG: join documents — soft-deleted / deleting excluded
    API->>API: fuse rankings — Reciprocal Rank Fusion, k = 60
    API->>RR: rerank top-50 → top-8 (only if it earns its latency and cost)
    API->>LLM: grounded prompt — numbered chunks, answer only from them
    loop stream
        LLM-->>API: token
        API-->>CL: SSE event "token"
    end
    API->>API: resolve citations [n] → only chunks retrieved for THIS request
    API-->>CL: SSE events "citation", "done"
    API->>PG: INSERT usage_events (input/output tokens)
    Note over API,LLM: request context propagates to provider —<br/>client disconnect stops token spend
```

---

## 5. State machines — document status and job lifecycle

### Document status

```mermaid
stateDiagram-v2
    [*] --> uploaded: Accept — file stored, row committed
    uploaded --> queued: job enqueued in same transaction
    queued --> processing: worker claims job, lease acquired
    processing --> indexed: chunks + vectors written in one tx
    processing --> failed: stage error, reason recorded
    failed --> processing: bounded retry, backoff + jitter
    uploaded --> deleting: soft delete requested
    queued --> deleting: soft delete requested
    processing --> deleting: soft delete requested
    indexed --> deleting: soft delete requested
    deleting --> [*]: chunks, vectors, cached content, object cleaned up
```

### Job lifecycle

```mermaid
stateDiagram-v2
    [*] --> queued: enqueued with the state change
    queued --> running: claim — SKIP LOCKED, lease 60s, attempts+1
    running --> running: heartbeat extends lease
    running --> succeeded: pipeline finished
    running --> queued: lease expired — crash recovery
    running --> failed: stage error
    failed --> queued: retry — attempts below max, backoff + jitter
    failed --> dead: attempts exhausted — surfaced for inspection
    succeeded --> [*]
    dead --> [*]
```

---

## 6. Deployment view (Docker Compose)

```mermaid
flowchart TB
    subgraph infra["docker-compose (dev) · containers (prod)"]
        LB["Load balancer / reverse proxy"]

        subgraph services["Stateless services — scale independently"]
            API1["api #1"]
            APIN["api #N — scale by request load"]
            W1["worker #1 — bounded pool"]
            WN["worker #N — scale by queue depth / age"]
            RELAY["outbox-relay — single active instance"]
        end

        PG[("PostgreSQL + pgvector<br/>single stateful core<br/>watch: queue depth · lock waits · ANN recall")]
        MINIO[("MinIO dev · S3 prod")]
        OTC["OTel collector"]
    end

    PROM["Prometheus"] --> GRAF["Grafana dashboards"]

    LB --> API1
    LB --> APIN
    API1 --> PG
    APIN --> PG
    API1 --> MINIO
    W1 --> PG
    WN --> PG
    W1 --> MINIO
    WN --> MINIO
    RELAY --> PG
    API1 -.-> OTC
    W1 -.-> OTC
    WN -.-> OTC
    OTC --> PROM
```

Notes:

- Workers shut down gracefully: in-flight jobs finish (`WaitGroup`); abandoned jobs are
  recovered via lease expiry — restarting a worker never corrupts document state.
- A broker (Kafka / NATS) can later replace the in-Postgres queue via the transactional
  outbox; consumers stay idempotent.

---

## 7. Trust boundaries and guard layers

**Core rule: the model is not a security boundary.** Hard guarantees live in deterministic
code and the database; model-based checks are an extra, probabilistic layer. Guards **fail
closed**: an error or timeout blocks the response.

```mermaid
flowchart LR
    subgraph untrusted["Untrusted inputs"]
        U1["Client requests<br/>questions · IDs · headers · tokens"]
        U2["Document text<br/>incl. hidden injected instructions"]
        U3["Model output<br/>markers · links · images"]
    end

    subgraph guards["Guard layers — deterministic, fail closed"]
        G1["Input<br/>rate limits · size and type checks"]
        G2["Retrieval<br/>RLS + tenant filter · soft-deleted excluded · chunk caps"]
        G3["Prompt assembly<br/>delimited untrusted data · no secrets · context budget"]
        G4["Tools and actions<br/>allowlist · schema validation · human confirmation"]
        G5["Output<br/>citation verification · refusals · redaction"]
        G6["Budget<br/>token limits · spend quotas · circuit breakers"]
    end

    subgraph trusted["Trusted core"]
        T1["Authenticated identity + membership"]
        T2["PostgreSQL RLS<br/>app.tenant_id set per transaction"]
        T3["Citations resolve to this request's retrieved records only"]
    end

    U1 --> G1 --> T1
    U2 --> G2 --> T2
    U2 --> G3
    U3 --> G5 --> T3
    G4 -.->|"the model proposes, code decides"| T1
    G6 -.->|"quotas stop spend"| T1
```

Canary check: every test tenant's documents carry a unique meaningless token — if tenant A's
answers ever contain tenant B's canary, there is a leak.

---

## Viewing / editing

- GitHub renders all diagrams above natively — no tooling needed.
- To iterate on a diagram, paste its Mermaid source into [mermaid.live](https://mermaid.live).
