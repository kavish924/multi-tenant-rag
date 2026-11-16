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
worker connect only as that role. RLS covers `memberships` too (it carries `tenant_id`).

`migrations/0003_system_functions.sql` creates `app_system` (`NOLOGIN`, **no** `BYPASSRLS` —
Cloud SQL has no superuser to grant it) and the first system function (TEN-REQ-010/011); later
specs add theirs (`claim_job` in `004`, `outbox_next`/`outbox_mark_published` in `006`,
`sweep_candidates` in `013`), each with a `system_access` policy on the tables it touches.
Migrations run as `migrator` (table owner, member of `app_system` so it can assign function
ownership); `app` is never a member of `app_system`:

```sql
CREATE ROLE app_system NOLOGIN;
GRANT app_system TO migrator;
GRANT SELECT ON memberships TO app_system;
CREATE POLICY system_access ON memberships TO app_system USING (true);

CREATE FUNCTION memberships_for_user(p_user uuid)
RETURNS TABLE (tenant_id uuid, role text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public AS $$
  SELECT m.tenant_id, m.role FROM memberships m WHERE m.user_id = p_user;
$$;
ALTER FUNCTION memberships_for_user(uuid) OWNER TO app_system;
REVOKE ALL ON FUNCTION memberships_for_user(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION memberships_for_user(uuid) TO app;
```

`app_system` gets only the table privileges its functions need (here: `SELECT` on `memberships`).

**Cloud SQL role mapping** (`docs/gcp-environment.md §6`): `app` is a group role granted to the
IAM database users of the `api`, `worker`, and `relay` service accounts; `migrator` is the IAM
user of the migration job's service account. No database passwords exist.

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

**Request authorization (happy):** token verified → user id from `sub` →
`memberships_for_user(sub)` (no tenant context needed) → tenant id from `X-Tenant-ID` (default:
sole membership) must be in that list → role ≥ required → handler proceeds; all DB work via
`WithTenant`.

**Create tenant:** `id := uuid.New()` → `WithTenant(id, ...)`: insert `tenants` row + owner
`memberships` row (passes RLS `WITH CHECK` because `app.tenant_id = id`).

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
  RLS without WHERE (AC-006), pool isolation (AC-007), no-context reads return nothing and the
  system-function allowlist is closed (AC-009/010).

## 10. Open questions

- Default tenant selection when a user belongs to multiple tenants and sends no header →
  **default:** 400 `tenant_required` (explicit is safer than guessing).
