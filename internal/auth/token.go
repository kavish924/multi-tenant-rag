package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken covers every rejected token: malformed, tampered, wrong
// algorithm, wrong issuer or audience, missing or past expiry (TEN-AC-002).
var ErrInvalidToken = errors.New("auth: invalid token")

const audience = "api"

// Tokens issues and verifies HS256 access tokens (TEN-REQ-002). The key comes
// from Secret Manager via the JWT_SIGNING_KEY environment variable.
type Tokens struct {
	key    []byte
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

// NewTokens builds a token issuer/verifier.
func NewTokens(key []byte, issuer string, ttl time.Duration) *Tokens {
	return &Tokens{key: key, issuer: issuer, ttl: ttl, now: time.Now}
}

// Issue signs a token for userID and returns it with its expiry.
func (t *Tokens) Issue(userID uuid.UUID) (string, time.Time, error) {
	now := t.now()
	exp := now.Add(t.ttl)
	claims := jwt.RegisteredClaims{
		Subject:   userID.String(),
		Issuer:    t.issuer,
		Audience:  jwt.ClaimStrings{audience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
		ID:        uuid.NewString(),
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign token: %w", err)
	}
	return s, exp, nil
}

// Verify checks signature, algorithm, issuer, audience and expiry, and
// returns the user id from `sub`.
func (t *Tokens) Verify(raw string) (uuid.UUID, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return t.key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return uuid.Nil, ErrInvalidToken
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, ErrInvalidToken
	}
	return id, nil
}
