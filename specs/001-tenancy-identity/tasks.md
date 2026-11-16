# 001 — Tenancy & Identity: Tasks

## Group 1: Schema and database

- [ ] 1.1 `migrations/0001_init.sql` — tenants, users (with `password_hash`), memberships; role CHECK constraint. (TEN-REQ-001, TEN-REQ-004)
- [ ] 1.2 `migrations/0002_rls.sql` — enable + FORCE RLS on tenant-owned tables; `tenant_isolation` policy; create dedicated `app` role; grant only needed privileges. (TEN-REQ-008)
- [ ] 1.3 `internal/storage`: pgx pool wired to the `app` role; `WithTenant(ctx, tenantID, fn)` using `set_config('app.tenant_id', $1, true)` inside a transaction. (TEN-REQ-007)
- [ ] 1.4 `migrations/0003_system_functions.sql` — `app_system` role (`NOLOGIN`, no `BYPASSRLS`), `system_access` policy on `memberships` scoped `TO app_system`, `memberships_for_user`, revoke from PUBLIC, grant EXECUTE to `app`. (TEN-REQ-010, TEN-REQ-011)
- [ ] 1.5 Cloud SQL role mapping in Terraform: `app` group role granted to the IAM DB users of `sa-api`/`sa-worker`/`sa-relay`; `migrator` for `sa-migrate`. (TEN-REQ-008, `docs/gcp-environment.md §6`)

## Group 2: Identity service

- [ ] 2.1 `internal/auth`: argon2id hash/verify; JWT issue/verify (HS256 or EdDSA; key from config). (TEN-REQ-001, TEN-REQ-002, TEN-NFR-002)
- [ ] 2.2 `POST /auth/register`, `POST /auth/login`, `GET /me` handlers. (TEN-REQ-001, TEN-REQ-002)
- [ ] 2.3 `internal/tenants`: create tenant (creator → owner, inside `WithTenant(newID)`), membership CRUD with role transitions. (TEN-REQ-003, TEN-REQ-004, TEN-REQ-011)

## Group 3: Middleware chain

- [ ] 3.1 `Authenticate` middleware (401 paths). (TEN-REQ-005, TEN-AC-001, TEN-AC-002)
- [ ] 3.2 `ResolveTenant` middleware: `memberships_for_user` + header verification; 404 for non-members; 400 when ambiguous with no header. (TEN-REQ-006, TEN-REQ-011, TEN-AC-003, TEN-AC-005)
- [ ] 3.3 `Authorize(minRole)` middleware: 403 matrix. (TEN-REQ-005, TEN-AC-004)
- [ ] 3.4 Route table protected by the chain; JSON error envelope helper.

## Group 4: Worker tenancy context

- [ ] 4.1 Worker `Process` wraps all pipeline DB work in `WithTenant(job.TenantID, ...)`. (TEN-REQ-009)

## Group 5: Tests and verification

- [ ] 5.1 Unit: JWT round-trip incl. expiry; password hash/verify; role comparisons.
- [ ] 5.2 Integration: register/login/me; membership CRUD per role matrix.
- [ ] 5.3 Security: query-without-WHERE under tenant context → restricted rows (TEN-AC-006); pool reuse isolation (TEN-AC-007); same-id cross-tenant fetch → only own data (TEN-AC-008).
- [ ] 5.4 Security: `app` with no tenant context reads zero rows from every tenant-owned table; enumerate functions `app` can EXECUTE as `SECURITY DEFINER` and fail on any not in the TEN-REQ-010 list; fail if `app` is a member of `app_system` or any role has `BYPASSRLS`. (TEN-AC-009, TEN-AC-010)
- [ ] 5.5 **Milestone 1 exit contribution:** tenant-scoped API tests pass (TEN-AC-001…010 green).
