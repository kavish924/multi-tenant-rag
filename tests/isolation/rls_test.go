//go:build integration

// Package isolation holds the database-level tenant isolation suite
// (spec 001 Group 5; extended by spec 015). Every assertion runs as the
// application login role, a member of `app` only.
package isolation

import (
	"context"
	"log"
	"os"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
)

// systemFunctions is the closed list from TEN-REQ-010. Later specs append
// claim_job (004), outbox_next / outbox_mark_published / outbox_mark_failed
// (006) and sweep_candidates (013) in the same migration review that adds them.
var systemFunctions = []string{
	"claim_job(text,interval,text[])",
	"memberships_for_user(uuid)",
	"outbox_mark_failed(bigint,text,integer)",
	"outbox_mark_published(bigint)",
	"outbox_next(integer)",
}

// globalTables are the only application tables allowed without forced RLS.
var globalTables = []string{"goose_db_version", "users"}

var env *pgtest.Env

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	e, err := pgtest.Start(ctx)
	cancel()
	if err != nil {
		log.Fatalf("pgtest: %v", err)
	}
	env = e
	code := m.Run()
	env.Close()
	os.Exit(code)
}

type fixture struct {
	tenantA, tenantB uuid.UUID
	userA, userB     uuid.UUID
	shared           uuid.UUID // member of both tenants
}

func seed(t *testing.T) fixture {
	t.Helper()
	env.Truncate(t)
	f := fixture{
		userA:  env.SeedUser(t, "a@a.test"),
		userB:  env.SeedUser(t, "b@b.test"),
		shared: env.SeedUser(t, "shared@x.test"),
	}
	f.tenantA = env.SeedTenant(t, "A", map[uuid.UUID]string{f.userA: "owner", f.shared: "member"})
	f.tenantB = env.SeedTenant(t, "B", map[uuid.UUID]string{f.userB: "owner", f.shared: "admin"})
	return f
}

