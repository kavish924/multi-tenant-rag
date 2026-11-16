package tenants

import "testing"

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		r, min Role
		want   bool
	}{
		{RoleOwner, RoleOwner, true},
		{RoleOwner, RoleAdmin, true},
		{RoleOwner, RoleMember, true},
		{RoleAdmin, RoleOwner, false},
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleMember, true},
		{RoleMember, RoleOwner, false},
		{RoleMember, RoleAdmin, false},
		{RoleMember, RoleMember, true},
		{Role("superuser"), RoleMember, false},
		{RoleOwner, Role("bogus"), false},
	}
	for _, c := range cases {
		if got := c.r.AtLeast(c.min); got != c.want {
			t.Errorf("%q.AtLeast(%q) = %v, want %v", c.r, c.min, got, c.want)
		}
	}
}

func TestParseRole(t *testing.T) {
	for _, s := range []string{"owner", "admin", "member"} {
		if _, err := ParseRole(s); err != nil {
			t.Errorf("ParseRole(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "Owner", "root", "viewer"} {
		if _, err := ParseRole(s); err == nil {
			t.Errorf("ParseRole(%q) accepted", s)
		}
	}
}
