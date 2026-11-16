package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected hash format %q", h)
	}
	ok, err := VerifyPassword("correct horse battery staple", h)
	if err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong horse battery staple", h)
	if err != nil || ok {
		t.Fatalf("verify wrong password: ok=%v err=%v", ok, err)
	}
}

func TestPasswordHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same password here")
	b, _ := HashPassword("same password here")
	if a == b {
		t.Fatal("two hashes of the same password are equal; salt missing")
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=0,t=1,p=1$AA$AA"} {
		if _, err := VerifyPassword("x", h); err == nil {
			t.Errorf("hash %q: expected error", h)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if ValidatePassword("short") == nil {
		t.Error("short password accepted")
	}
	if ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)) == nil {
		t.Error("over-long password accepted")
	}
	if err := ValidatePassword("twelve chars"); err != nil {
		t.Errorf("valid password rejected: %v", err)
	}
}

func TestNormalizeEmail(t *testing.T) {
	got, err := NormalizeEmail("  Alice@Example.COM ")
	if err != nil || got != "alice@example.com" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "no-at-sign", "Alice <alice@example.com>", "a@b@c"} {
		if _, err := NormalizeEmail(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tk := NewTokens(testKey, "test", 15*time.Minute)
	id := uuid.New()
	raw, exp, err := tk.Issue(id)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(exp); d <= 14*time.Minute || d > 15*time.Minute {
		t.Fatalf("expiry %v not ~15m away", d)
	}
	got, err := tk.Verify(raw)
	if err != nil || got != id {
		t.Fatalf("verify: got %v, %v", got, err)
	}
}

// TEN-AC-002: expired, malformed and tampered tokens are rejected.
func TestTokenRejections(t *testing.T) {
	tk := NewTokens(testKey, "test", time.Minute)
	id := uuid.New()
	raw, _, _ := tk.Issue(id)

	expired := NewTokens(testKey, "test", time.Minute)
	expired.now = func() time.Time { return time.Now().Add(2 * time.Minute) }

	otherKey := NewTokens([]byte("ffffffffffffffffffffffffffffffff"), "test", time.Minute)
	otherIssuer := NewTokens(testKey, "someone-else", time.Minute)

	parts := strings.Split(raw, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]

	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{
		Subject: id.String(), Issuer: "test", Audience: jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	})
	noneRaw, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)

	noExp := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: id.String(), Issuer: "test", Audience: jwt.ClaimStrings{audience},
	})
	noExpRaw, _ := noExp.SignedString(testKey)

	cases := map[string]struct {
		v   *Tokens
		raw string
	}{
		"expired":      {expired, raw},
		"wrong key":    {otherKey, raw},
		"wrong issuer": {otherIssuer, raw},
		"tampered":     {tk, tampered},
		"malformed":    {tk, "not-a-jwt"},
		"empty":        {tk, ""},
		"alg none":     {tk, noneRaw},
		"no exp":       {tk, noExpRaw},
	}
	for name, c := range cases {
		if _, err := c.v.Verify(c.raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: got %v, want ErrInvalidToken", name, err)
		}
	}
}
