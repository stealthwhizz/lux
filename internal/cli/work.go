package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/client"
)

// ---- agents ----

func (a *App) agent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return a.agentList(ctx, rest)
	case "show", "inspect":
		return a.agentShow(ctx, rest)
	case "import":
		return a.agentImport(ctx, rest, false)
	case "validate":
		return a.agentImport(ctx, rest, true)
	case "sync":
		return a.agentSync(ctx, rest)
	case "enable", "disable":
		return a.agentEnable(ctx, rest, sub == "disable")
	case "run":
		return a.agentRun(ctx, rest)
	}
	return usageError{fmt.Sprintf("unknown agent subcommand %q", sub)}
}

func (a *App) agentList(ctx context.Context, args []string) error {
	if _, err := parse(a.newFlags("agent list"), args); err != nil {
		return err
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	agents, err := c.Agents(ctx)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(agents)
	}
	if len(agents) == 0 {
		fmt.Fprintln(a.Out, "No agents yet. Import one with `lux agent import PATH-OR-GIT-URL`.")
		return nil
	}
	var rows [][]string
	for _, ag := range agents {
		rev := "no valid revision"
		if ag.Revision != nil {
			rev = ag.Revision.CommitSHA[:12]
		}
		state := "enabled"
		if ag.Disabled {
			state = "disabled"
		}
		active := ag.TaskCounts["running"] + ag.TaskCounts["queued"] + ag.TaskCounts["starting"] + ag.TaskCounts["waiting_approval"]
		rows = append(rows, []string{ag.Slug, state, rev, fmt.Sprint(active), ag.Executor, truncate(ag.Description, 50)})
	}
	a.table("AGENT\tSTATE\tREVISION\tACTIVE\tEXECUTOR\tDESCRIPTION", rows)
	return nil
}

func (a *App) agentShow(ctx context.Context, args []string) error {
	pos, err := parse(a.newFlags("agent show"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"usage: lux agent show AGENT"}
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	ag, err := c.Agent(ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(ag)
	}
	fmt.Fprintf(a.Out, "%s (%s)\n%s\n\n", ag.Name, ag.ID, ag.Description)
	src := ag.SourceURL
	if ag.SourceRef != "" {
		src += " @ " + ag.SourceRef
	}
	fmt.Fprintf(a.Out, "Source     %s (%s)\nRuntime    %s\nExecutor   %s\n", src, ag.SourceKind, ag.Runtime, ag.Executor)
	if ag.Disabled {
		fmt.Fprintln(a.Out, "State      disabled")
	}
	if ag.Revision == nil {
		fmt.Fprintln(a.Out, "Revision   none valid; fix the repository and run `lux agent sync`")
	} else {
		fmt.Fprintf(a.Out, "Revision   %s (%s)\n", ag.Revision.CommitSHA, ag.Revision.CreatedAt.Local().Format(time.DateTime))
		var rep client.Report
		if json := ag.Revision.Validation; len(json) > 0 {
			_ = jsonUnmarshal(json, &rep)
		}
		a.printReport(rep)
	}
	if len(ag.TaskCounts) > 0 {
		var parts []string
		for _, k := range sortedKeysInt(ag.TaskCounts) {
			parts = append(parts, fmt.Sprintf("%s %d", k, ag.TaskCounts[k]))
		}
		fmt.Fprintf(a.Out, "Tasks      %s\n", strings.Join(parts, ", "))
	}
	return nil
}

func (a *App) printReport(rep client.Report) {
	for _, e := range rep.Errors {
		fmt.Fprintf(a.Out, "  error    %s: %s\n", orDash(e.Path), e.Message)
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(a.Out, "  warning  %s: %s\n", orDash(w.Path), w.Message)
	}
	if len(rep.CodeExecution) > 0 {
		fmt.Fprintf(a.Out, "  runs code on load: %s\n", strings.Join(rep.CodeExecution, ", "))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sourceFrom turns a CLI argument into an agent source: an existing local
// directory becomes an absolute local path; anything else is a git URL.
func sourceFrom(arg, ref string) client.AgentSource {
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		abs, _ := filepath.Abs(arg)
		return client.AgentSource{SourceKind: "local", SourceURL: abs, SourceRef: ref}
	}
	return client.AgentSource{SourceKind: "git", SourceURL: arg, SourceRef: ref}
}

func (a *App) agentImport(ctx context.Context, args []string, dryRun bool) error {
	fs := a.newFlags("agent import")
	ref := fs.String("ref", "", "branch or tag (default: the repository's default branch)")
	slug := fs.String("slug", "", "name for the agent in Lux (default: the manifest name)")
	cred := fs.String("credential", "", "git credential configured on the server, for private repositories")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"usage: lux agent import PATH-OR-GIT-URL [--ref REF] [--slug SLUG]"}
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	src := sourceFrom(pos[0], *ref)
	src.Credential = *cred
	if dryRun {
		ins, err := c.ValidateAgent(ctx, src)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(ins)
		}
		verdict := "valid"
		if !ins.Report.Valid {
			verdict = "INVALID"
		}
		fmt.Fprintf(a.Out, "%s at %s: %s\n", pos[0], ins.CommitSHA[:12], verdict)
		a.printReport(ins.Report)
		if !ins.Report.Valid {
			return exitError{ExitError, ""}
		}
		return nil
	}
	ag, rep, err := c.ImportAgent(ctx, src, *slug)
	if err != nil {
		var ae *client.Error
		if asErr(err, &ae) && ae.Code == "invalid_agent" && !a.json {
			var d struct{ Report client.Report }
			_ = jsonUnmarshal(ae.Details, &d)
			fmt.Fprintln(a.Out, "The repository did not pass validation:")
			a.printReport(d.Report)
		}
		return err
	}
	if a.json {
		return a.printJSON(map[string]any{"agent": ag, "report": rep})
	}
	fmt.Fprintf(a.Out, "Imported %s at %s.\n", ag.Slug, ag.Revision.CommitSHA[:12])
	a.printReport(*rep)
	return nil
}

