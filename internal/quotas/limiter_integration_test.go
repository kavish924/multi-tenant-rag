//go:build integration

package quotas_test

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/quotas"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
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

func limiter(def quotas.Limits) *quotas.Limiter {
	l := quotas.New(env.Store, def)
	l.CacheTTL = -1 // read overrides on every call
	return l
}

func override(t *testing.T, tenant uuid.UUID, col string, v int64) {
	t.Helper()
	if _, err := env.Admin.Exec(context.Background(),
		`INSERT INTO tenant_quotas (tenant_id, `+col+`) VALUES ($1, $2)
		 ON CONFLICT (tenant_id) DO UPDATE SET `+col+` = EXCLUDED.`+col, tenant, v); err != nil {
		t.Fatal(err)
	}
}

func exceeded(t *testing.T, err error, code string) *quotas.Exceeded {
	t.Helper()
	var ex *quotas.Exceeded
	if !errors.As(err, &ex) || ex.Code != code {
		t.Fatalf("err %v, want %s", err, code)
	}
	return ex
}

// QTA-AC-001/002 at the limiter: counts stop at the limit with a Retry-After
// to the window end; Release refunds.
func TestReserveAndRefund(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	def := quotas.Defaults
	def.UploadsDay = 2
	l := limiter(def)

	r1, err := l.Reserve(ctx, tid, quotas.DimUploads, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(ctx, tid, quotas.DimUploads, 1); err != nil {
		t.Fatal(err)
	}
	ex := exceeded(t, func() error { _, err := l.Reserve(ctx, tid, quotas.DimUploads, 1); return err }(), quotas.CodeUploads)
	if ex.Limit != 2 || ex.Used != 2 || ex.RetryAfter <= 0 || ex.RetryAfter > 24*time.Hour {
		t.Errorf("exceeded %+v", ex)
	}
	r1.Release(ctx)
	r1.Release(ctx) // idempotent
	if _, err := l.Reserve(ctx, tid, quotas.DimUploads, 1); err != nil {
		t.Errorf("refund did not free a unit: %v", err)
	}
	// Another tenant is unaffected.
	if _, err := l.Reserve(ctx, env.SeedTenant(t, "B", nil), quotas.DimUploads, 1); err != nil {
		t.Error(err)
	}
	// A zero limit admits nothing.
	override(t, tid, "questions_day", 0)
	exceeded(t, func() error { _, err := l.Reserve(ctx, tid, quotas.DimQuestions, 1); return err }(), quotas.CodeQuestions)
}

// QTA-AC-005: 100 concurrent reservations at 1 remaining admit exactly one
// (zero tolerance for counts).
func TestReserveRace(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	def := quotas.Defaults
	def.QuestionsDay = 10
	l := limiter(def)
	for i := 0; i < 9; i++ {
		if _, err := l.Reserve(ctx, tid, quotas.DimQuestions, 1); err != nil {
			t.Fatal(err)
		}
	}
	var ok, denied atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.Reserve(ctx, tid, quotas.DimQuestions, 1)
			var ex *quotas.Exceeded
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &ex):
				denied.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || denied.Load() != 99 {
		t.Errorf("admitted %d, denied %d; want 1 and 99", ok.Load(), denied.Load())
	}
}

// QTA-AC-003: projected tokens over the remaining budget are rejected.
func TestCheckTokens(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	override(t, tid, "generate_tokens_month", 10_000)
	l := limiter(quotas.Defaults)
	if err := l.CheckTokens(ctx, tid, quotas.DimGenerateTokens, 8_000); err != nil {
		t.Fatal(err)
	}
	rec := usage.NewRecorder(env.Store)
	if err := rec.Record(ctx, usage.Event{TenantID: tid, Kind: usage.KindGenerate, Model: "m", InputTokens: 2_000, OutputTokens: 500}); err != nil {
		t.Fatal(err)
	}
	ex := exceeded(t, l.CheckTokens(ctx, tid, quotas.DimGenerateTokens, 8_000), quotas.CodeTokens)
	if ex.Used != 2_500 || ex.Limit != 10_000 {
		t.Errorf("exceeded %+v", ex)
	}
}

