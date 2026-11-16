// Package auth provides password hashing, JWT access tokens, the user
// service, and the request-context accessor for the authenticated user.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors returned by the user service.
var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidEmail       = errors.New("invalid email address")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrUserNotFound       = errors.New("user not found")
)

// User is a platform user. Users are global; tenant access comes from
// memberships.
type User struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// Service implements register, login and user lookup. The users table is
// global (not tenant-owned), so it is queried on the pool directly.
type Service struct {
	db     *pgxpool.Pool
	tokens *Tokens
	// dummyHash is verified when an email is unknown so login timing does
	// not reveal which emails are registered.
	dummyHash string
}

// NewService builds the user service.
func NewService(db *pgxpool.Pool, tokens *Tokens) (*Service, error) {
	h, err := HashPassword(uuid.NewString() + uuid.NewString())
	if err != nil {
		return nil, err
	}
	return &Service{db: db, tokens: tokens, dummyHash: h}, nil
}

// NormalizeEmail trims and lower-cases an address and checks its syntax.
func NormalizeEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if len(e) == 0 || len(e) > 254 {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return "", ErrInvalidEmail
	}
	return e, nil
}

// Register creates a user with an argon2id password hash.
func (s *Service) Register(ctx context.Context, email, password string) (User, error) {
	e, err := NormalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	u := User{Email: e}
	err = s.db.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, created_at`,
		e, hash).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("auth: insert user: %w", err)
	}
	return u, nil
}

// Login verifies credentials and issues an access token.
func (s *Service) Login(ctx context.Context, email, password string) (string, time.Time, error) {
	e, err := NormalizeEmail(email)
	if err != nil || len(password) > 4*MaxPasswordLen {
		return "", time.Time{}, ErrInvalidCredentials
	}
	var id uuid.UUID
	var hash string
	err = s.db.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE email = $1`, e).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(password, s.dummyHash)
		return "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: load user: %w", err)
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: verify: %w", err)
	}
	if !ok {
		return "", time.Time{}, ErrInvalidCredentials
	}
	return s.tokens.Issue(id)
}

// Get returns a user by id.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (User, error) {
	u := User{ID: id}
	err := s.db.QueryRow(ctx, `SELECT email, created_at FROM users WHERE id = $1`, id).Scan(&u.Email, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: get user: %w", err)
	}
	return u, nil
}

type ctxKey struct{}

// WithUser stores the authenticated user id in ctx.
func WithUser(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// UserFrom returns the authenticated user id, if any.
func UserFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}
