package policy

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Tool rule actions.
const (
	ActionAllow           = "allow"
	ActionDeny            = "deny"
	ActionRequireApproval = "require_approval"
)

// Conditions a rule can test besides tool name and pattern.
const (
	// WhenPathOutsideAgentDir matches when the tool's `path` argument is
	// absolute, starts with ~, or climbs out with "..". (Symlinks inside
	// the checkout are refused at import; see agentrepo.)
	WhenPathOutsideAgentDir = "path_outside_agent_dir"
	// WhenPathInWorkspace matches when the `path` argument stays inside the
	// agent's workspace/ folder, which Lux keeps and shows to its owner.
	WhenPathInWorkspace = "path_in_workspace"
)

// ToolRule is one entry of a workspace's tool policy. Rules are checked in
// order; the first that matches decides. If none matches, the call is
// denied.
type ToolRule struct {
	// Tool is a tool name, or "*" for any tool.
	Tool string `json:"tool"`
	// Match is an optional regular expression (RE2 syntax) tested against
	// the call's `command` argument, else its `path`, else its JSON args.
	Match string `json:"match,omitempty"`
	// When is an optional named condition (see the When* constants).
	When string `json:"when,omitempty"`
	// Action is allow, deny or require_approval.
	Action string `json:"action"`
	// Reason is shown to the agent and to approvers.
	Reason string `json:"reason,omitempty"`

	re *regexp.Regexp
}

// DefaultToolRules apply when a workspace sets no tool_rules. They are
// strict because the local executor does not isolate agents.
func DefaultToolRules() []ToolRule {
	return []ToolRule{
		{Tool: "cli", Action: ActionRequireApproval, Reason: "shell commands need approval"},
		{Tool: "write", When: WhenPathInWorkspace, Action: ActionAllow, Reason: "files in the agent's own workspace"},
		{Tool: "edit", When: WhenPathInWorkspace, Action: ActionAllow, Reason: "files in the agent's own workspace"},
		{Tool: "write", Action: ActionRequireApproval, Reason: "writing files needs approval"},
		{Tool: "edit", Action: ActionRequireApproval, Reason: "editing files needs approval"},
		{Tool: "capture_photo", Action: ActionRequireApproval, Reason: "using the camera needs approval"},
		{Tool: "read", When: WhenPathOutsideAgentDir, Action: ActionRequireApproval, Reason: "reading outside the agent's directory needs approval"},
		{Tool: "read", Action: ActionAllow},
		{Tool: "memory", Action: ActionAllow},
		// Connectors (tools luxd serves with the person's own accounts): reading
		// is allowed, anything that sends or creates needs approval.
		{Tool: "lux__gmail_search", Action: ActionAllow},
		{Tool: "lux__gmail_read", Action: ActionAllow},
		{Tool: "lux__calendar_events", Action: ActionAllow},
		{Tool: "lux__gmail_draft", Action: ActionRequireApproval, Reason: "saving an email draft needs approval"},
		{Tool: "lux__gmail_send", Action: ActionRequireApproval, Reason: "sending email needs approval"},
		{Tool: "lux__calendar_create", Action: ActionRequireApproval, Reason: "adding calendar events needs approval"},
		{Tool: "lux__update_status", Action: ActionAllow},
		{Tool: "lux__follow_up", Action: ActionAllow},
		{Tool: "lux__browser_open", Action: ActionAllow},
		{Tool: "lux__browser_read", Action: ActionAllow},
		{Tool: "lux__browser_back", Action: ActionAllow},
		{Tool: "lux__browser_click", Action: ActionRequireApproval, Reason: "clicking on a website needs approval"},
		{Tool: "lux__browser_type", Action: ActionRequireApproval, Reason: "typing into a website needs approval"},
		{Tool: "lux__drive_search", Action: ActionAllow},
		{Tool: "lux__drive_read", Action: ActionAllow},
		{Tool: "lux__outlook_search", Action: ActionAllow},
		{Tool: "lux__outlook_read", Action: ActionAllow},
		{Tool: "lux__outlook_events", Action: ActionAllow},
		{Tool: "lux__onedrive_search", Action: ActionAllow},
		{Tool: "lux__onedrive_read", Action: ActionAllow},
		{Tool: "lux__outlook_send", Action: ActionRequireApproval, Reason: "sending email needs approval"},
		{Tool: "lux__outlook_create_event", Action: ActionRequireApproval, Reason: "adding calendar events needs approval"},
		{Tool: "lux__slack_search", Action: ActionAllow},
		{Tool: "lux__slack_channels", Action: ActionAllow},
		{Tool: "lux__slack_read", Action: ActionAllow},
		{Tool: "lux__slack_send", Action: ActionRequireApproval, Reason: "posting in Slack needs approval"},
		{Tool: "task_tracker", Action: ActionAllow},
		{Tool: "skill_learner", Action: ActionAllow},
	}
}

