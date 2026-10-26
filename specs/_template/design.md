# Design — <Feature name>

- **Spec ID:** 0XX `<PREFIX>`
- **Status:** Draft (design for approved requirements)
- **Shared design:** [system-design.md](../../docs/system-design.md),
  [system-architecture.md](../../docs/system-architecture.md)

## 1. Context and goals

Why this feature exists in the architecture; which problem from the requirements it solves.

## 2. Scope

**In:** ...
**Out:** ... (and which spec covers it)

## 3. Components

New/changed packages under `internal/...`, binaries, and their responsibilities.

## 4. Data model changes

Migration files, DDL deltas, indexes, constraints. Reference shared schema decisions instead
of duplicating them.

## 5. API contract

Endpoints, request/response shapes, status codes, error envelope. Add to `api/openapi.yaml`.

## 6. Key flows

Step-by-step behavior for the main scenarios (happy path + one failure path each).

## 7. Error handling and failure modes

Failure → detection → response table. Include retry/lease/idempotency semantics where relevant.

## 8. Security and multi-tenancy

How the three isolation layers apply here; what is untrusted; which guard layer enforces what.

## 9. Testing strategy

Which tests from the four families (unit / integration / security / failure & load) verify
each acceptance criterion.

## 10. Open questions

Unresolved decisions, with a default if unresolved at implementation time.
