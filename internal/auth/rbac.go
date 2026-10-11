// Package auth implements roles and permissions.
package auth

type Role string

const (
	Viewer   Role = "viewer"
	Operator Role = "operator"
	Approver Role = "approver"
	Admin    Role = "admin"
	Owner    Role = "owner"
)

type Permission string

const (
	PermRead            Permission = "read"            // see agents, tasks, logs
	PermRunTask         Permission = "task.run"        // create tasks, conversations, cancel own tasks
	PermCancelAny       Permission = "task.cancel_any" // cancel tasks created by others
	PermDecideApproval  Permission = "approval.decide" // approve or reject
	PermManageAgents    Permission = "agent.manage"    // import, sync, disable agents
	PermManageRepos     Permission = "repo.manage"     // register code repositories
	PermManagePolicy    Permission = "policy.manage"   // edit workspace policy and budgets
	PermManageMembers   Permission = "member.manage"   // add/remove members, create tokens for others
	PermReadAudit       Permission = "audit.read"      // read the audit log
	PermManageSchedules Permission = "schedule.manage" // enable/disable schedules
)

var rolePerms = map[Role][]Permission{
	Viewer:   {PermRead},
	Operator: {PermRead, PermRunTask},
	Approver: {PermRead, PermRunTask, PermDecideApproval},
	Admin: {PermRead, PermRunTask, PermCancelAny, PermDecideApproval, PermManageAgents,
		PermManageRepos, PermManagePolicy, PermReadAudit, PermManageSchedules},
	Owner: {PermRead, PermRunTask, PermCancelAny, PermDecideApproval, PermManageAgents,
		PermManageRepos, PermManagePolicy, PermManageMembers, PermReadAudit, PermManageSchedules},
}

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool {
	_, ok := rolePerms[Role(r)]
	return ok
}

// Can reports whether role r grants permission p.
func Can(r Role, p Permission) bool {
	for _, x := range rolePerms[r] {
		if x == p {
			return true
		}
	}
	return false
}

// Rank orders roles so a member cannot grant a role above their own.
func Rank(r Role) int {
	switch r {
	case Viewer:
		return 1
	case Operator:
		return 2
	case Approver:
		return 3
	case Admin:
		return 4
	case Owner:
		return 5
	}
	return 0
}