func (a *App) agentSync(ctx context.Context, args []string) error {
	pos, err := parse(a.newFlags("agent sync"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"usage: lux agent sync AGENT"}
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	res, err := c.SyncAgent(ctx, pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(res)
	}
	switch {
	case !res.Report.Valid:
		fmt.Fprintln(a.Out, "The latest commit is invalid; the agent keeps running its previous revision.")
		a.printReport(res.Report)
		return exitError{ExitError, ""}
	case res.Changed:
		fmt.Fprintf(a.Out, "Updated %s to %s.\n", res.Agent.Slug, res.Agent.Revision.CommitSHA[:12])
	default:
		fmt.Fprintf(a.Out, "%s is up to date.\n", res.Agent.Slug)
	}
	return nil
}

func (a *App) agentEnable(ctx context.Context, args []string, disable bool) error {
	pos, err := parse(a.newFlags("agent enable"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"usage: lux agent enable|disable AGENT"}
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	ag, err := c.SetAgentDisabled(ctx, pos[0], disable)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(ag)
	}
	state := "enabled"
	if disable {
		state = "disabled"
	}
	fmt.Fprintf(a.Out, "%s is %s.\n", ag.Slug, state)
	return nil
}

func (a *App) agentRun(ctx context.Context, args []string) error {
	fs := a.newFlags("agent run")
	follow := fs.Bool("follow", false, "stream progress until the task ends")
	wait := fs.Bool("wait", false, "wait quietly for the task to end; exit 3 unless it succeeded")
	key := fs.String("idempotency-key", "", "retry-safe key: repeating the command returns the same task")
	repo := fs.String("repo", "", "repository (see `lux repo list`) the agent works on")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError{`usage: lux agent run AGENT "PROMPT" [--follow | --wait]`}
	}
	prompt := pos[1]
	if prompt == "-" {
		b, err := io.ReadAll(io.LimitReader(a.In, 1<<20))
		if err != nil {
			return err
		}
		prompt = string(b)
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	t, err := c.CreateTask(ctx, pos[0], prompt, *repo, *key)
	if err != nil {
		return err
	}
	if !*follow && !*wait {
		if a.json {
			return a.printJSON(t)
		}
		fmt.Fprintf(a.Out, "Started task %s. Follow it with `lux logs %s --follow`.\n", t.ID, t.ID)
		return nil
	}
	return a.followTask(ctx, c, t.ID, 0, *follow)
}

// ---- tasks ----

func (a *App) task(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		fs := a.newFlags("task list")
		agent := fs.String("agent", "", "only this agent")
		state := fs.String("state", "", "comma-separated states (e.g. running,waiting_approval)")
		limit := fs.Int("limit", 30, "how many")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		tasks, err := c.Tasks(ctx, client.TaskFilter{Agent: *agent, State: *state, Limit: *limit})
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(tasks)
		}
		if len(tasks) == 0 {
			fmt.Fprintln(a.Out, "No tasks.")
			return nil
		}
		var rows [][]string
		for _, t := range tasks {
			rows = append(rows, []string{t.ID, t.AgentName, t.State, ago(t.CreatedAt), truncate(oneLine(t.Prompt), 50)})
		}
		a.table("TASK\tAGENT\tSTATE\tCREATED\tPROMPT", rows)
		return nil
	case "show":
		pos, err := parse(a.newFlags("task show"), rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError{"usage: lux task show TASK"}
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		t, err := c.Task(ctx, pos[0])
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(t)
		}
		fmt.Fprintf(a.Out, "Task       %s\nAgent      %s @ %s\nState      %s", t.ID, t.AgentName, short(t.CommitSHA), t.State)
		if t.StateReason != "" {
			fmt.Fprintf(a.Out, " (%s)", t.StateReason)
		}
		fmt.Fprintf(a.Out, "\nExecutor   %s (isolation: %s)\nCreated    %s\nPrompt     %s\n", t.Executor, t.Isolation, t.CreatedAt.Local().Format(time.DateTime), t.Prompt)
		if t.ResultSummary != "" {
			fmt.Fprintf(a.Out, "\nResult\n%s\n", t.ResultSummary)
		}
		return nil
	case "cancel":
		pos, err := parse(a.newFlags("task cancel"), rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError{"usage: lux task cancel TASK"}
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		t, err := c.CancelTask(ctx, pos[0])
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(t)
		}
		fmt.Fprintf(a.Out, "Task %s is %s.\n", t.ID, t.State)
		return nil
	case "logs":
		return a.logs(ctx, rest)
	}
	return usageError{fmt.Sprintf("unknown task subcommand %q", sub)}
}

