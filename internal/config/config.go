// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DB selects how binaries reach PostgreSQL.
//
// Exactly one mode is used:
//   - URL set: a plain DSN (workstation compose stack, tests).
//   - CloudSQLInstance set: the Cloud SQL Go connector with IAM database
//     authentication over private IP. User is the IAM database user
//     (for example "sa-api@<project>.iam"); no password exists.
type DB struct {
	URL              string
	CloudSQLInstance string // "<project>:<region>:<instance>"
	User             string
	Name             string
}

// API is the configuration of cmd/api.
type API struct {
	Addr      string
	DB        DB
	JWTKey    []byte
	JWTIssuer string
	JWTTTL    time.Duration
	LogLevel  string

	Objects        Objects
	MaxUploadBytes int64
	Rerank         Rerank
	Quotas         Quotas
	Guards         Guards
}

// Rerank configures the optional rerank stage (spec 009). It is per
// deployment, never per request (RER-REQ-002).
type Rerank struct {
	Enabled  bool          // RERANK_ENABLED, default false
	Provider string        // RERANK_PROVIDER: "llm" or "cross_encoder"
	Keep     int           // RERANK_KEEP, default 8
	Timeout  time.Duration // RERANK_TIMEOUT, default 800ms
	Endpoint string        // RERANK_ENDPOINT, required for cross_encoder
	Model    string        // RERANK_MODEL, optional
}

// LoadRerank reads the rerank settings.
func LoadRerank() (Rerank, error) {
	r := Rerank{
		Provider: env("RERANK_PROVIDER", "llm"),
		Keep:     8,
		Timeout:  800 * time.Millisecond,
		Endpoint: os.Getenv("RERANK_ENDPOINT"),
		Model:    os.Getenv("RERANK_MODEL"),
	}
	switch v := strings.ToLower(os.Getenv("RERANK_ENABLED")); v {
	case "", "false", "0", "no":
	case "true", "1", "yes":
		r.Enabled = true
	default:
		return Rerank{}, fmt.Errorf("config: RERANK_ENABLED %q is not a boolean", v)
	}
	var err error
	if r.Keep, err = intEnv("RERANK_KEEP", r.Keep, 1, 50); err != nil {
		return Rerank{}, err
	}
	if v := os.Getenv("RERANK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 10*time.Second {
			return Rerank{}, errors.New("config: RERANK_TIMEOUT must be a duration in (0, 10s]")
		}
		r.Timeout = d
	}
	switch r.Provider {
	case "llm":
	case "cross_encoder":
		if r.Enabled && r.Endpoint == "" {
			return Rerank{}, errors.New("config: RERANK_ENDPOINT is required for RERANK_PROVIDER=cross_encoder")
		}
	default:
		return Rerank{}, fmt.Errorf("config: RERANK_PROVIDER %q must be llm or cross_encoder", r.Provider)
	}
	return r, nil
}

// Objects selects the object store. Only "fs" (local directory, for
// development and tests) exists until the Cloud Storage implementation
// lands with the GCP work.
type Objects struct {
	Kind string // OBJECT_STORE: "fs"
	Dir  string // OBJECT_STORE_DIR for "fs"
}

// LoadObjects reads the object store settings shared by api and worker.
func LoadObjects() (Objects, error) {
	o := Objects{Kind: env("OBJECT_STORE", "fs"), Dir: env("OBJECT_STORE_DIR", ".data/objects")}
	if o.Kind != "fs" {
		return Objects{}, fmt.Errorf("config: OBJECT_STORE %q not supported yet (only fs)", o.Kind)
	}
	return o, nil
}

// Worker is the configuration of cmd/worker.
type Worker struct {
	DB          DB
	Objects     Objects
	Quotas      Quotas
	Concurrency int
	// Retention (spec 013): RETENTION_DAYS (30), SWEEP_INTERVAL (1h),
	// ORPHAN_GRACE (1h).
	Retention     time.Duration
	SweepInterval time.Duration
	OrphanGrace   time.Duration
	LogLevel      string
}

// LoadWorker reads cmd/worker configuration.
func LoadWorker() (Worker, error) {
	db, err := LoadDB()
	if err != nil {
		return Worker{}, err
	}
	objs, err := LoadObjects()
	if err != nil {
		return Worker{}, err
	}
	q, err := LoadQuotas()
	if err != nil {
		return Worker{}, err
	}
	w := Worker{DB: db, Objects: objs, Quotas: q, Concurrency: 4, LogLevel: env("LOG_LEVEL", "info"),
		Retention: 30 * 24 * time.Hour, SweepInterval: time.Hour, OrphanGrace: time.Hour}
	days, err := intEnv("RETENTION_DAYS", 30, 0, 3650)
	if err != nil {
		return Worker{}, err
	}
	w.Retention = time.Duration(days) * 24 * time.Hour
	for _, f := range []struct {
		key string
		dst *time.Duration
	}{{"SWEEP_INTERVAL", &w.SweepInterval}, {"ORPHAN_GRACE", &w.OrphanGrace}} {
		if v := os.Getenv(f.key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < time.Minute {
				return Worker{}, fmt.Errorf("config: %s must be a duration of at least 1m", f.key)
			}
			*f.dst = d
		}
	}
	if v := os.Getenv("WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return Worker{}, errors.New("config: WORKER_CONCURRENCY must be 1..64")
		}
		w.Concurrency = n
	}
	return w, nil
}

