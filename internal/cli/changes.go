package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-gitagent/lux/internal/client"
)

// ---- repositories ----

func (a *App) repo(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		if _, err := parse(a.newFlags("repo list"), args); err != nil {
			return err
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		list, err := c.Repositories(ctx)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(list)
		}
		if len(list) == 0 {
			fmt.Fprintln(a.Out, "No repositories. Add one with `lux repo add NAME GIT-URL`.")
			return nil
		}
		var rows [][]string
		for _, r := range list {
			rows = append(rows, []string{r.Name, r.SourceURL, orDash(r.DefaultBranch), orDash(r.Credential), orDash(r.TestCommand)})
		}
		a.table("NAME\tSOURCE\tBRANCH\tCREDENTIAL\tTESTS", rows)
		return nil
	case "add":
		fs := a.newFlags("repo add")
		branch := fs.String("branch", "", "branch tasks start from (default: the remote's default branch)")
		tests := fs.String("test-command", "", "command the agent is told to run the tests with")
		cred := fs.String("credential", "", "git credential configured on the server")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 2 {
			return usageError{"usage: lux repo add NAME GIT-URL [--branch B] [--test-command CMD] [--credential NAME]"}
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		src := sourceFrom(pos[1], "")
		r, err := c.AddRepository(ctx, client.Repository{Name: pos[0], SourceKind: src.SourceKind, SourceURL: src.SourceURL,
			DefaultBranch: *branch, TestCommand: *tests, Credential: *cred})
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(r)
		}
		fmt.Fprintf(a.Out, "Added %s. Run a task on it with `lux agent run AGENT \"PROMPT\" --repo %s`.\n", r.Name, r.Name)
		return nil
	case "remove", "rm":
		pos, err := parse(a.newFlags("repo remove"), args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError{"usage: lux repo remove NAME"}
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		if err := c.RemoveRepository(ctx, pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Removed %s.\n", pos[0])
		return nil
	}
	return usageError{fmt.Sprintf("unknown repo subcommand %q", sub)}
}

// ---- changes ----

func (a *App) changes(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if sub == "list" || sub == "ls" {
		fs := a.newFlags("changes list")
		all := fs.Bool("all", false, "include published and discarded changes")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := a.authed()
		if err != nil {
			return err
		}
		state := "proposed"
		if *all {
			state = ""
		}
		list, err := c.WorkspaceChanges(ctx, state)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(list)
		}
		if len(list) == 0 {
			fmt.Fprintln(a.Out, "No changes are waiting for review.")
			return nil
		}
		var rows [][]string
		for _, ch := range list {
			rows = append(rows, []string{ch.TaskID, ch.Target, ch.State, ch.AgentName, stat(ch), truncate(oneLine(ch.TaskPrompt), 50)})
		}
		a.table("TASK\tTARGET\tSTATE\tAGENT\tCHANGES\tPROMPT", rows)
		return nil
	}

	fs := a.newFlags("changes " + sub)
	memory := fs.Bool("memory", false, "the agent's memory and skills instead of the task's repository")
	pr := fs.Bool("pr", false, "also open a GitHub pull request (publish only)")
	title := fs.String("title", "", "commit and pull request title (publish only)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{fmt.Sprintf("usage: lux changes %s TASK [--memory]", sub)}
	}
	target := "repository"
	if *memory {
		target = "memory"
	}
	c, err := a.authed()
	if err != nil {
		return err
	}
	switch sub {
	case "show", "diff":
		list, err := c.TaskChanges(ctx, pos[0])
		if err != nil {
			return err
		}
		var ch *client.Change
		for i := range list {
			if list[i].Target == target {
				ch = &list[i]
			}
		}
		if ch == nil {
			fmt.Fprintf(a.Out, "Task %s made no %s changes.\n", pos[0], target)
			return nil
		}
		if a.json {
			return a.printJSON(ch)
		}
		fmt.Fprintf(a.Out, "%s changes, %s (%s)\n", ch.Target, ch.State, stat(*ch))
		if ch.LastError != "" {
			fmt.Fprintf(a.Out, "Last publish failed: %s\n", ch.LastError)
		}
		if ch.Branch != "" {
			fmt.Fprintf(a.Out, "Published to %s at %s %s\n", ch.Branch, short(ch.CommitSHA), ch.PullRequestURL)
		}
		if !ch.PatchAvailable {
			for _, f := range ch.Files {
				fmt.Fprintf(a.Out, "  %s +%d -%d\n", f.Path, f.Added, f.Deleted)
			}
			fmt.Fprintf(a.Out, "The patch (%d bytes) was too large to keep.\n", ch.PatchBytes)
			return nil
		}
		patch, err := c.Patch(ctx, pos[0], target)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Out)
		fmt.Fprint(a.Out, patch)
		return nil
	case "publish":
		res, err := c.Publish(ctx, pos[0], target, *pr, *title)
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(res)
		}
		fmt.Fprintf(a.Out, "Pushed %s at %s.\n", res.Change.Branch, short(res.Change.CommitSHA))
		if res.Change.PullRequestURL != "" {
			fmt.Fprintf(a.Out, "Pull request: %s\n", res.Change.PullRequestURL)
		}
		if res.PullRequestError != "" {
			fmt.Fprintf(a.Out, "The branch was pushed, but the pull request could not be opened: %s\n", res.PullRequestError)
			return exitError{ExitError, ""}
		}
		return nil
	case "discard":
		if _, err := c.Discard(ctx, pos[0], target); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Discarded the %s changes of %s.\n", target, pos[0])
		return nil
	}
	return usageError{fmt.Sprintf("unknown changes subcommand %q", sub)}
}

func stat(ch client.Change) string {
	added, deleted := 0, 0
	for _, f := range ch.Files {
		added, deleted = added+f.Added, deleted+f.Deleted
	}
	return fmt.Sprintf("%d files +%d -%d", len(ch.Files), added, deleted)
}
