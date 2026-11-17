package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeSource is an in-memory outbox with the system functions' semantics.
type fakeSource struct {
	rows        []*fakeRow
	nextErr     error
	markErr     error
	maxAttempts []int
}

type fakeRow struct {
	pending
	published, quarantined bool
}

func (f *fakeSource) add(tenant uuid.UUID, topic string) int64 {
	id := int64(len(f.rows) + 1)
	f.rows = append(f.rows, &fakeRow{pending: pending{Event: Event{
		ID: id, TenantID: tenant, Topic: topic, OccurredAt: time.Now(), Data: json.RawMessage(`{}`),
	}}})
	return id
}

func (f *fakeSource) next(_ context.Context, limit int) ([]pending, error) {
	if f.nextErr != nil {
		return nil, f.nextErr
	}
	var out []pending
	for _, r := range f.rows {
		if !r.published && !r.quarantined && len(out) < limit {
			out = append(out, r.pending)
		}
	}
	return out, nil
}

func (f *fakeSource) row(id int64) *fakeRow { return f.rows[id-1] }

func (f *fakeSource) markPublished(_ context.Context, id int64) (bool, error) {
	if f.markErr != nil {
		return false, f.markErr
	}
	r := f.row(id)
	if r.published || r.quarantined {
		return false, nil
	}
	r.published = true
	return true, nil
}

func (f *fakeSource) markFailed(_ context.Context, id int64, _ string, maxAttempts int) (bool, error) {
	f.maxAttempts = append(f.maxAttempts, maxAttempts)
	r := f.row(id)
	r.attempts++
	r.quarantined = r.attempts >= maxAttempts
	return r.quarantined, nil
}

// scriptPub fails events whose id is in fail (permanently if listed in perm).
type scriptPub struct {
	got  []int64
	fail map[int64]bool
	perm map[int64]bool
}

func (p *scriptPub) Publish(_ context.Context, ev Event) error {
	if p.fail[ev.ID] {
		if p.perm[ev.ID] {
			return Permanent(fmt.Errorf("rejected %d", ev.ID))
		}
		return fmt.Errorf("broker down for %d", ev.ID)
	}
	p.got = append(p.got, ev.ID)
	return nil
}

func TestBatchPublishesInIDOrder(t *testing.T) {
	src := &fakeSource{}
	a, b := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		src.add(a, TopicDocumentCreated)
		src.add(b, TopicDocumentIndexed)
	}
	pub := &scriptPub{}
	r := &Relay{Publisher: pub}
	st, err := r.batch(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fetched != 6 || st.Published != 6 || !slices.Equal(pub.got, []int64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("stats %+v order %v", st, pub.got)
	}
	if st, _ = r.batch(context.Background(), src); st.Fetched != 0 {
		t.Errorf("published rows fetched again: %+v", st)
	}
}

// OBX-AC-005: a failed row holds back its tenant's later rows, not others'.
func TestBatchHoldsBackTenantAfterFailure(t *testing.T) {
	src := &fakeSource{}
	a, b := uuid.New(), uuid.New()
	src.add(a, TopicDocumentCreated) // 1 fails
	src.add(b, TopicDocumentCreated) // 2
	src.add(a, TopicDocumentIndexed) // 3 must wait for 1
	src.add(b, TopicDocumentIndexed) // 4
	pub := &scriptPub{fail: map[int64]bool{1: true}}
	r := &Relay{Publisher: pub}
	st, err := r.batch(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pub.got, []int64{2, 4}) || st.Failed != 1 || st.Skipped != 1 {
		t.Fatalf("order %v stats %+v", pub.got, st)
	}
	if src.row(1).attempts != 0 {
		t.Error("a transient failure must not count toward quarantine")
	}

	delete(pub.fail, 1)
	pub.got = nil
	if _, err := r.batch(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pub.got, []int64{1, 3}) {
		t.Errorf("retry order %v, want [1 3]", pub.got)
	}
}

