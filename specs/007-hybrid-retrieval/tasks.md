# 007 — Hybrid Retrieval: Tasks

## Group 1: Query path

- [x] 1.1 `Retriever` interface + `Hit` type. (RET-REQ-001)
- [x] 1.2 `hybrid.sql` embedded + pgx query fn; question length guard. (RET-REQ-002/003/004, RET-AC-006)
- [x] 1.3 Deleted/`deleting` exclusion join. (RET-REQ-005, RET-AC-002)
- [x] 1.4 Per-document cap window-function pass (config, default 5). (RET-REQ-006, RET-AC-003)

## Group 2: Quality knobs

- [ ] 2.1 *(done in code: pgvector ≥ 0.8 → `hnsw.iterative_scan = relaxed_order`, else `hnsw.ef_search = 400`; small-tenant test written, not yet run)* Over-fetch factor / iterative-scan support detection; document decision + test with smallest tenant. (RET-REQ-007)
- [ ] 2.2 *(ef_search ≥ 100 set per query; EXPLAIN baseline needs a seeded Cloud SQL)* HNSW/GIN index tuning notes in migrations; EXPLAIN baseline captured. (RET-NFR-001)

## Group 3: Tests and verification

- [x] 3.1 RRF unit tests (both-legs > one-leg; k math). (RET-AC-007)
- [ ] 3.2 *(written in `internal/retrieval/retrieval_integration_test.go`, not yet run)* Seeded multi-tenant corpus integration suite: lexical hit, semantic hit, deleted exclusion, cross-tenant probe, limit edges. (RET-AC-001/002/004/005/008)
- [ ] 3.3 *(pending a load environment)* Latency benchmark harness (1M synthetic chunks). (RET-NFR-001)
- [ ] 3.4 *(with 014)* **Milestone 3 exit contribution:** retrieval leg of the golden-dataset measurement (with `014`).