func (a *App) logs(ctx context.Context, args []string) error {
	fs := a.newFlags("logs")
	follow := fs.Bool("follow", false, "keep streaming until the task ends")
	fs.BoolVar(follow, "f", false, "same as --follow")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"usage: lux logs TASK [--follow]"}
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	if *follow {
		return a.followTask(ctx, c, pos[0], 0, true)
	}
	evs, err := c.Events(ctx, pos[0], 0)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(evs)
	}
	r := &renderer{out: a.Out, taskID: pos[0]}
	for _, ev := range evs {
		r.render(ev)
	}
	r.end()
	return nil
}

// followTask streams (or quietly waits for) a task and sets the exit code
// from its final state.
func (a *App) followTask(ctx context.Context, c *client.Client, taskID string, after int, show bool) error {
	r := &renderer{out: a.Out, json: a.json, taskID: taskID}
	err := c.FollowEvents(ctx, taskID, after, func(ev client.Event) {
		if show {
			r.render(ev)
		}
	})
	r.end()
	if err != nil {
		return err
	}
	t, err := c.Task(context.WithoutCancel(ctx), taskID)
	if err != nil {
		return err
	}
	if !a.json {
		if t.State == "succeeded" && t.ResultSummary != "" && !r.sawReply {
			fmt.Fprintf(a.Out, "\n%s\n", t.ResultSummary)
		}
		fmt.Fprintf(a.Out, "\nTask %s %s", t.ID, t.State)
		if t.StateReason != "" && t.State != "succeeded" {
			fmt.Fprintf(a.Out, ": %s", t.StateReason)
		}
		fmt.Fprintln(a.Out)
	} else if !show {
		a.printJSON(t)
	}
	if t.State != "succeeded" {
		return exitError{ExitTaskFailed, ""}
	}
	return nil
}

// renderer prints task events for people.
type renderer struct {
	out      io.Writer
	json     bool
	midLine  bool
	sawReply bool
	taskID   string
}

func (r *renderer) line(format string, args ...any) {
	if r.midLine {
		fmt.Fprintln(r.out)
		r.midLine = false
	}
	fmt.Fprintf(r.out, format+"\n", args...)
}