// MaxTokenTTL is the upper bound from TEN-NFR-002.
const MaxTokenTTL = 15 * time.Minute

// MinJWTKeyLen is the minimum HS256 key length in bytes.
const MinJWTKeyLen = 32

// LoadDB reads the database settings shared by every binary.
func LoadDB() (DB, error) {
	db := DB{
		URL:              os.Getenv("DATABASE_URL"),
		CloudSQLInstance: os.Getenv("CLOUDSQL_INSTANCE"),
		User:             os.Getenv("DB_USER"),
		Name:             env("DB_NAME", "app"),
	}
	switch {
	case db.URL != "" && db.CloudSQLInstance != "":
		return DB{}, errors.New("config: set DATABASE_URL or CLOUDSQL_INSTANCE, not both")
	case db.URL == "" && db.CloudSQLInstance == "":
		return DB{}, errors.New("config: DATABASE_URL or CLOUDSQL_INSTANCE is required")
	case db.CloudSQLInstance != "" && db.User == "":
		return DB{}, errors.New("config: DB_USER (IAM database user) is required with CLOUDSQL_INSTANCE")
	}
	return db, nil
}

// LoadAPI reads cmd/api configuration.
func LoadAPI() (API, error) {
	db, err := LoadDB()
	if err != nil {
		return API{}, err
	}
	cfg := API{
		Addr:      ":" + env("PORT", "8080"),
		DB:        db,
		JWTKey:    []byte(os.Getenv("JWT_SIGNING_KEY")),
		JWTIssuer: env("JWT_ISSUER", "multi-tenant-rag"),
		LogLevel:  env("LOG_LEVEL", "info"),
	}
	if len(cfg.JWTKey) < MinJWTKeyLen {
		return API{}, fmt.Errorf("config: JWT_SIGNING_KEY must be at least %d bytes", MinJWTKeyLen)
	}
	ttl, err := time.ParseDuration(env("JWT_TTL", "15m"))
	if err != nil {
		return API{}, fmt.Errorf("config: JWT_TTL: %w", err)
	}
	if ttl <= 0 || ttl > MaxTokenTTL {
		return API{}, fmt.Errorf("config: JWT_TTL must be in (0, %s]", MaxTokenTTL)
	}
	cfg.JWTTTL = ttl

	cfg.Objects, err = LoadObjects()
	if err != nil {
		return API{}, err
	}
	cfg.MaxUploadBytes = 50 << 20
	if v := os.Getenv("MAX_UPLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return API{}, errors.New("config: MAX_UPLOAD_BYTES must be a positive integer")
		}
		cfg.MaxUploadBytes = n
	}
	if cfg.Rerank, err = LoadRerank(); err != nil {
		return API{}, err
	}
	if cfg.Quotas, err = LoadQuotas(); err != nil {
		return API{}, err
	}
	if cfg.Guards, err = LoadGuards(); err != nil {
		return API{}, err
	}
	return cfg, nil
}

// List splits a comma-separated environment variable, dropping blanks.
func List(key string) []string {
	var out []string
	for _, s := range strings.Split(os.Getenv(key), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Relay is the configuration of cmd/outbox-relay (spec 006).
type Relay struct {
	DB           DB
	Publisher    string // OUTBOX_PUBLISHER: "log" (Pub/Sub lands with the GCP work)
	BatchSize    int
	PollInterval time.Duration
	MaxAttempts  int
	LogLevel     string
}

// LoadRelay reads cmd/outbox-relay configuration.
func LoadRelay() (Relay, error) {
	db, err := LoadDB()
	if err != nil {
		return Relay{}, err
	}
	r := Relay{
		DB:           db,
		Publisher:    env("OUTBOX_PUBLISHER", "log"),
		BatchSize:    100,
		PollInterval: time.Second,
		MaxAttempts:  5,
		LogLevel:     env("LOG_LEVEL", "info"),
	}
	if r.Publisher != "log" {
		return Relay{}, fmt.Errorf("config: OUTBOX_PUBLISHER %q not supported yet (only log)", r.Publisher)
	}
	if r.BatchSize, err = intEnv("OUTBOX_BATCH_SIZE", r.BatchSize, 1, 1000); err != nil {
		return Relay{}, err
	}
	if r.MaxAttempts, err = intEnv("OUTBOX_MAX_ATTEMPTS", r.MaxAttempts, 1, 100); err != nil {
		return Relay{}, err
	}
	if v := os.Getenv("OUTBOX_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Millisecond || d > time.Minute {
			return Relay{}, errors.New("config: OUTBOX_POLL_INTERVAL must be a duration in [10ms, 1m]")
		}
		r.PollInterval = d
	}
	return r, nil
}

func intEnv(key string, def, lo, hi int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("config: %s must be %d..%d", key, lo, hi)
	}
	return n, nil
}

