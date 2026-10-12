package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/policy"
	"github.com/open-gitagent/lux/internal/schedule"
	"github.com/open-gitagent/lux/internal/store"
)

// fireSchedules starts a task for every schedule that is due. Each task's
// idempotency key names the occurrence, so an occurrence starts at most one
// task even if luxd stops between creating the task and recording it.
// After downtime, a schedule fires once for its missed time and then
// continues from now; missed occurrences are not replayed.
func (o *Orchestrator) fireSchedules(ctx context.Context) {
	now := time.Now().UTC()
	due, err := o.Store.DueSchedules(ctx, now, 50)
	if err != nil {
		if ctx.Err() == nil {
			o.Log.Error("load due schedules", "err", err)
		}
		return
	}
	for _, sc := range due {
		at := *sc.NextRunAt
		var next *time.Time
		if n, ok := schedule.Next(sc.Mode, sc.Cron, sc.RunAt, now); ok && sc.Mode == "repeat" {
			next = &n
		}
		taskID, why := o.startScheduled(ctx, sc, at)
		if _, err := o.Store.AdvanceSchedule(ctx, sc.ID, at, next, taskID, why, now); err != nil {
			o.Log.Error("advance schedule", "schedule", sc.ID, "err", err)
		}
		if why != "" {
			o.Log.Warn("schedule did not start a task", "schedule", sc.ID, "agent", sc.AgentName, "reason", why)
		} else {
			o.Log.Info("schedule started a task", "schedule", sc.ID, "agent", sc.AgentName, "task", taskID)
		}
	}
}

// startScheduled applies the same checks as a task created through the
// API. It returns the task id, or why no task was started.
func (o *Orchestrator) startScheduled(ctx context.Context, sc store.Schedule, at time.Time) (string, string) {
	agent, err := o.Store.GetAgent(ctx, sc.WorkspaceID, sc.AgentID)
	switch {
	case err != nil:
		return "", "could not load the agent: " + err.Error()
	case agent.Disabled:
		return "", "the agent is disabled"
	case agent.Revision == nil:
		return "", "the agent has no valid revision"
	}
	prov, ok := o.Providers[agent.Executor]
	if !ok {
		return "", fmt.Sprintf("executor %q is not available on this server", agent.Executor)
	}
	ws, err := o.Store.WorkspaceByID(ctx, sc.WorkspaceID)
	if err != nil {
		return "", "could not load the workspace: " + err.Error()
	}
	pol, err := policy.Parse(ws.Policy)
	if err != nil {
		return "", "the workspace policy is invalid; an administrator must fix it"
	}
	var report agentrepo.Report
	if err := json.Unmarshal(agent.Revision.Validation, &report); err != nil {
		return "", "the agent's revision record is unreadable; sync the agent"
	}
	if len(report.CodeExecution) > 0 && !pol.AllowCodeExecutionOnLoad {
		return "", "the agent runs code when it loads and the workspace policy does not allow that"
	}
	timeout := 3600
	if report.Manifest != nil && report.Manifest.Runtime.Timeout > 0 {
		timeout = report.Manifest.Runtime.Timeout
	}
	timeout = min(timeout, pol.TaskTimeoutCap())
	convID := o.routineConversation(ctx, sc)
	t, created, err := o.Store.CreateTask(ctx, store.NewTask{
		WorkspaceID: sc.WorkspaceID, OrgID: ws.OrgID, AgentID: agent.ID, RevisionID: agent.Revision.ID, ScheduleID: sc.ID,
		ConversationID: convID, CreatedBy: sc.CreatedBy,
		Prompt: sc.Prompt, Executor: agent.Executor, Isolation: prov.Isolation(), MaxAttempts: 1, TimeoutSeconds: timeout,
		IdempotencyKey: fmt.Sprintf("schedule:%s:%d", sc.ID, at.Unix()), TriggerKind: "schedule",
	})
	if err != nil {
		return "", "could not create the task: " + err.Error()
	}
	if created && convID != "" {
		note := "Routine: " + sc.Name
		if sc.Kind == "follow_up" {
			note = sc.Name // already reads "Follow-up: …"
		}
		if _, err := o.Store.AddSystemMessage(ctx, convID, note, t.ID); err != nil {
			o.Log.Error("note routine in thread", "task", t.ID, "err", err)
		}
	}
	if err := o.Store.Audit(ctx, store.AuditEvent{OrgID: ws.OrgID, WorkspaceID: ws.ID, ActorKind: "schedule", ActorID: sc.ID,
		Action: "task.create", TargetKind: "task", TargetID: t.ID, Details: map[string]any{"schedule": sc.Name, "agent": agent.Slug, "due": at}}); err != nil {
		o.Log.Error("audit scheduled task", "task", t.ID, "err", err)
	}
	return t.ID, ""
}

// routineConversation is where a routine's result goes: the agent's thread
// with the routine's owner, unless that thread is busy (then the routine
// runs on its own and shows under all tasks). Repository schedules have
// no owner and always run on their own.
func (o *Orchestrator) routineConversation(ctx context.Context, sc store.Schedule) string {
	if sc.CreatedBy == "" {
		return ""
	}
	convs, err := o.Store.ListConversations(ctx, sc.WorkspaceID, sc.AgentID, 50)
	if err != nil {
		return ""
	}
	for _, c := range convs {
		if c.CreatedBy == sc.CreatedBy {
			if c.ActiveTaskID != "" {
				return ""
			}
			return c.ID
		}
	}
	c, err := o.Store.CreateConversation(ctx, sc.WorkspaceID, sc.AgentID, sc.Name, sc.CreatedBy)
	if err != nil {
		return ""
	}
	return c.ID
}
