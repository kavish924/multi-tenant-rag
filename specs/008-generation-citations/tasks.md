# 008 — Generation, Streaming & Citations: Tasks

## Group 1: Prompt and stream

- [x] 1.1 Prompt builder: numbered chunks, only-from-context instructions, insufficient-context instruction, untrusted-data delimiters, context budget hook (cap enforced fully in `012`). (GEN-REQ-002, GEN-AC-005/008)
- [ ] 1.2 *(stream abstraction + deterministic `llm.FakeGenerator` done; Vertex AI Gemini provider pending GCP)* `Generator` stream abstraction (tokens + terminal error + final usage) with provider impl + mock. (GEN-REQ-001)
- [x] 1.3 SSE handler: Flusher check, headers, event loop, error event, close. (GEN-REQ-001/003/008, GEN-AC-001/006)

## Group 2: Citations and usage

- [x] 2.1 `ResolveCitations` (regex filter, range check, dedupe) + citation event emission. (GEN-REQ-005, GEN-AC-002/003)
- [ ] 2.2 *(done in code; DB rows asserted in `tests/integration/ask_test.go`, not yet run)* Usage recording: `generate` (and retrieval `embed`) usage rows on every terminal path. (GEN-REQ-007, GEN-AC-007)
- [x] 2.3 Disconnect propagation into provider call + timing test. (GEN-REQ-004, GEN-AC-004, GEN-NFR-002)
- [ ] 2.4 *(existing content endpoint re-checks access; asserted in `tests/integration/ask_test.go`, not yet run)* Citation dereference re-authorization (document deleted/foreign-tenant paths). (GEN-REQ-006, GEN-AC-009)

## Group 3: Tests and verification

- [ ] 3.1 *(unit SSE suite passes; end-to-end suite written, not yet run)* SSE integration suite (event order, framing, done/citation payloads). (GEN-AC-001/002)
- [x] 3.2 Citation unit table: valid, out-of-range, duplicate, malformed. (GEN-AC-003)
- [ ] 3.3 *(unit tests pass)* Zero-hits path; 502 path; mid-stream provider failure path. (GEN-AC-005/006)
- [ ] 3.4 *(with 014)* **Milestone 3 exit contribution:** end-to-end ask on golden sample produces grounded, cited answer.