func (r *renderer) end() {
	if r.midLine {
		fmt.Fprintln(r.out)
		r.midLine = false
	}
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (r *renderer) render(ev client.Event) {
	if r.json {
		enc := jsonEncoder(r.out)
		enc.Encode(ev)
		return
	}
	p := ev.Payload
	switch ev.Type {
	case "task.state":
		msg := "state: " + str(p, "state")
		if reason := str(p, "reason"); reason != "" {
			msg += " (" + truncate(reason, 200) + ")"
		}
		r.line("· %s", msg)
	case "assistant.delta":
		fmt.Fprint(r.out, str(p, "text"))
		r.midLine = true
		r.sawReply = true
	case "assistant.message":
		if e := str(p, "error"); e != "" {
			r.line("! model error: %s", e)
		}
		r.end()
	case "tool.call":
		args, _ := p["args"].(map[string]any)
		detail := str(args, "command")
		if detail == "" {
			detail = str(args, "path")
		}
		r.line("→ %s %s", str(p, "name"), truncate(oneLine(detail), 120))
	case "tool.result":
		status := "ok"
		if p["is_error"] == true {
			status = "error"
		}
		r.line("  ← %s %s: %s", str(p, "name"), status, truncate(oneLine(str(p, "content")), 120))
	case "tool.blocked":
		r.line("✗ %s blocked: %s", str(p, "name"), str(p, "reason"))
	case "lux.approval_requested":
		// Written by luxd the moment approval is needed. (The runtime's own
		// tool.awaiting_approval arrives later, from its event file, so it
		// is not shown.)
		r.line("⏸ needs approval: %s. Decide with: lux approvals approve %s", str(p, "summary"), str(p, "approval_id"))
	case "lux.approval_decided":
		r.line("  approval %s: %s", str(p, "approval_id"), str(p, "decision"))
	case "lux.tool_denied":
		r.line("✗ %s denied by policy: %s", str(p, "tool"), str(p, "reason"))
	case "lux.model_fallback":
		r.line("· model fallback: %s → %s", str(p, "from"), str(p, "to"))
	case "lux.budget_exceeded":
		r.line("! %s", str(p, "message"))
	case "lux.changes":
		target := map[string]string{"repository": "the repository", "memory": "its memory"}[str(p, "target")]
		r.line("± changed %s: %v files, +%v -%v. Review with: lux changes show %s%s", target, p["files"], p["added"], p["deleted"],
			r.taskID, map[bool]string{true: " --memory"}[str(p, "target") == "memory"])
	case "lux.changes_error":
		r.line("! %s", str(p, "message"))
	case "lux.changes_published":
		r.line("± %s changes pushed to %s by %s", str(p, "target"), str(p, "branch"), str(p, "by"))
	case "run.error":
		r.line("! %s", str(p, "message"))
	case "runtime.invalid_event":
		r.line("! runtime: %s", str(p, "message"))
	}
}

// ---- approvals ----

func (a *App) approvals(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		fs := a.newFlags("approvals list")
		all := fs.Bool("all", false, "include decided approvals")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		state := "pending"
		if *all {
			state = ""
		}
		list, err := c.Approvals(ctx, state)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(list)
		}
		if len(list) == 0 {
			fmt.Fprintln(a.Out, "Nothing is waiting for approval.")
			return nil
		}
		var rows [][]string
		for _, ap := range list {
			rows = append(rows, []string{ap.ID, ap.State, ap.AgentName, ago(ap.CreatedAt), truncate(oneLine(ap.Summary), 70)})
		}
		a.table("APPROVAL\tSTATE\tAGENT\tREQUESTED\tACTION", rows)
		return nil
	case "approve", "reject":
		fs := a.newFlags("approvals " + sub)
		note := fs.String("note", "", "note recorded with the decision")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError{fmt.Sprintf("usage: lux approvals %s ID [--note TEXT]", sub)}
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		ap, err := c.Decide(ctx, pos[0], sub == "approve", *note)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(ap)
		}
		fmt.Fprintf(a.Out, "%s: %s\n", ap.State, ap.Summary)
		return nil
	}
	return usageError{fmt.Sprintf("unknown approvals subcommand %q", sub)}
}

// ---- chat ----

// chat runs an interactive conversation: each line on stdin is a message;
// the agent's progress and reply are printed. "/exit" or end of input quits.
func (a *App) chat(ctx context.Context, args []string) error {
	fs := a.newFlags("chat")
	convID := fs.String("conversation", "", "continue an existing conversation")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 && *convID == "" {
		return usageError{"usage: lux chat AGENT  (or --conversation ID)"}
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	interactive := isTerminal(a.In)
	sc := bufio.NewScanner(a.In)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if interactive {
		fmt.Fprintln(a.Out, "Type a message and press Enter. /exit to quit.")
	}
	for {
		if interactive {
			fmt.Fprint(a.Out, "\n> ")
		}
		if !sc.Scan() {
			return sc.Err()
		}
		msg := strings.TrimSpace(sc.Text())
		if msg == "" {
			continue
		}
		if msg == "/exit" || msg == "/quit" {
			return nil
		}
		var t *client.Task
		if *convID == "" {
			conv, task, err := c.CreateConversation(ctx, pos[0], msg)
			if err != nil {
				return err
			}
			*convID, t = conv.ID, task
			if !a.json {
				fmt.Fprintf(a.Out, "(conversation %s)\n", conv.ID)
			}
		} else {
			if t, err = c.SendMessage(ctx, *convID, msg); err != nil {
				return err
			}
		}
		// A failed turn is reported but does not end the chat.
		if err := a.followTask(ctx, c, t.ID, 0, true); err != nil {
			if _, ok := err.(exitError); !ok {
				return err
			}
		}
	}
}

// ---- helpers ----

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

func sortedKeysInt(m map[string]int) []string {
	conv := map[string]string{}
	for k := range m {
		conv[k] = ""
	}
	return sortedKeys(conv)
}
