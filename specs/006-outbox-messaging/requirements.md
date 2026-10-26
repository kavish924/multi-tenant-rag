# 006 — Outbox Messaging: Requirements

- **Spec ID:** 006 `OBX`
- **Status:** Draft
- **Milestone:** 2 — Async ingestion
- **Depends on:** `specs/004-job-queue`
- **Shared context:** `docs/system-design.md §4` (when you add a broker)

## Overview

Transactional outbox: state-changing transactions write an event row atomically; a relay
publishes rows to a broker. Delivery is at-least-once, so consumers are idempotent by contract
(idempotency keys + unique constraints). Until a real broker is justified, the relay ships with
a no-op/log publisher so the pattern is in place.

## User stories

- **OBX-US-1:** As a developer, I want to emit events in the same transaction as state changes so events never lie about committed state.
- **OBX-US-2:** As a platform operator, I want duplicate deliveries to be harmless so at-least-once delivery never corrupts consumers.
- **OBX-US-3:** As a developer, I want to add a real broker later without touching producers.

## Requirements

### Functional

- **OBX-REQ-001:** `Outbox.Emit(ctx, tx, topic, tenantID, payload)` writes an `outbox` row inside the caller's transaction.
- **OBX-REQ-002:** Topics (initial): `document.created`, `document.indexed`, `document.failed`, `document.deleting`.
- **OBX-REQ-003:** `cmd/outbox-relay` polls unpublished rows (`published_at IS NULL`) in `id` order, publishes via a `Publisher` interface, then marks `published_at` — one row at a time, at-least-once.
- **OBX-REQ-004:** `Publisher` implementations: `LogPublisher` (default, no broker) and a placeholder NATS/Kafka impl when justified.
- **OBX-REQ-005:** Event payload carries: `event_id` (outbox row id), `topic`, `tenant_id`, `occurred_at`, and topic-specific ids (e.g. `document_id`) — never document text.
- **OBX-REQ-006:** Consumers MUST be idempotent: dedupe on `event_id`/natural keys with unique constraints; spec `013` cleanup consumer demonstrates the pattern.
- **OBX-REQ-007:** Relay crashes between publish and mark ⇒ redelivery on restart; consumers tolerate it.

### Non-functional

- **OBX-NFR-001:** Relay lag p95 < 5 s under normal load; batch size and poll interval configurable.

## Acceptance criteria (EARS)

- **OBX-AC-001:** WHEN a state change and `Emit` share a transaction, THE SYSTEM SHALL persist both or neither.
- **OBX-AC-002:** WHEN the relay publishes a row and crashes before marking, THE SYSTEM SHALL publish it again on restart (at-least-once proof).
- **OBX-AC-003:** WHEN a consumer receives a duplicate `event_id`, THE SYSTEM SHALL process it exactly once effectively (no duplicate side effects).
- **OBX-AC-004:** WHEN no broker is configured, THE SYSTEM SHALL run with `LogPublisher` and mark rows published.
- **OBX-AC-005:** WHEN the relay publishes, THE SYSTEM SHALL preserve per-tenant ordering per topic (single-flight per partition key = tenant).

## Out of scope

- Consumer framework/DLQ UI; schema registry; exactly-once delivery (impossible; idempotence instead).
