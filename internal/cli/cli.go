// Package cli implements the `lux` command. All state changes go through
// the Lux API (internal/client); the CLI keeps only the shared client
// configuration (internal/config).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/open-gitagent/lux/internal/client"
	"github.com/open-gitagent/lux/internal/config"
)

// Exit codes, stable for scripts.
const (
	ExitOK         = 0
	ExitError      = 1
	ExitUsage      = 2
	ExitTaskFailed = 3 // a task the command waited for did not succeed
	ExitAuth       = 4 // missing or rejected credentials
)

// App holds the command's input and output.
type App struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer

	// Set by global flags.
	envName   string
	workspace string
	json      bool
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

// Run executes the lux command line and returns the exit code.
func Run(args []string, in io.Reader, out, errw io.Writer) int {
	a := &App{In: in, Out: out, Err: errw}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := a.dispatch(ctx, args)
	if err == nil {
		return ExitOK
	}
	var ue usageError
	var ee exitError
	var ae *client.Error
	switch {
	case errors.As(err, &ue):
		fmt.Fprintln(errw, "lux:", ue.msg)
		fmt.Fprintln(errw, "Run `lux help` for usage.")
		return ExitUsage
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(errw, "lux:", ee.msg)
		}
		return ee.code
	case errors.As(err, &ae) && ae.Status == 401:
		fmt.Fprintln(errw, "lux: not signed in to this environment (or the token was revoked). Run `lux login`.")
		return ExitAuth
	case errors.As(err, &ae):
		msg := ae.Message
		if ae.Code != "" {
			msg += " [" + ae.Code + "]"
		}
		fmt.Fprintln(errw, "lux:", msg)
		if len(ae.Details) > 0 && string(ae.Details) != "null" && a.json {
			fmt.Fprintln(errw, string(ae.Details))
		}
		return ExitError
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(errw, "lux: interrupted")
		return ExitError
	default:
		fmt.Fprintln(errw, "lux:", err)
		return ExitError
	}
}

// globalFlags adds the flags every command accepts.
func (a *App) globalFlags(fs *flag.FlagSet) {
	fs.StringVar(&a.envName, "env", "", "environment to use (default: the current one; see `lux env`)")
	fs.StringVar(&a.workspace, "workspace", "", "workspace slug (default: the environment's, else \"default\")")
	fs.BoolVar(&a.json, "json", false, "print machine-readable JSON")
}

func (a *App) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a.globalFlags(fs)
	return fs
}

// parse parses flags, allowing them before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, usageError{"see `lux help`"}
			}
			return nil, usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// connect returns an API client for the selected environment.
func (a *App) connect() (*client.Client, string, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, "", err
	}
	name, env, err := cfg.Env(a.envName)
	if err != nil {
		return nil, "", usageError{err.Error()}
	}
	tok, err := config.Token(name)
	if err != nil {
		return nil, "", err
	}
	ws := a.workspace
	if ws == "" {
		ws = env.Workspace
	}
	return client.New(env.URL, tok, ws), name, nil
}

func (a *App) authed() (*client.Client, error) {
	c, name, err := a.connect()
	if err != nil {
		return nil, err
	}
	if c.Token == "" {
		return nil, exitError{ExitAuth, fmt.Sprintf("no token for environment %q; run `lux login --env %s`", name, name)}
	}
	return c, nil
}

func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *App) table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(a.Out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func (a *App) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		a.help()
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		a.help()
		return nil
	case "version", "--version":
		return a.version(ctx, rest)
	case "status":
		return a.status(ctx, rest)
	case "doctor":
		return a.doctor(ctx, rest)
	case "login":
		return a.login(ctx, rest)
	case "logout":
		return a.logout(rest)
	case "env":
		return a.env(rest)
	case "config":
		return a.configCmd(rest)
	case "server":
		return a.server(ctx, rest)
	case "agent", "agents":
		return a.agent(ctx, rest)
	case "task", "tasks":
		return a.task(ctx, rest)
	case "logs":
		return a.logs(ctx, rest)
	case "approvals", "approval":
		return a.approvals(ctx, rest)
	case "repo", "repos":
		return a.repo(ctx, rest)
	case "changes", "change":
		return a.changes(ctx, rest)
	case "chat":
		return a.chat(ctx, rest)
	}
	return usageError{fmt.Sprintf("unknown command %q", cmd)}
}

func (a *App) help() {
	fmt.Fprint(a.Out, `lux: run and supervise AI agents

Usage: lux <command> [arguments] [--env NAME] [--workspace SLUG] [--json]

Connection
  status                         server and sign-in status
  doctor                         check this machine and the server
  login [--env N] [--url URL]    sign in; reads the token from stdin
  logout [--env N]               forget the stored token
  env [list|use N|add N URL|remove N]
  config                         show the configuration file paths and values
  server start|stop|status       run a local luxd in the background

Agents
  agent list
  agent show AGENT
  agent import PATH|GIT-URL [--ref REF] [--slug SLUG]
  agent validate PATH|GIT-URL [--ref REF]
  agent sync AGENT
  agent enable|disable AGENT
  agent import ... [--credential NAME]   for private repositories
  agent run AGENT "PROMPT" [--follow | --wait] [--repo NAME]

Tasks
  task list [--agent A] [--state S]
  task show TASK
  task cancel TASK
  logs TASK [--follow]

Approvals
  approvals [list] [--all]
  approvals approve ID [--note TEXT]
  approvals reject ID [--note TEXT]

Repositories and changes
  repo [list]
  repo add NAME GIT-URL [--branch B] [--test-command CMD] [--credential NAME]
  repo remove NAME
  changes [list] [--all]         changes waiting for review
  changes show TASK [--memory]   the patch, exactly as it would be pushed
  changes publish TASK [--memory] [--pr] [--title T]
  changes discard TASK [--memory]

Conversations
  chat AGENT                     interactive conversation (stdin)

Exit codes: 0 ok, 1 error, 2 usage, 3 task did not succeed, 4 not signed in.
`)
}

func sortedKeys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
