package quotas

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWindows(t *testing.T) {
	at := time.Date(2026, 12, 31, 23, 59, 59, 0, time.FixedZone("x", 5*3600)) // 18:59:59 UTC
	d := WindowFor(DimQuestions, at)
	if !d.Start.Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) || !d.End.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day window %v", d)
	}
	m := WindowFor(DimGenerateTokens, at)
	if !m.Start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) || !m.End.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month window %v", m)
	}
	for _, dim := range []string{DimUploads, DimQuestions} {
		if WindowFor(dim, at).End.Sub(WindowFor(dim, at).Start) != 24*time.Hour {
			t.Errorf("%s window not a day", dim)
		}
	}
}

func TestOverridesAndLimits(t *testing.T) {
	five, zero := int64(5), int64(0)
	l := Overrides{QuestionsDay: &five, ConcurrentAsks: &zero}.Apply(Defaults)
	if l.QuestionsDay != 5 || l.ConcurrentAsks != 0 || l.UploadsDay != Defaults.UploadsDay {
		t.Errorf("apply %+v", l)
	}
	if Defaults != (Limits{500, 1000, 50_000_000, 20_000_000, 5, 3}) {
		t.Errorf("defaults drifted from QTA-REQ-001: %+v", Defaults)
	}
	for dim, want := range map[string]int64{DimUploads: 500, DimQuestions: 1000, DimEmbedTokens: 50_000_000, DimGenerateTokens: 20_000_000, "x": 0} {
		if Defaults.LimitFor(dim) != want {
			t.Errorf("LimitFor(%s)", dim)
		}
	}
	for kind, want := range map[string]string{"embed": DimEmbedTokens, "generate": DimGenerateTokens, "rerank": DimGenerateTokens} {
		if got, ok := TokenDimension(kind); !ok || got != want {
			t.Errorf("TokenDimension(%s) = %s", kind, got)
		}
	}
	if _, ok := TokenDimension("other"); ok {
		t.Error("unknown kind mapped")
	}
}

// QTA-REQ-004: 429 envelope with Retry-After and machine-readable code.
func TestWriteExceeded(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteExceeded(rec, &Exceeded{Code: CodeQuestions, Limit: 1000, Used: 1000, RetryAfter: 3599500 * time.Millisecond})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "3600" {
		t.Fatalf("status %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Code       string `json:"code"`
			RetryAfter int64  `json:"retry_after"`
			Limit      int64  `json:"limit"`
			Used       int64  `json:"used"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "quota_questions" ||
		body.Error.RetryAfter != 3600 || body.Error.Limit != 1000 || body.Error.Used != 1000 {
		t.Errorf("body %s", rec.Body)
	}
	if (&Exceeded{RetryAfter: 0}).RetryAfterSeconds() != 1 {
		t.Error("Retry-After must be at least 1")
	}
}