func compileRules(rules []ToolRule) ([]ToolRule, error) {
	out := make([]ToolRule, len(rules))
	for i, r := range rules {
		if r.Tool == "" {
			return nil, fmt.Errorf("tool_rules[%d]: tool is required (use \"*\" for any tool)", i)
		}
		switch r.Action {
		case ActionAllow, ActionDeny, ActionRequireApproval:
		default:
			return nil, fmt.Errorf("tool_rules[%d]: action must be allow, deny or require_approval", i)
		}
		switch r.When {
		case "", WhenPathOutsideAgentDir, WhenPathInWorkspace:
		default:
			return nil, fmt.Errorf("tool_rules[%d]: unknown condition %q", i, r.When)
		}
		if r.Match != "" {
			re, err := regexp.Compile(r.Match)
			if err != nil {
				return nil, fmt.Errorf("tool_rules[%d]: match: %v", i, err)
			}
			r.re = re
		}
		out[i] = r
	}
	return out, nil
}

// Decision is the outcome of evaluating a tool call.
type Decision struct {
	Action string `json:"action"`
	// Rule describes the rule that decided, for audit and display.
	Rule   string `json:"rule"`
	Reason string `json:"reason,omitempty"`
}

// Supervision is the part of an agent's compliance settings that affects
// tool calls (OpenGAP compliance.supervision / segregation_of_duties).
type Supervision struct {
	// HumanInTheLoopAlways: every tool call needs approval.
	HumanInTheLoopAlways bool
	// StrictSOD: the approver must differ from the task's creator.
	StrictSOD bool
}

// SupervisionFromManifest reads the relevant settings from agent.yaml (as JSON).
func SupervisionFromManifest(manifest []byte) Supervision {
	var m struct {
		Compliance struct {
			Supervision struct {
				HumanInTheLoop string `json:"human_in_the_loop"`
			} `json:"supervision"`
			SOD struct {
				Enforcement string `json:"enforcement"`
			} `json:"segregation_of_duties"`
		} `json:"compliance"`
	}
	_ = json.Unmarshal(manifest, &m)
	return Supervision{
		HumanInTheLoopAlways: m.Compliance.Supervision.HumanInTheLoop == "always",
		StrictSOD:            m.Compliance.SOD.Enforcement == "strict",
	}
}

// subject is what a rule's Match is tested against.
func subject(args map[string]any) string {
	if s, ok := args["command"].(string); ok {
		return s
	}
	if s, ok := args["path"].(string); ok {
		return s
	}
	b, _ := json.Marshal(args)
	return string(b)
}

// PathOutsideAgentDir reports whether a tool path may leave the agent's checkout.
func PathOutsideAgentDir(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "\\") || (len(p) > 1 && p[1] == ':') {
		return true
	}
	clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	return clean == ".." || strings.HasPrefix(clean, "../")
}

// PathInWorkspace reports whether a relative path stays inside workspace/.
func PathInWorkspace(p string) bool {
	if p == "" || PathOutsideAgentDir(p) {
		return false
	}
	clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	return strings.HasPrefix(clean, "workspace/")
}

// EvaluateTool decides a tool call. Anything unexpected denies.
func (p Workspace) EvaluateTool(tool string, args map[string]any, sup Supervision) Decision {
	if sup.HumanInTheLoopAlways {
		return Decision{Action: ActionRequireApproval, Rule: "agent compliance: human_in_the_loop = always",
			Reason: "this agent requires human approval for every action"}
	}
	rules := p.toolRules
	if rules == nil {
		rules, _ = compileRules(DefaultToolRules())
	}
	for i, r := range rules {
		if r.Tool != "*" && r.Tool != tool {
			continue
		}
		if r.When == WhenPathOutsideAgentDir {
			ps, _ := args["path"].(string)
			if !PathOutsideAgentDir(ps) {
				continue
			}
		}
		if r.When == WhenPathInWorkspace {
			ps, _ := args["path"].(string)
			if !PathInWorkspace(ps) {
				continue
			}
		}
		if r.re != nil && !r.re.MatchString(subject(args)) {
			continue
		}
		label := fmt.Sprintf("tool_rules[%d]", i)
		if p.ToolRules == nil {
			label = fmt.Sprintf("default rule %d", i)
		}
		return Decision{Action: r.Action, Rule: label, Reason: r.Reason}
	}
	return Decision{Action: ActionDeny, Rule: "no matching rule", Reason: "no tool rule allows " + tool}
}
