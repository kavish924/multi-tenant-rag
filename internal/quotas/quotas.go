// Package quotas enforces per-tenant spend limits before providers are
// called (spec 011): daily upload and question counts, monthly token
// budgets, and concurrent ask streams. Counters are fixed UTC windows in
// Postgres; effective limits are defaults overlaid with tenant_quotas
// overrides, cached for a few seconds.
package quotas

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/httpx"
)

// Dimensions (tenant_usage_counters.dimension).
const (
	DimUploads        = "uploads"
	DimQuestions      = "questions"
	DimEmbedTokens    = "embed_tokens"
	DimGenerateTokens = "generate_tokens"
)

// Rejection codes (QTA-REQ-004).
const (
	CodeUploads     = "quota_uploads"
	CodeQuestions   = "quota_questions"
	CodeTokens      = "quota_tokens"
	CodeConcurrency = "concurrency"
)

// Limits are a tenant's effective quotas.
type Limits struct {
	UploadsDay          int64 `json:"uploads_day"`
	QuestionsDay        int64 `json:"questions_day"`
	EmbedTokensMonth    int64 `json:"embed_tokens_month"`
	GenerateTokensMonth int64 `json:"generate_tokens_month"`
	ConcurrentAsks      int64 `json:"concurrent_asks"`
	ConcurrentJobs      int64 `json:"concurrent_jobs"`
}

// Defaults are the platform defaults (QTA-REQ-001).
var Defaults = Limits{
	UploadsDay:          500,
	QuestionsDay:        1000,
	EmbedTokensMonth:    50_000_000,
	GenerateTokensMonth: 20_000_000,
	ConcurrentAsks:      5,
	ConcurrentJobs:      3,
}

// Overrides is one tenant_quotas row; nil fields keep the default.
type Overrides struct {
	UploadsDay          *int64 `json:"uploads_day,omitempty"`
	QuestionsDay        *int64 `json:"questions_day,omitempty"`
	EmbedTokensMonth    *int64 `json:"embed_tokens_month,omitempty"`
	GenerateTokensMonth *int64 `json:"generate_tokens_month,omitempty"`
	ConcurrentAsks      *int64 `json:"concurrent_asks,omitempty"`
	ConcurrentJobs      *int64 `json:"concurrent_jobs,omitempty"`
}

// Apply overlays o on d.
func (o Overrides) Apply(d Limits) Limits {
	set := func(dst *int64, v *int64) {
		if v != nil {
			*dst = *v
		}
	}
	set(&d.UploadsDay, o.UploadsDay)
	set(&d.QuestionsDay, o.QuestionsDay)
	set(&d.EmbedTokensMonth, o.EmbedTokensMonth)
	set(&d.GenerateTokensMonth, o.GenerateTokensMonth)
	set(&d.ConcurrentAsks, o.ConcurrentAsks)
	set(&d.ConcurrentJobs, o.ConcurrentJobs)
	return d
}

// Exceeded is a quota rejection: the API maps it to 429 with Retry-After.
type Exceeded struct {
	Code       string
	Limit      int64
	Used       int64
	RetryAfter time.Duration
}

func (e *Exceeded) Error() string {
	return fmt.Sprintf("%s: %d of %d used", e.Code, e.Used, e.Limit)
}

// RetryAfterSeconds rounds up, at least 1.
func (e *Exceeded) RetryAfterSeconds() int64 {
	s := int64((e.RetryAfter + time.Second - 1) / time.Second)
	return max(s, 1)
}

// Window is the fixed UTC window a dimension counts in.
type Window struct {
	Start, End time.Time
}

// WindowFor returns the window containing now: UTC days for counts, UTC
// calendar months for tokens.
func WindowFor(dim string, now time.Time) Window {
	now = now.UTC()
	switch dim {
	case DimEmbedTokens, DimGenerateTokens:
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return Window{Start: start, End: start.AddDate(0, 1, 0)}
	default:
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return Window{Start: start, End: start.AddDate(0, 0, 1)}
	}
}

// LimitFor returns the limit of a counted dimension.
func (l Limits) LimitFor(dim string) int64 {
	switch dim {
	case DimUploads:
		return l.UploadsDay
	case DimQuestions:
		return l.QuestionsDay
	case DimEmbedTokens:
		return l.EmbedTokensMonth
	case DimGenerateTokens:
		return l.GenerateTokensMonth
	}
	return 0
}

// codeFor maps a dimension to its rejection code.
func codeFor(dim string) string {
	switch dim {
	case DimUploads:
		return CodeUploads
	case DimQuestions:
		return CodeQuestions
	default:
		return CodeTokens
	}
}

// TokenDimension maps a usage kind to the budget it draws from: embeddings
// from embed_tokens, generation and LLM reranking from generate_tokens.
func TokenDimension(kind string) (string, bool) {
	switch kind {
	case "embed":
		return DimEmbedTokens, true
	case "generate", "rerank":
		return DimGenerateTokens, true
	}
	return "", false
}

// WriteExceeded writes the 429 envelope with Retry-After (design §5):
// {"error":{"code","message","retry_after","limit","used"}}.
func WriteExceeded(w http.ResponseWriter, e *Exceeded) {
	ra := e.RetryAfterSeconds()
	w.Header().Set("Retry-After", strconv.FormatInt(ra, 10))
	msg := "quota exceeded"
	if e.Code == CodeConcurrency {
		msg = "too many concurrent requests for this tenant"
	}
	httpx.JSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"code": e.Code, "message": msg, "retry_after": ra, "limit": e.Limit, "used": e.Used,
	}})
}

// FromConfig builds the platform defaults from config.Quotas fields.
func FromConfig(uploadsDay, questionsDay, embedMonth, generateMonth, concurrentAsks, concurrentJobs int64) Limits {
	return Limits{
		UploadsDay: uploadsDay, QuestionsDay: questionsDay,
		EmbedTokensMonth: embedMonth, GenerateTokensMonth: generateMonth,
		ConcurrentAsks: concurrentAsks, ConcurrentJobs: concurrentJobs,
	}
}