// TEN-AC-006: a query with no WHERE tenant_id under a tenant context still
// sees only that tenant's rows, and cannot write another tenant's rows.
func TestRLSRestrictsQueriesWithoutWhere(t *testing.T) {
	f := seed(t)
	ctx := context.Background()

	err := env.Store.WithTenant(ctx, f.tenantA, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("memberships visible under A = %d, want 2", n)
		}
		rows, err := tx.Query(ctx, `SELECT id FROM tenants`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != f.tenantA {
			t.Errorf("tenants visible under A = %v, want [%s]", ids, f.tenantA)
		}
		// An UPDATE with no WHERE only touches A's rows.
		_, err = tx.Exec(ctx, `UPDATE memberships SET role = 'member'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var bOwners int
	if err := env.Admin.QueryRow(ctx,
		`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'`, f.tenantB).Scan(&bOwners); err != nil {
		t.Fatal(err)
	}
	if bOwners != 1 {
		t.Errorf("unscoped UPDATE under A changed tenant B (owners = %d)", bOwners)
	}

	// WITH CHECK: writing a row for B under A's context fails.
	err = env.Store.WithTenant(ctx, f.tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`, f.tenantB, f.userA)
		return err
	})
	if err == nil {
		t.Error("insert of tenant B row under tenant A context succeeded")
	}
}

// TEN-AC-007: a pooled connection does not keep the previous tenant.
func TestPoolReuseDoesNotLeakTenant(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	pool, err := env.NewAppPool(ctx, 1) // one connection: reuse is guaranteed
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var pidIn int
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantA.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pidIn)
	})
	if err != nil {
		t.Fatal(err)
	}

	var pidOut, n int
	var setting *string
	if err := pool.QueryRow(ctx,
		`SELECT pg_backend_pid(), current_setting('app.tenant_id', true), (SELECT count(*) FROM memberships)`).
		Scan(&pidOut, &setting, &n); err != nil {
		t.Fatalf("query after tenant tx (the policy must not fail on an empty setting): %v", err)
	}
	if pidIn != pidOut {
		t.Fatalf("connection not reused (pid %d vs %d); test is not meaningful", pidIn, pidOut)
	}
	if setting != nil && *setting != "" {
		t.Errorf("app.tenant_id leaked into next use: %q", *setting)
	}
	if n != 0 {
		t.Errorf("memberships visible after tenant tx = %d, want 0", n)
	}
}

// TEN-AC-009 (part 1): every application table is either explicitly global
// or has RLS enabled and forced, and `app` with no tenant context reads zero
// rows from each RLS table even though the tables hold data.
func TestNoTenantContextReadsNothing(t *testing.T) {
	seed(t)
	ctx := context.Background()

	rows, err := env.Admin.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
		ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	type tbl struct {
		name          string
		rls, forceRLS bool
	}
	tables, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tbl, error) {
		var x tbl
		err := r.Scan(&x.name, &x.rls, &x.forceRLS)
		return x, err
	})
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, tb := range tables {
		if slices.Contains(globalTables, tb.name) {
			continue
		}
		if !tb.rls || !tb.forceRLS {
			t.Errorf("table %s: RLS enabled=%v forced=%v; tenant-owned tables need both", tb.name, tb.rls, tb.forceRLS)
			continue
		}
		var total, visible int
		q := `SELECT count(*) FROM ` + pgx.Identifier{tb.name}.Sanitize()
		if err := env.Admin.QueryRow(ctx, q).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if err := env.App.QueryRow(ctx, q).Scan(&visible); err != nil {
			t.Fatalf("%s as app: %v", tb.name, err)
		}
		if visible != 0 {
			t.Errorf("table %s: app sees %d rows with no tenant context, want 0", tb.name, visible)
		}
		if total == 0 {
			t.Logf("table %s has no seed rows; zero-row check is vacuous", tb.name)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no RLS tables found")
	}
}

// TEN-AC-009 (part 2): the cross-tenant surface is closed.
func TestSystemFunctionSurfaceIsClosed(t *testing.T) {
	ctx := context.Background()

	rows, err := env.Admin.Query(ctx, `
		SELECT p.oid::regprocedure::text
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE p.prosecdef
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND has_function_privilege('app', p.oid, 'EXECUTE')`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := slices.Clone(systemFunctions)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("SECURITY DEFINER functions executable by app = %v, want exactly %v", got, want)
	}

	// Each system function is owned by app_system and pins search_path with
	// pg_temp last.
	rows, err = env.Admin.Query(ctx, `
		SELECT p.oid::regprocedure::text, pg_get_userbyid(p.proowner), coalesce(array_to_string(p.proconfig, ','), '')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE p.prosecdef AND n.nspname = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var fn, owner, conf string
		if err := rows.Scan(&fn, &owner, &conf); err != nil {
			t.Fatal(err)
		}
		if owner != "app_system" {
			t.Errorf("%s owned by %s, want app_system", fn, owner)
		}
		if conf != "search_path=pg_catalog, public, pg_temp" {
			t.Errorf("%s config %q, want search_path=pg_catalog, public, pg_temp", fn, conf)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	var appInSystem, loginInSystem bool
	if err := env.Admin.QueryRow(ctx,
		`SELECT pg_has_role('app', 'app_system', 'MEMBER'), pg_has_role($1, 'app_system', 'MEMBER')`,
		pgtest.AppLogin).Scan(&appInSystem, &loginInSystem); err != nil {
		t.Fatal(err)
	}
	if appInSystem || loginInSystem {
		t.Errorf("app or its login is a member of app_system (app=%v login=%v)", appInSystem, loginInSystem)
	}

	// No non-superuser role may hold BYPASSRLS (Cloud SQL cannot grant it,
	// and nothing here may depend on it). The test container's bootstrap
	// superuser is the only role allowed to bypass RLS.
	rows, err = env.Admin.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolbypassrls AND NOT rolsuper`)
	if err != nil {
		t.Fatal(err)
	}
	bypass, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(bypass) != 0 {
		t.Errorf("roles with BYPASSRLS: %v", bypass)
	}

	var appSuper, appBypass bool
	if err := env.App.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&appSuper, &appBypass); err != nil {
		t.Fatal(err)
	}
	if appSuper || appBypass {
		t.Errorf("application login is superuser=%v bypassrls=%v", appSuper, appBypass)
	}

	if _, err := env.App.Exec(ctx, `SET ROLE app_system`); err == nil {
		_, _ = env.App.Exec(ctx, `RESET ROLE`)
		t.Error("application login can SET ROLE app_system")
	}
}

// TEN-AC-010: memberships_for_user returns only the given user's rows.
func TestMembershipsForUser(t *testing.T) {
	f := seed(t)
	ctx := context.Background()

	cases := map[uuid.UUID][]uuid.UUID{
		f.userA:    {f.tenantA},
		f.userB:    {f.tenantB},
		f.shared:   {f.tenantA, f.tenantB},
		uuid.New(): nil,
	}
	for u, want := range cases {
		rows, err := env.App.Query(ctx, `SELECT tenant_id FROM memberships_for_user($1)`, u)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			t.Fatal(err)
		}
		sortIDs(got)
		sortIDs(want)
		if !slices.Equal(got, want) {
			t.Errorf("memberships_for_user(%s) = %v, want %v", u, got, want)
		}
	}
}

// A caller must not be able to shadow `memberships` with a temp table to
// forge results from the SECURITY DEFINER function (search_path pins
// pg_temp last and the function schema-qualifies its tables).
func TestSystemFunctionIgnoresTempTableShadowing(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	conn, err := env.App.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	defer func() { _, _ = conn.Exec(ctx, `DISCARD TEMP`) }()

	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE memberships (tenant_id uuid, user_id uuid, role text)`); err != nil {
		t.Skipf("app cannot create temp tables (attack not possible): %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO pg_temp.memberships VALUES ($1, $2, 'owner')`, f.tenantB, f.userA); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT tenant_id FROM memberships_for_user($1)`, f.userA)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != f.tenantA {
		t.Errorf("temp table shadowing changed results: %v", got)
	}
}

func sortIDs(ids []uuid.UUID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
}
