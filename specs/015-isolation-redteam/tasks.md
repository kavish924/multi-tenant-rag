# 015 — Isolation Tests & Red Teaming: Tasks

## Group 1: Infrastructure

- [ ] 1.1 Test-tenant provisioning + canary seeding into fixture corpora. (RT-REQ-002)
- [ ] 1.2 Capture harness (all answers saved) + canary scanner. (RT-REQ-002, RT-AC-003)
- [ ] 1.3 `redteam.yaml` config, localhost-target guard, budget meters. (design §7/§8)

## Group 2: Isolation suite (per-PR)

- [ ] 2.1 Parameterized cross-tenant id probes (documents, jobs, chunks, citations, signed URLs). (RT-REQ-001, RT-AC-001)
- [ ] 2.2 Forged identifiers matrix (headers, bodies, token claims). (RT-AC-002)
- [ ] 2.3 Auth/role abuse: tampered/expired tokens, member→admin endpoints incl. deletion + job inspection. (RT-REQ-001)

## Group 3: Red-team categories (nightly/release)

- [ ] 3.1 Direct + indirect injection suites (incl. hidden text/metadata payloads). (RT-REQ-003, RT-AC-004)
- [ ] 3.2 Cache/shared-state leakage probes (cache keys tenant-inclusive assertion). (RT-AC-003 + design)
- [ ] 3.3 Data-exfiltration checks (no outbound-triggering links/images with private data).
- [ ] 3.4 Retrieval poisoning (keyword-stuffed/look-alike docs) — caps + provenance hold.
- [ ] 3.5 Resource abuse / denial-of-wallet (bombs, floods) — limits + quotas trip early. (RT-AC-005)
- [ ] 3.6 Policy bypass / off-scope — refuses or reports insufficient context.
- [ ] 3.7 Deletion-race scenarios with `013` harness. (RT-AC-006)

## Group 4: Documentation and CI

- [ ] 4.1 `docs/threat-model.md` (attackers × goals × entry points, category mapping). (RT-REQ-004)
- [ ] 4.2 `docs/red-team-report.md` generator + curated findings→guardrails mapping. (RT-REQ-005, RT-AC-007)
- [ ] 4.3 CI wiring (isolation per PR; red-team nightly + release) + OWASP/ATLAS current-version check note. (RT-REQ-006)
