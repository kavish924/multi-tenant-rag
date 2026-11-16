// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
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