// QTA-AC-004: the concurrent_asks cap is exact across callers, slots free
// on release, and expired slots do not count.
func TestAskSlots(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	def := quotas.Defaults
	def.ConcurrentAsks = 3
	l := limiter(def)

	var got atomic.Int32
	var slots sync.Map
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := l.AcquireAskSlot(ctx, tid)
			if err == nil {
				got.Add(1)
				slots.Store(i, s)
			}
		}(i)
	}
	wg.Wait()
	if got.Load() != 3 {
		t.Fatalf("acquired %d slots, cap 3", got.Load())
	}
	exceeded(t, func() error { _, err := l.AcquireAskSlot(ctx, tid); return err }(), quotas.CodeConcurrency)
	slots.Range(func(_, v any) bool { v.(*quotas.Slot).Release(ctx); return false }) // free one
	if _, err := l.AcquireAskSlot(ctx, tid); err != nil {
		t.Errorf("released slot not reusable: %v", err)
	}
	if _, err := env.Admin.Exec(ctx, `UPDATE ask_slots SET expires_at = now() - interval '1 second' WHERE tenant_id = $1`, tid); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AcquireAskSlot(ctx, tid); err != nil {
		t.Errorf("expired slots still counted: %v", err)
	}
}

// QTA-AC-007: an override applies on the next request without redeploy
// (immediately with no cache, within the TTL with the default cache).
func TestOverrideHotApply(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	now := time.Now()
	l := quotas.New(env.Store, quotas.Defaults)
	l.Now = func() time.Time { return now }
	if lim, _ := l.Effective(ctx, tid); lim.QuestionsDay != 1000 {
		t.Fatalf("default %d", lim.QuestionsDay)
	}
	five := int64(5)
	if err := quotas.SetOverrides(ctx, env.Store, tid, quotas.Overrides{QuestionsDay: &five}); err == nil {
		t.Error("the app role must not be able to write overrides")
	}
	override(t, tid, "questions_day", 5)
	if lim, _ := l.Effective(ctx, tid); lim.QuestionsDay != 1000 {
		t.Errorf("cache ignored within TTL: %d", lim.QuestionsDay)
	}
	now = now.Add(quotas.DefaultCacheTTL + time.Millisecond)
	if lim, _ := l.Effective(ctx, tid); lim.QuestionsDay != 5 {
		t.Errorf("override not applied after TTL: %d", lim.QuestionsDay)
	}
}

// QTA-AC-008: the report's token counters equal usage_events aggregates.
func TestReportMatchesUsageEvents(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := env.SeedTenant(t, "A", nil)
	rec := usage.NewRecorder(env.Store)
	events := []usage.Event{
		{Kind: usage.KindEmbed, Model: "text-embedding-005", InputTokens: 1200},
		{Kind: usage.KindGenerate, Model: "gemini-2.5-flash", InputTokens: 7000, OutputTokens: 300},
		{Kind: usage.KindRerank, Model: "gemini-2.5-flash", InputTokens: 900, OutputTokens: 20},
		{Kind: usage.KindGenerate, Model: "gemini-2.5-flash", InputTokens: 50, Outcome: usage.OutcomeCancelled},
	}
	for _, e := range events {
		e.TenantID = tid
		if err := rec.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := limiter(quotas.Defaults).Report(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	var embed, gen int64
	if err := env.Admin.QueryRow(ctx, `
		SELECT coalesce(sum(input_tokens + output_tokens) FILTER (WHERE kind = 'embed'), 0),
		       coalesce(sum(input_tokens + output_tokens) FILTER (WHERE kind IN ('generate', 'rerank')), 0)
		FROM usage_events WHERE tenant_id = $1`, tid).Scan(&embed, &gen); err != nil {
		t.Fatal(err)
	}
	if rep.Dimensions[quotas.DimEmbedTokens].Used != embed || rep.Dimensions[quotas.DimGenerateTokens].Used != gen || gen != 8270 {
		t.Errorf("report %+v vs usage_events embed=%d generate=%d", rep.Dimensions, embed, gen)
	}
	if rep.EstimatedCostUSD <= 0 || rep.Dimensions[quotas.DimQuestions].Limit != 1000 {
		t.Errorf("cost %v limits %+v", rep.EstimatedCostUSD, rep.Dimensions)
	}
}