// Design §7: permanent rejections quarantine the row after MaxAttempts,
// after which the tenant's later rows flow again.
func TestBatchQuarantinesPoisonRow(t *testing.T) {
	src := &fakeSource{}
	a := uuid.New()
	src.add(a, TopicDocumentCreated)
	src.add(a, TopicDocumentIndexed)
	pub := &scriptPub{fail: map[int64]bool{1: true}, perm: map[int64]bool{1: true}}
	var alerted []int64
	r := &Relay{Publisher: pub, MaxAttempts: 3, Quarantined: func(ev Event, _ error) { alerted = append(alerted, ev.ID) }}

	for i := 1; i <= 2; i++ {
		st, err := r.batch(context.Background(), src)
		if err != nil || st.Rejected != 1 || st.Skipped != 1 || len(pub.got) != 0 {
			t.Fatalf("attempt %d: %+v %v got %v", i, st, err, pub.got)
		}
	}
	st, err := r.batch(context.Background(), src)
	if err != nil || st.Quarantined != 1 || !slices.Equal(pub.got, []int64{2}) {
		t.Fatalf("third attempt: %+v %v got %v", st, err, pub.got)
	}
	if !slices.Equal(alerted, []int64{1}) || !src.row(1).quarantined {
		t.Errorf("alerts %v quarantined %v", alerted, src.row(1).quarantined)
	}
	if !slices.Equal(src.maxAttempts, []int{3, 3, 3}) {
		t.Errorf("max attempts passed %v", src.maxAttempts)
	}
}

// OBX-AC-002 (unit form): a stop between publish and mark leaves the row
// pending, so the next batch publishes it again.
func TestBatchCrashBeforeMarkRedelivers(t *testing.T) {
	src := &fakeSource{}
	src.add(uuid.New(), TopicDocumentCreated)
	pub := &scriptPub{}
	crash := errors.New("crash")
	r := &Relay{Publisher: pub, BeforeMark: func(Event) error { return crash }}
	if _, err := r.batch(context.Background(), src); !errors.Is(err, crash) {
		t.Fatalf("err %v", err)
	}
	r.BeforeMark = nil
	if _, err := r.batch(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pub.got, []int64{1, 1}) || !src.row(1).published {
		t.Errorf("deliveries %v published %v", pub.got, src.row(1).published)
	}
}

func TestBatchDatabaseErrors(t *testing.T) {
	r := &Relay{Publisher: &scriptPub{}}
	if _, err := r.batch(context.Background(), &fakeSource{nextErr: errors.New("conn reset")}); err == nil {
		t.Error("outbox_next error not returned")
	}
	src := &fakeSource{markErr: errors.New("conn reset")}
	src.add(uuid.New(), TopicDocumentCreated)
	if _, err := r.batch(context.Background(), src); err == nil {
		t.Error("mark error not returned")
	}
}

func TestBatchStopsOnCancel(t *testing.T) {
	src := &fakeSource{}
	src.add(uuid.New(), TopicDocumentCreated)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pub := &scriptPub{}
	st, err := (&Relay{Publisher: pub}).batch(ctx, src)
	if err != nil || len(pub.got) != 0 || st.Published != 0 {
		t.Errorf("published after cancel: %+v %v", st, err)
	}
}

func TestBackoff(t *testing.T) {
	r := &Relay{PollInterval: time.Second, MaxBackoff: 10 * time.Second}
	var got []time.Duration
	for f := 1; f <= 6; f++ {
		got = append(got, r.backoff(f))
	}
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff %v", got)
		}
	}
}

// OBX-REQ-005: the envelope carries event_id, topic, tenant_id,
// occurred_at and the ids-only data.
func TestEnvelope(t *testing.T) {
	tenant, doc := uuid.New(), uuid.New()
	data, _ := json.Marshal(DocumentIndexed{DocumentID: doc, Chunks: 3, EmbedModel: "m"})
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ev := Event{ID: 42, Topic: TopicDocumentIndexed, TenantID: tenant, OccurredAt: at, Data: data}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["event_id"] != float64(42) || m["topic"] != TopicDocumentIndexed || m["tenant_id"] != tenant.String() ||
		m["occurred_at"] != "2026-01-02T03:04:05Z" || m["version"] != float64(SchemaVersion) {
		t.Errorf("envelope %s", b)
	}
	if d := m["data"].(map[string]any); d["document_id"] != doc.String() || d["chunks"] != float64(3) {
		t.Errorf("data %v", d)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil || back.ID != 42 || back.TenantID != tenant || !back.OccurredAt.Equal(at) {
		t.Errorf("round trip %+v %v", back, err)
	}
}

func TestPayloadTopics(t *testing.T) {
	for p, want := range map[Payload]string{
		DocumentCreated{}:  "document.created",
		DocumentIndexed{}:  "document.indexed",
		DocumentFailed{}:   "document.failed",
		DocumentDeleting{}: "document.deleting",
	} {
		if p.Topic() != want {
			t.Errorf("%T topic %q", p, p.Topic())
		}
	}
	if err := (LogPublisher{}).Publish(context.Background(), Event{ID: 1}); err != nil {
		t.Errorf("log publisher: %v", err)
	}
	if !IsPermanent(fmt.Errorf("wrap: %w", Permanent(errors.New("x")))) || IsPermanent(errors.New("x")) || Permanent(nil) != nil {
		t.Error("Permanent / IsPermanent")
	}
}
