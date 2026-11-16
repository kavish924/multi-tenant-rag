package tenants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

// Errors returned by the tenant service.
var (
	ErrInvalidName   = errors.New("tenant name must be 1 to 200 characters")
	ErrNotFound      = errors.New("not found")
	ErrAlreadyMember = errors.New("user is already a member of this tenant")
	ErrLastOwner     = errors.New("a tenant must keep at least one owner")
	ErrOwnerGrant    = errors.New("only an owner can grant the owner role")
)

// Tenant is a customer workspace.
type Tenant struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Member is a membership joined with the user's email.
type Member struct {
	UserID    uuid.UUID `json:"user_id"`
	Email     string    `json:"email"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Service implements tenant and membership operations. Every query on a
// tenant-owned table goes through Store.WithTenant (TEN-REQ-007).
type Service struct {
	store *storage.Store
}

// NewService builds the tenant service.
func NewService(store *storage.Store) *Service { return &Service{store: store} }

// MembershipsFor lists a user's memberships through the memberships_for_user
// system function, which needs no tenant context (TEN-REQ-011).
func (s *Service) MembershipsFor(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	rows, err := s.store.Pool().Query(ctx, `SELECT tenant_id, role FROM memberships_for_user($1)`, userID)
	if err != nil {
		return nil, fmt.Errorf("tenants: memberships_for_user: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Membership, error) {
		var m Membership
		var role string
		if err := r.Scan(&m.TenantID, &role); err != nil {
			return m, err
		}
		m.Role = Role(role)
		return m, nil
	})
	if err != nil {
		return nil, fmt.Errorf("tenants: memberships_for_user: %w", err)
	}
	return out, nil
}

// Create makes a tenant with the caller as owner. The id is generated here so
// both inserts run inside WithTenant(newID) and pass RLS WITH CHECK.
func (s *Service) Create(ctx context.Context, ownerID uuid.UUID, name string) (Tenant, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		return Tenant{}, ErrInvalidName
	}
	t := Tenant{ID: uuid.New(), Name: name}
	err := s.store.WithTenant(ctx, t.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO tenants (id, name) VALUES ($1, $2) RETURNING created_at`,
			t.ID, t.Name).Scan(&t.CreatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`,
			t.ID, ownerID)
		return err
	})
	if err != nil {
		return Tenant{}, fmt.Errorf("tenants: create: %w", err)
	}
	return t, nil
}

// ListMembers returns the tenant's members ordered by join time.
func (s *Service) ListMembers(ctx context.Context, tenantID uuid.UUID) ([]Member, error) {
	var out []Member
	err := s.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.user_id, u.email, m.role, m.created_at
			FROM memberships m JOIN users u ON u.id = m.user_id
			ORDER BY m.created_at, m.user_id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) { return scanMember(r) })
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("tenants: list members: %w", err)
	}
	return out, nil
}

// AddMember adds an existing user, looked up by email, to the tenant. Only
// an owner may grant the owner role.
func (s *Service) AddMember(ctx context.Context, tenantID uuid.UUID, actor Role, email string, role Role) (Member, error) {
	if role.rank() == 0 {
		return Member{}, ErrInvalidRole
	}
	if role == RoleOwner && actor != RoleOwner {
		return Member{}, ErrOwnerGrant
	}
	e, err := auth.NormalizeEmail(email)
	if err != nil {
		return Member{}, ErrNotFound
	}
	m := Member{Email: e, Role: role}
	err = s.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, e).Scan(&m.UserID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3) RETURNING created_at`,
			tenantID, m.UserID, string(role)).Scan(&m.CreatedAt)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Member{}, ErrNotFound
	case isUniqueViolation(err):
		return Member{}, ErrAlreadyMember
	case err != nil:
		return Member{}, fmt.Errorf("tenants: add member: %w", err)
	}
	return m, nil
}

// ChangeRole sets a member's role. Demoting the last owner is refused.
func (s *Service) ChangeRole(ctx context.Context, tenantID, userID uuid.UUID, role Role) (Member, error) {
	if role.rank() == 0 {
		return Member{}, ErrInvalidRole
	}
	var m Member
	err := s.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		current, err := lockMember(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		if current == RoleOwner && role != RoleOwner {
			if err := ensureAnotherOwner(ctx, tx, tenantID); err != nil {
				return err
			}
		}
		row := tx.QueryRow(ctx, `
			UPDATE memberships m SET role = $3
			FROM users u
			WHERE m.tenant_id = $1 AND m.user_id = $2 AND u.id = m.user_id
			RETURNING m.user_id, u.email, m.role, m.created_at`,
			tenantID, userID, string(role))
		m, err = scanMember(row)
		return err
	})
	if err != nil {
		return Member{}, mapErr("change role", err)
	}
	return m, nil
}

// RemoveMember deletes a membership. Removing the last owner is refused.
func (s *Service) RemoveMember(ctx context.Context, tenantID, userID uuid.UUID) error {
	err := s.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		current, err := lockMember(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		if current == RoleOwner {
			if err := ensureAnotherOwner(ctx, tx, tenantID); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
		return err
	})
	return mapErr("remove member", err)
}

// lockMember locks every owner row plus the target row, so two concurrent
// requests cannot each remove or demote a different "other" owner and leave
// the tenant with none.
func lockMember(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (Role, error) {
	if _, err := tx.Exec(ctx, `
		SELECT 1 FROM memberships
		WHERE tenant_id = $1 AND (role = 'owner' OR user_id = $2)
		ORDER BY user_id
		FOR UPDATE`, tenantID, userID); err != nil {
		return "", err
	}
	var role string
	if err := tx.QueryRow(ctx,
		`SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID).Scan(&role); err != nil {
		return "", err
	}
	return Role(role), nil
}

func ensureAnotherOwner(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	var owners int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'`,
		tenantID).Scan(&owners); err != nil {
		return err
	}
	if owners < 2 {
		return ErrLastOwner
	}
	return nil
}

func scanMember(r pgx.Row) (Member, error) {
	var m Member
	var role string
	err := r.Scan(&m.UserID, &m.Email, &role, &m.CreatedAt)
	m.Role = Role(role)
	return m, err
}

func mapErr(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case errors.Is(err, ErrLastOwner):
		return ErrLastOwner
	default:
		return fmt.Errorf("tenants: %s: %w", op, err)
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
