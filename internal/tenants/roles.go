// Package tenants implements tenants, memberships and roles (TEN-REQ-003/004).
package tenants

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Role is a membership role. The set is closed and mirrors the CHECK
// constraint in migrations/0001_init.sql.
type Role string

// Roles in increasing order of privilege.
const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

// ErrInvalidRole is returned for any role outside the closed set.
var ErrInvalidRole = errors.New("role must be one of owner, admin, member")

// ParseRole validates a role string.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if r.rank() == 0 {
		return "", ErrInvalidRole
	}
	return r, nil
}

func (r Role) rank() int {
	switch r {
	case RoleMember:
		return 1
	case RoleAdmin:
		return 2
	case RoleOwner:
		return 3
	default:
		return 0
	}
}

// AtLeast reports whether r grants at least the privileges of min. Unknown
// roles never satisfy any minimum.
func (r Role) AtLeast(min Role) bool {
	return r.rank() > 0 && min.rank() > 0 && r.rank() >= min.rank()
}

// Membership is a user's verified role in one tenant.
type Membership struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Role     Role      `json:"role"`
}

type ctxKey struct{}

// WithMembership stores the verified tenant membership for the request.
func WithMembership(ctx context.Context, m Membership) context.Context {
	return context.WithValue(ctx, ctxKey{}, m)
}

// FromContext returns the verified membership set by ResolveTenant. Handlers
// must take the tenant id from here, never from client input (TEN-AC-005).
func FromContext(ctx context.Context) (Membership, bool) {
	m, ok := ctx.Value(ctxKey{}).(Membership)
	return m, ok && m.TenantID != uuid.Nil
}
