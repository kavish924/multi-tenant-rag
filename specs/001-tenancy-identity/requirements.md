# 001 — Tenancy & Identity: Requirements

- **Spec ID:** 001 `TEN`
- **Status:** Draft
- **Milestone:** 1 — Working Go API
- **Depends on:** none
- **Shared context:** `docs/system-design.md §1` (multi-tenancy model)

## Overview

Provides authenticated users, tenants, and role-based memberships — the foundation every other
feature uses to resolve "who is asking, on behalf of which tenant". Includes the three-layer
isolation foundation: application authorization, row-level security, and the transaction-scoped
tenant context used by both API and workers.

## User stories

- **TEN-US-1:** As a platform operator, I want to create tenants so that new customers can onboard.
- **TEN-US-2:** As a tenant owner, I want to add/remove members and assign roles so that access matches my team.
- **TEN-US-3:** As a tenant member, I want my requests automatically scoped to my tenant so that I never see another customer's data.
- **TEN-US-4:** As a developer, I want the database to enforce tenant isolation (RLS) so that a forgotten `WHERE` clause cannot leak data.

## Requirements

### Functional

- **TEN-REQ-001:** Users register with email + password; passwords hashed with a modern KDF (argon2id or bcrypt).
- **TEN-REQ-002:** Login issues a signed JWT access token carrying the user id (`sub`) and expiry.
- **TEN-REQ-003:** Tenants have owners; owners can create/modify/remove memberships within their tenant.
- **TEN-REQ-004:** Membership roles are `owner`, `admin`, `member` (CHECK constraint; no others).
- **TEN-REQ-005:** A middleware chain on every protected route: Authenticate → ResolveTenant → Authorize(minRole).
- **TEN-REQ-006:** The tenant for a request is resolved from the token + verified membership; a tenant selector (e.g. `X-Tenant-ID` header) is only honored **after** membership verification.
- **TEN-REQ-007:** Every tenant-scoped DB operation runs through `Store.WithTenant(...)`, which sets `app.tenant_id` with `set_config(..., is_local => true)` inside the transaction.
- **TEN-REQ-008:** RLS is enabled and forced (`FORCE ROW LEVEL SECURITY`) on tenants-owned tables, with a dedicated non-superuser application role.
- **TEN-REQ-009:** Workers run pipeline DB work through the same `WithTenant` wrapper.

### Non-functional

- **TEN-NFR-001:** Auth middleware adds < 5 ms p95 overhead excluding DB lookups.
- **TEN-NFR-002:** Tokens are short-lived (≤ 15 min); refresh is out of scope until a client needs it (demo client re-logs in).

## Acceptance criteria (EARS)

- **TEN-AC-001:** WHEN a request arrives without a valid token, THE SYSTEM SHALL respond 401 and process no tenant data.
- **TEN-AC-002:** IF a token is expired, malformed, or tampered, THEN THE SYSTEM SHALL respond 401.
- **TEN-AC-003:** WHEN an authenticated user requests a tenant they hold no membership in, THE SYSTEM SHALL respond 404 (existence hidden) and return no data.
- **TEN-AC-004:** WHEN a member with role `member` calls an owner/admin-only endpoint, THE SYSTEM SHALL respond 403.
- **TEN-AC-005:** WHEN a client supplies a forged tenant identifier (header, query, or body), THE SYSTEM SHALL ignore it and use only the verified membership tenant.
- **TEN-AC-006:** WHEN a query is executed without its `WHERE tenant_id` clause under a tenant context, RLS SHALL still restrict rows to the current tenant (isolation test).
- **TEN-AC-007:** WHEN a pooled connection is reused after a tenant transaction, it SHALL NOT retain the previous tenant setting (isolation test).
- **TEN-AC-008:** WHEN two users in different tenants request the same resource id, THE SYSTEM SHALL return data only for the requester's tenant.

## Out of scope

- SSO/OIDC, MFA, refresh tokens, invitations by email (future spec).
- Per-document ACLs (guardrail layers may add later; tenant boundary only for now).
