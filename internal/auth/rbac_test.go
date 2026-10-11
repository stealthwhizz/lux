package auth

import "testing"

func TestRolePermissions(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		{Viewer, PermRead, true},
		{Viewer, PermRunTask, false},
		{Operator, PermRunTask, true},
		{Operator, PermDecideApproval, false},
		{Operator, PermManageAgents, false},
		{Approver, PermDecideApproval, true},
		{Approver, PermManagePolicy, false},
		{Admin, PermManagePolicy, true},
		{Admin, PermManageMembers, false},
		{Owner, PermManageMembers, true},
		{Role("root"), PermRead, false},
	}
	for _, c := range cases {
		if got := Can(c.role, c.perm); got != c.want {
			t.Errorf("Can(%s, %s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestValidRole(t *testing.T) {
	for _, r := range []string{"viewer", "operator", "approver", "admin", "owner"} {
		if !ValidRole(r) {
			t.Errorf("%s should be valid", r)
		}
	}
	if ValidRole("superuser") {
		t.Error("unknown role accepted")
	}
}
