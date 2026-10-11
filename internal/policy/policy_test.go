package policy

import "testing"

func TestParse(t *testing.T) {
	p, err := Parse([]byte(`{}`))
	if err != nil || p.AllowCodeExecutionOnLoad || p.RequireOpenGAP || p.TaskTimeoutCap() != 86400 {
		t.Fatalf("defaults = %+v, %v", p, err)
	}
	p, err = Parse(nil)
	if err != nil || p.AllowCodeExecutionOnLoad {
		t.Fatalf("nil = %+v, %v", p, err)
	}
	p, err = Parse([]byte(`{"allow_code_execution_on_load": true, "max_task_timeout_seconds": 600}`))
	if err != nil || !p.AllowCodeExecutionOnLoad || p.TaskTimeoutCap() != 600 {
		t.Fatalf("parsed = %+v, %v", p, err)
	}
	// A misspelt key must not silently leave a control at its default.
	if _, err := Parse([]byte(`{"allow_code_execution_onload": true}`)); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := Parse([]byte(`{"max_task_timeout_seconds": -1}`)); err == nil {
		t.Fatal("negative timeout accepted")
	}
	p, err = Parse([]byte(`{"allow_model_fallback": true, "monthly_token_budget": 1000000, "agent_monthly_token_budget": 5000, "model_requests_per_minute": 30}`))
	if err != nil || !p.AllowModelFallback || p.MonthlyTokenBudget != 1000000 || p.AgentMonthlyTokenBudget != 5000 || p.ModelRequestsPerMinute != 30 {
		t.Fatalf("gateway policy = %+v, %v", p, err)
	}
	for _, bad := range []string{`{"monthly_token_budget": -1}`, `{"model_requests_per_minute": -5}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
