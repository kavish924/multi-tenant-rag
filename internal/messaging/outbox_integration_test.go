//go:build integration

package messaging_test

import (
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/messaging"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
)

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

// recorder is a publisher that remembers every delivery.
type recorder struct {
	mu   sync.Mutex
	got  []messaging.Event
	fail func(messaging.Event) error
}

func (r *recorder) Publish(_ context.Context, ev messaging.Event) error {
	if r.fail != nil {
		if err := r.fail(ev); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ev)
	return nil
}

func (r *recorder) ids() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, len(r.got))
	for i, e := range r.got {
		out[i] = e.ID
	}
	return out
}

func relay(p messaging.Publisher) *messaging.Relay {
	return &messaging.Relay{Pool: env.App, Publisher: p, BatchSize: 1000}
}

func emit(t *testing.T, tenant uuid.UUID, p messaging.Payload) int64 {
	t.Helper()
	var id int64
	err := env.Store.WithTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		var err error
		id, err = messaging.Emit(context.Background(), tx, tenant, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func pendingCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := env.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE published_at IS NULL AND quarantined_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// OBX-AC-001.
func TestEmitCommitAndRollback(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tenant := env.SeedTenant(t, "A", nil)
	doc := uuid.New()

	boom := errors.New("rollback")
	err := env.Store.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := messaging.Emit(ctx, tx, tenant, messaging.DocumentCreated{DocumentID: doc}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || pendingCount(t) != 0 {
		t.Fatalf("rolled-back emit persisted (err %v, rows %d)", err, pendingCount(t))
	}

	id := emit(t, tenant, messaging.DocumentCreated{DocumentID: doc, ContentType: "text/plain", SizeBytes: 5})
	var topic, gotDoc string
	if err := env.Admin.QueryRow(ctx, `SELECT topic, payload->>'document_id' FROM outbox WHERE id = $1`, id).Scan(&topic, &gotDoc); err != nil {
		t.Fatal(err)
	}
	if topic != messaging.TopicDocumentCreated || gotDoc != doc.String() {
		t.Errorf("row %s %s", topic, gotDoc)
	}

	// Forced RLS: a transaction for one tenant cannot emit for another.
	other := env.SeedTenant(t, "B", nil)
	err = env.Store.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := messaging.Emit(ctx, tx, other, messaging.DocumentCreated{DocumentID: doc})
		return err
	})
	if err == nil {
		t.Error("emitted an event for another tenant")
	}
}

// The app role can neither read other tenants' events nor ack rows directly.
func TestOutboxIsolation(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	a, b := env.SeedTenant(t, "A", nil), env.SeedTenant(t, "B", nil)
	emit(t, a, messaging.DocumentDeleting{DocumentID: uuid.New()})
	emit(t, b, messaging.DocumentDeleting{DocumentID: uuid.New()})

	var n int
	_ = env.Store.WithTenant(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n)
	})
	if n != 1 {
		t.Errorf("tenant A sees %d outbox rows, want 1", n)
	}
	if err := env.App.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil || n != 0 {
		t.Errorf("no tenant set: %d rows, err %v", n, err)
	}
	err := env.Store.WithTenant(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now()`)
		return err
	})
	if err == nil {
		t.Error("app role updated outbox directly")
	}
}

// OBX-AC-004 and OBX-AC-002.
func TestRelayPublishesAndRedeliversAfterCrash(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tenant := env.SeedTenant(t, "A", nil)
	first := emit(t, tenant, messaging.DocumentCreated{DocumentID: uuid.New()})
	second := emit(t, tenant, messaging.DocumentIndexed{DocumentID: uuid.New(), Chunks: 2})

	rec := &recorder{}
	r := relay(rec)
	crash := errors.New("crash after publish")
	r.BeforeMark = func(ev messaging.Event) error {
		if ev.ID == second {
			return crash
		}
		return nil
	}
	if _, err := r.Tick(ctx); !errors.Is(err, crash) {
		t.Fatalf("tick: %v", err)
	}
	if pendingCount(t) != 1 {
		t.Fatalf("pending after crash = %d, want 1", pendingCount(t))
	}

	r.BeforeMark = nil
	st, err := r.Tick(ctx)
	if err != nil || st.Published != 1 {
		t.Fatalf("restart tick: %+v %v", st, err)
	}
	if got := rec.ids(); !slices.Equal(got, []int64{first, second, second}) {
		t.Errorf("deliveries %v, want second delivered twice", got)
	}
	if pendingCount(t) != 0 {
		t.Error("rows left unpublished")
	}

	// LogPublisher marks rows published with no broker.
	emit(t, tenant, messaging.DocumentDeleting{DocumentID: uuid.New()})
	if st, err := relay(messaging.LogPublisher{}).Tick(ctx); err != nil || st.Published != 1 || pendingCount(t) != 0 {
		t.Errorf("log publisher: %+v %v", st, err)
	}
}

// OBX-AC-003: a redelivered event has no second side effect.
func TestIdempotentConsumer(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tenant := env.SeedTenant(t, "A", nil)
	emit(t, tenant, messaging.DocumentDeleting{DocumentID: uuid.New()})

	effects := 0
	consume := func(_ context.Context, ev messaging.Event) error {
		return env.Store.WithTenant(ctx, ev.TenantID, func(tx pgx.Tx) error {
			first, err := messaging.Consume(ctx, tx, "test-consumer", ev)
			if err != nil || !first {
				return err
			}
			effects++
			return nil
		})
	}
	rec := &recorder{fail: func(ev messaging.Event) error { return consume(ctx, ev) }}
	r := relay(rec)
	r.BeforeMark = func(messaging.Event) error { return errors.New("crash") }
	_, _ = r.Tick(ctx)
	r.BeforeMark = nil
	if _, err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.ids()) != 2 || effects != 1 {
		t.Errorf("deliveries %d, side effects %d; want 2 and 1", len(rec.ids()), effects)
	}
}

// OBX-AC-005: under concurrent emits, each tenant's events are published in
// emit (id) order, and one relay is active at a time.
func TestRelayOrderingAndSingleLeader(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tenants := []uuid.UUID{env.SeedTenant(t, "A", nil), env.SeedTenant(t, "B", nil), env.SeedTenant(t, "C", nil)}

	var wg sync.WaitGroup
	for _, tid := range tenants {
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					err := env.Store.WithTenant(ctx, tid, func(tx pgx.Tx) error {
						_, err := messaging.Emit(ctx, tx, tid, messaging.DocumentCreated{DocumentID: uuid.New()})
						return err
					})
					if err != nil {
						t.Error(err) // not Fatal: this is a goroutine
						return
					}
				}
			}()
		}
	}
	wg.Wait()

	// One failing row of tenant A holds back A's later rows only.
	var firstA int64
	_ = env.Admin.QueryRow(ctx, `SELECT min(id) FROM outbox WHERE tenant_id = $1`, tenants[0]).Scan(&firstA)
	down := true
	rec := &recorder{fail: func(ev messaging.Event) error {
		if down && ev.ID == firstA {
			return errors.New("broker hiccup")
		}
		return nil
	}}
	r := relay(rec)
	r.BatchSize = 40
	for i := 0; i < 50 && pendingCount(t) > 0; i++ {
		if i == 3 {
			down = false
		}
		if _, err := r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if pendingCount(t) != 0 {
		t.Fatalf("%d rows never published", pendingCount(t))
	}
	last := map[uuid.UUID]int64{}
	rec.mu.Lock()
	for _, ev := range rec.got {
		if ev.ID <= last[ev.TenantID] {
			t.Fatalf("tenant %s: event %d published after %d", ev.TenantID, ev.ID, last[ev.TenantID])
		}
		last[ev.TenantID] = ev.ID
	}
	n := len(rec.got)
	rec.mu.Unlock()
	if n != 300 {
		t.Errorf("published %d events, want 300", n)
	}

	// While one relay holds the lock, another cannot run.
	conn, err := env.App.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(0x6f7574626f78)).Scan(&got); err != nil || !got {
		t.Fatalf("take lock: %v %v", got, err)
	}
	if _, err := relay(rec).Tick(ctx); !errors.Is(err, messaging.ErrNotLeader) {
		t.Errorf("second relay: %v, want ErrNotLeader", err)
	}
	_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(0x6f7574626f78))
}

// Design §7: permanent rejections quarantine the row; later rows flow.
func TestRelayQuarantine(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tenant := env.SeedTenant(t, "A", nil)
	poison := emit(t, tenant, messaging.DocumentCreated{DocumentID: uuid.New()})
	after := emit(t, tenant, messaging.DocumentIndexed{DocumentID: uuid.New()})

	rec := &recorder{fail: func(ev messaging.Event) error {
		if ev.ID == poison {
			return messaging.Permanent(errors.New("schema rejected"))
		}
		return nil
	}}
	var alerts []int64
	r := relay(rec)
	r.MaxAttempts = 2
	r.Quarantined = func(ev messaging.Event, _ error) { alerts = append(alerts, ev.ID) }
	for i := 0; i < 2; i++ {
		if _, err := r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var attempts int
	var quarantined bool
	var lastErr string
	_ = env.Admin.QueryRow(ctx, `SELECT attempts, quarantined_at IS NOT NULL, last_error FROM outbox WHERE id = $1`, poison).
		Scan(&attempts, &quarantined, &lastErr)
	if attempts != 2 || !quarantined || lastErr != "schema rejected" {
		t.Errorf("poison row: attempts %d quarantined %v err %q", attempts, quarantined, lastErr)
	}
	if !slices.Equal(alerts, []int64{poison}) || !slices.Equal(rec.ids(), []int64{after}) {
		t.Errorf("alerts %v deliveries %v", alerts, rec.ids())
	}
	if pendingCount(t) != 0 {
		t.Error("rows still pending")
	}
}

// Run loop: rows emitted while the relay runs are published (OBX-NFR-001
// smoke check; the lag benchmark is task 4.4).
func TestRelayRun(t *testing.T) {
	env.Truncate(t)
	tenant := env.SeedTenant(t, "A", nil)
	rec := &recorder{}
	r := relay(rec)
	r.PollInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	for i := 0; i < 10; i++ {
		emit(t, tenant, messaging.DocumentCreated{DocumentID: uuid.New()})
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.ids()) < 10 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.ids()) != 10 {
		t.Errorf("published %d of 10 within 5s", len(rec.ids()))
	}
}
