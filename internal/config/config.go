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
	Concurrency int
	LogLevel    string
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
	w := Worker{DB: db, Objects: objs, Concurrency: 4, LogLevel: env("LOG_LEVEL", "info")}
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
