// Package policy defines the workspace policy document stored in
// workspaces.policy and the decisions derived from it.
package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Workspace is the policy for one workspace. The zero value is the safe
// default: nothing optional is allowed.
type Workspace struct {
	// AllowCodeExecutionOnLoad permits running agents whose repository
	// executes code when loaded (plugins, hooks, extends, MCP commands).
	AllowCodeExecutionOnLoad bool `json:"allow_code_execution_on_load"`
	// RequireOpenGAP makes `opengap validate` failures block an import.
	RequireOpenGAP bool `json:"require_opengap"`
	// MaxTaskTimeoutSeconds caps the timeout a task may request (0 = 86400).
	MaxTaskTimeoutSeconds int `json:"max_task_timeout_seconds,omitempty"`

	// AllowModelFallback lets the gateway move to an agent's next declared
	// model when the current one is unreachable or returns a 5xx.
	AllowModelFallback bool `json:"allow_model_fallback"`
	// MonthlyTokenBudget caps input+output tokens per calendar month (UTC)
	// for the whole workspace. 0 = no limit.
	MonthlyTokenBudget int64 `json:"monthly_token_budget,omitempty"`
	// AgentMonthlyTokenBudget is the same cap for each agent. 0 = no limit.
	AgentMonthlyTokenBudget int64 `json:"agent_monthly_token_budget,omitempty"`
	// ModelRequestsPerMinute limits model calls per agent. 0 = no limit.
	ModelRequestsPerMinute int `json:"model_requests_per_minute,omitempty"`

	// ToolRules decide each tool call (see tools.go). Absent: the default
	// rules. Present but empty: every tool call is denied.
	ToolRules *[]ToolRule `json:"tool_rules,omitempty"`
	// RequireDistinctApprover (maker-checker): the person who created a task
	// cannot approve its actions. Default true.
	RequireDistinctApprover *bool `json:"require_distinct_approver,omitempty"`
	// ApprovalTimeoutSeconds: pending approvals expire (and deny) after this.
	// 0 = 3600.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds,omitempty"`

	toolRules []ToolRule // compiled ToolRules
}

// DistinctApprover reports whether maker-checker applies.
func (p Workspace) DistinctApprover() bool {
	return p.RequireDistinctApprover == nil || *p.RequireDistinctApprover
}

// ApprovalTimeout returns the approval expiry in seconds.
func (p Workspace) ApprovalTimeout() int {
	if p.ApprovalTimeoutSeconds > 0 {
		return p.ApprovalTimeoutSeconds
	}
	return 3600
}

// Parse decodes a stored policy. Unknown fields are rejected so a typo
// cannot silently leave a control switched off.
func Parse(raw []byte) (Workspace, error) {
	var p Workspace
	if len(raw) == 0 {
		return p, nil
	}
	if err := strictUnmarshal(raw, &p); err != nil {
		return Workspace{}, fmt.Errorf("workspace policy: %w", err)
	}
	if p.MaxTaskTimeoutSeconds < 0 || p.MonthlyTokenBudget < 0 || p.AgentMonthlyTokenBudget < 0 || p.ModelRequestsPerMinute < 0 || p.ApprovalTimeoutSeconds < 0 {
		return Workspace{}, fmt.Errorf("workspace policy: limits must not be negative")
	}
	if p.ToolRules != nil {
		rules, err := compileRules(*p.ToolRules)
		if err != nil {
			return Workspace{}, fmt.Errorf("workspace policy: %w", err)
		}
		if rules == nil {
			rules = []ToolRule{} // explicitly empty: deny everything
		}
		p.toolRules = rules
	}
	return p, nil
}

// TaskTimeoutCap returns the largest timeout a task may use.
func (p Workspace) TaskTimeoutCap() int {
	if p.MaxTaskTimeoutSeconds > 0 {
		return p.MaxTaskTimeoutSeconds
	}
	return 86400
}

func strictUnmarshal(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