// Quotas are the platform quota defaults and breaker settings (spec 011).
// Per-tenant overrides live in tenant_quotas, not here.
type Quotas struct {
	UploadsDay          int64 // QUOTA_UPLOADS_DAY (500)
	QuestionsDay        int64 // QUOTA_QUESTIONS_DAY (1000)
	EmbedTokensMonth    int64 // QUOTA_EMBED_TOKENS_MONTH (50M)
	GenerateTokensMonth int64 // QUOTA_GENERATE_TOKENS_MONTH (20M)
	ConcurrentAsks      int64 // QUOTA_CONCURRENT_ASKS (5)
	ConcurrentJobs      int64 // QUOTA_CONCURRENT_JOBS (3)

	BreakerThreshold int           // BREAKER_THRESHOLD (5 consecutive failures)
	BreakerOpenFor   time.Duration // BREAKER_OPEN_FOR (30s)
}

// LoadQuotas reads quota defaults and breaker settings.
func LoadQuotas() (Quotas, error) {
	q := Quotas{
		UploadsDay: 500, QuestionsDay: 1000,
		EmbedTokensMonth: 50_000_000, GenerateTokensMonth: 20_000_000,
		ConcurrentAsks: 5, ConcurrentJobs: 3,
		BreakerThreshold: 5, BreakerOpenFor: 30 * time.Second,
	}
	for _, f := range []struct {
		key string
		dst *int64
	}{
		{"QUOTA_UPLOADS_DAY", &q.UploadsDay},
		{"QUOTA_QUESTIONS_DAY", &q.QuestionsDay},
		{"QUOTA_EMBED_TOKENS_MONTH", &q.EmbedTokensMonth},
		{"QUOTA_GENERATE_TOKENS_MONTH", &q.GenerateTokensMonth},
		{"QUOTA_CONCURRENT_ASKS", &q.ConcurrentAsks},
		{"QUOTA_CONCURRENT_JOBS", &q.ConcurrentJobs},
	} {
		if v := os.Getenv(f.key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return Quotas{}, fmt.Errorf("config: %s must be a non-negative integer", f.key)
			}
			*f.dst = n
		}
	}
	var err error
	if q.BreakerThreshold, err = intEnv("BREAKER_THRESHOLD", q.BreakerThreshold, 1, 1000); err != nil {
		return Quotas{}, err
	}
	if v := os.Getenv("BREAKER_OPEN_FOR"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 10*time.Minute {
			return Quotas{}, errors.New("config: BREAKER_OPEN_FOR must be a duration in (0, 10m]")
		}
		q.BreakerOpenFor = d
	}
	return q, nil
}

// Guards configures the guardrail policy (spec 012).
type Guards struct {
	OutputMode          string        // GUARD_OUTPUT_MODE: windowed (default) or strict
	WindowTokens        int           // GUARD_WINDOW_TOKENS: windowed buffer, default 256
	RedactEmails        bool          // GUARD_REDACT_EMAILS, default false
	Faithfulness        string        // GUARD_FAITHFULNESS: off (default), warn or block
	FaithfulnessTimeout time.Duration // GUARD_FAITHFULNESS_TIMEOUT, default 1.5s
	ProbeLimit          int           // GUARD_PROBE_LIMIT: blocks per user per minute before 429, default 10
}

// LoadGuards reads the guard settings.
func LoadGuards() (Guards, error) {
	g := Guards{
		OutputMode: env("GUARD_OUTPUT_MODE", "windowed"), WindowTokens: 256,
		Faithfulness: env("GUARD_FAITHFULNESS", "off"), FaithfulnessTimeout: 1500 * time.Millisecond, ProbeLimit: 10,
	}
	if g.OutputMode != "windowed" && g.OutputMode != "strict" {
		return Guards{}, errors.New("config: GUARD_OUTPUT_MODE must be windowed or strict")
	}
	if g.Faithfulness != "off" && g.Faithfulness != "warn" && g.Faithfulness != "block" {
		return Guards{}, errors.New("config: GUARD_FAITHFULNESS must be off, warn or block")
	}
	var err error
	if g.WindowTokens, err = intEnv("GUARD_WINDOW_TOKENS", g.WindowTokens, 64, 8192); err != nil {
		return Guards{}, err
	}
	if g.ProbeLimit, err = intEnv("GUARD_PROBE_LIMIT", g.ProbeLimit, 1, 10_000); err != nil {
		return Guards{}, err
	}
	if v := os.Getenv("GUARD_REDACT_EMAILS"); v != "" {
		if g.RedactEmails, err = strconv.ParseBool(v); err != nil {
			return Guards{}, errors.New("config: GUARD_REDACT_EMAILS must be a boolean")
		}
	}
	if v := os.Getenv("GUARD_FAITHFULNESS_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 30*time.Second {
			return Guards{}, errors.New("config: GUARD_FAITHFULNESS_TIMEOUT must be a duration in (0, 30s]")
		}
		g.FaithfulnessTimeout = d
	}
	return g, nil
}
