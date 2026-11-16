-- One-time Cloud SQL setup, run once per environment after
-- `terraform apply` in deployments/terraform/env/<env> and before the first
-- migrate job. Cloud SQL IAM users cannot create roles, so the built-in
-- `postgres` user creates the two group roles and hands them to `migrator`.
--
-- How: Cloud Console → SQL → <env>-pg → Cloud SQL Studio, database `app`,
-- user `postgres` (set its password first in Cloud Shell:
--   gcloud sql users set-password postgres --instance=<env>-pg --prompt-for-password).
-- Replace PROJECT_ID below (the IAM user is the migrate service account
-- email without ".gserviceaccount.com"; `terraform output db_users` prints it).

CREATE ROLE app        NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
CREATE ROLE app_system NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;

-- ADMIN OPTION lets migrations run GRANT app_system TO CURRENT_USER and let
-- cmd/migrate add the api/worker/relay IAM users to `app` (APP_DB_LOGIN_ROLES).
GRANT app        TO "sa-migrate@PROJECT_ID.iam" WITH ADMIN OPTION;
GRANT app_system TO "sa-migrate@PROJECT_ID.iam" WITH ADMIN OPTION;

-- migrator creates and owns the schema objects.
GRANT USAGE, CREATE ON SCHEMA public TO "sa-migrate@PROJECT_ID.iam";

-- The pgvector extension (needed from spec 005) is created by the
-- cloudsqlsuperuser-backed `postgres` user; check the version here
-- (iterative scans need >= 0.8, spec 007 RET-REQ-007).
CREATE EXTENSION IF NOT EXISTS vector;
SELECT extversion FROM pg_extension WHERE extname = 'vector';
