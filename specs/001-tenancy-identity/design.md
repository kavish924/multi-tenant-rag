# 001 — Tenancy & Identity: Design

- **Spec ID:** 001 `TEN` · **Milestone:** 1
- **Shared design:** [system-design.md §1 Multi-tenancy](../../docs/system-design.md#1-multi-tenancy-model)

## 1. Context and goals

Everything else composes on top of "user + tenant + role". This spec establishes the identity
model, the middleware chain, and the transaction-scoped tenant context (`WithTenant`) that API
and workers both use. It is the first layer of the three-layer defense (app authorization).

## 2. Scope

**In:** users, tenants, memberships, JWT auth, middleware chain, RLS migration, `WithTenant`.
**Out:** object storage keys (`003`), per-document ACLs, invitations/email, refresh tokens.

## 3. Components

- `internal/auth` — password hashing (argon2id), JWT sign/verify, `auth.UserFrom(ctx)`.
- `internal/tenants` — tenant + membership service; `RequireRole` helpers.
- `internal/storage` — `Store` (pgx pool), `WithTenant(ctx, tenantID, fn)`; dedicated app role DSN.
- `cmd/api` middleware chain: `Authenticate` → `ResolveTenant` → `Authorize(role)`.

## 4. Data model changes

`migrations/0001_init.sql` (tenant tables portion) + `migrations/0002_rls.sql`:

```sql
CREATE TABLE tenants (id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
                      name text NOT NULL,
                      created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE users   (id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
                      email text NOT NULL UNIQUE,
                      password_hash text NOT NULL);
CREATE TABLE memberships (
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  user_id   uuid NOT NULL REFERENCES users(id),
  role      text NOT NULL CHECK (role IN ('owner','admin','member')),
  PRIMARY KEY (tenant_id, user_id));

ALTER TABLE documents ENABLE ROW LEVEL SECURITY;  -- and every tenant-owned table
ALTER TABLE documents FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON documents
  USING      (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
```

A dedicated `app` DB role (no superuser, non-owner) is created in `0002_rls.sql`; the API and
worker connect only as that role.

## 5. API contract

| Method | Path | Auth | Min role | Purpose |
| ------ | ---- | ---- | -------- | ------- |
| POST | `/auth/register` | — | — | Create user (email, password) |
| POST | `/auth/login` | — | — | Issue JWT |
| GET | `/me` | JWT | member | User + membership list |
| POST | `/tenants` | JWT | member | Create tenant (creator becomes owner) |
| GET | `/tenants/{id}/members` | JWT | admin | List members |
| POST | `/tenants/{id}/members` | JWT | admin | Add member (email, role) |
| PATCH | `/tenants/{id}/members/{uid}` | JWT | owner | Change role |
| DELETE | `/tenants/{id}/members/{uid}` | JWT | owner | Remove member (owner cannot be removed if last) |

Errors: `401` unauthenticated/expired, `403` insufficient role, `404` non-member tenant
(existence hidden), JSON error envelope `{"error": {"code", "message"}}`.

## 6. Key flows

**Request authorization (happy):** token verified → user id from `sub` → tenant id from
`X-Tenant-ID` (default: sole membership) → membership lookup (user, tenant) → role ≥ required →
handler proceeds; all DB work via `WithTenant`.

**Failure:** expired token → 401 before any lookup; no membership → 404; insufficient role → 403.

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Expired/tampered token | JWT verify | 401 |
| Unknown tenant / not a member | membership query empty | 404 |
| Role too low | `Authorize` check | 403 |
| DB pool connection carries stale tenant | impossible: `set_config(..., true)` is tx-local; isolation test guards | — |

## 8. Security and multi-tenancy

This spec **is** layer 1 (application authorization) and sets up layer 2 (RLS). Layer 3
(storage prefixes) is specified in `003`. Forged tenant identifiers are never trusted
(TEN-AC-005); RLS is the backstop for application bugs (TEN-AC-006/007).

## 9. Testing strategy

- Unit: JWT round-trip, password hash/verify, role checks, middleware ordering.
- Integration: register/login/me; membership CRUD; role matrix.
- Security (feeds `015`): cross-tenant access (TEN-AC-003/008), forged tenant id (AC-005),
  RLS without WHERE (AC-006), pool isolation (AC-007).

## 10. Open questions

- Default tenant selection when a user belongs to multiple tenants and sends no header →
  **default:** 400 `tenant_required` (explicit is safer than guessing).
