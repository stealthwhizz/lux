package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/open-gitagent/lux/internal/client"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/version"
)

func (a *App) version(ctx context.Context, args []string) error {
	fs := a.newFlags("version")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	out := map[string]any{"cli": version.Version, "commit": version.Commit, "api": version.APIVersion}
	if c, _, err := a.connect(); err == nil {
		if s, err := c.Status(ctx); err == nil {
			out["server"] = s.Version
		}
	}
	if a.json {
		return a.printJSON(out)
	}
	fmt.Fprintf(a.Out, "lux %s (API %s)\n", version.Version, version.APIVersion)
	if s, ok := out["server"]; ok {
		fmt.Fprintf(a.Out, "server %s\n", s)
	}
	return nil
}

func (a *App) status(ctx context.Context, args []string) error {
	fs := a.newFlags("status")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, name, err := a.connect()
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		if a.json {
			a.printJSON(map[string]any{"environment": name, "url": c.BaseURL, "reachable": false, "error": err.Error()})
		}
		return exitError{ExitError, fmt.Sprintf("environment %q (%s) is not reachable: %v", name, c.BaseURL, err)}
	}
	var me *client.Me
	var meErr error
	if c.Token != "" {
		me, meErr = c.Me(ctx)
	}
	if a.json {
		out := map[string]any{"environment": name, "url": c.BaseURL, "reachable": true, "server": st, "signed_in": me != nil}
		if me != nil {
			out["user"] = me
		}
		return a.printJSON(out)
	}
	fmt.Fprintf(a.Out, "Environment  %s (%s)\n", name, c.BaseURL)
	fmt.Fprintf(a.Out, "Server       luxd %s, %s mode, schema v%d\n", st.Version, st.Mode, st.SchemaVersion)
	for _, k := range sortedKeys(st.Components) {
		fmt.Fprintf(a.Out, "  %-18s %s\n", k, st.Components[k])
	}
	for _, e := range st.Executors {
		avail := "available"
		if !e.Available {
			avail = "unavailable"
		}
		fmt.Fprintf(a.Out, "  executor %-9s %s, isolation: %s\n", e.Name, avail, e.Isolation)
	}
	var provs []string
	for _, p := range st.ModelProviders {
		provs = append(provs, p.Name)
	}
	fmt.Fprintf(a.Out, "  model providers    %s\n", orNone(strings.Join(provs, ", ")))
	switch {
	case c.Token == "":
		fmt.Fprintln(a.Out, "Signed in    no (run `lux login`)")
	case meErr != nil:
		fmt.Fprintf(a.Out, "Signed in    no: %v\n", meErr)
		return exitError{ExitAuth, ""}
	default:
		var wss []string
		for _, w := range me.Workspaces {
			wss = append(wss, w.Slug+" ("+w.Role+")")
		}
		fmt.Fprintf(a.Out, "Signed in    %s; workspaces: %s\n", me.Username, strings.Join(wss, ", "))
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// login stores a token for an environment after checking it with the server.
// The token is read from stdin so it never appears in shell history or ps.
func (a *App) login(ctx context.Context, args []string) error {
	fs := a.newFlags("login")
	rawURL := fs.String("url", "", "server URL (adds or updates the environment)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	name := a.envName
	if name == "" {
		name = cfg.Current
	}
	env := cfg.Environments[name]
	if *rawURL != "" {
		u, err := url.Parse(*rawURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return usageError{"--url must be an http(s) URL"}
		}
		if u.Scheme == "http" && !isLoopback(u.Hostname()) {
			fmt.Fprintln(a.Err, "warning: this server uses plain HTTP; the token will travel unencrypted")
		}
		env.URL = strings.TrimRight(u.String(), "/")
	}
	if env.URL == "" {
		return usageError{fmt.Sprintf("environment %q has no URL; pass --url", name)}
	}
	if isTerminal(a.In) {
		fmt.Fprintf(a.Err, "Paste an API token for %s and press Enter: ", env.URL)
	}
	line, err := bufio.NewReader(a.In).ReadString('\n')
	tok := strings.TrimSpace(line)
	if tok == "" {
		if err != nil {
			return usageError{"no token on stdin"}
		}
		return usageError{"empty token"}
	}
	c := client.New(env.URL, tok, env.Workspace)
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}
	cfg.Environments[name] = env
	if cfg.Current == "" {
		cfg.Current = name
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if err := config.SetToken(name, tok); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(map[string]any{"environment": name, "user": me.Username})
	}
	fmt.Fprintf(a.Out, "Signed in to %s as %s.\n", name, me.Username)
	return nil
}

func isLoopback(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (a *App) logout(args []string) error {
	fs := a.newFlags("logout")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	name := a.envName
	if name == "" {
		name = cfg.Current
	}
	if err := config.SetToken(name, ""); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Removed the stored token for %s. (The token itself is still valid on the server until revoked.)\n", name)
	return nil
}

func (a *App) env(args []string) error {
	fs := a.newFlags("env")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	sub := "list"
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "list":
		if a.json {
			return a.printJSON(cfg)
		}
		var rows [][]string
		for _, n := range cfg.Names() {
			e := cfg.Environments[n]
			cur := ""
			if n == cfg.Current {
				cur = "*"
			}
			tok, _ := config.Token(n)
			signed := "no"
			if tok != "" {
				signed = "yes"
			}
			rows = append(rows, []string{cur, n, e.URL, signed})
		}
		a.table("\tNAME\tURL\tTOKEN", rows)
		return nil
	case "use":
		if len(pos) != 2 {
			return usageError{"usage: lux env use NAME"}
		}
		if _, ok := cfg.Environments[pos[1]]; !ok {
			return usageError{fmt.Sprintf("no environment %q", pos[1])}
		}
		cfg.Current = pos[1]
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Now using %s.\n", pos[1])
		return nil
	case "add":
		if len(pos) != 3 {
			return usageError{"usage: lux env add NAME URL"}
		}
		u, err := url.Parse(pos[2])
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return usageError{"URL must be http(s)://host[:port]"}
		}
		cfg.Environments[pos[1]] = config.Environment{URL: strings.TrimRight(pos[2], "/")}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Added %s. Sign in with `lux login --env %s`.\n", pos[1], pos[1])
		return nil
	case "remove":
		if len(pos) != 2 {
			return usageError{"usage: lux env remove NAME"}
		}
		if pos[1] == cfg.Current {
			return usageError{"cannot remove the current environment; `lux env use` another first"}
		}
		delete(cfg.Environments, pos[1])
		_ = config.SetToken(pos[1], "")
		return cfg.Save()
	}
	return usageError{fmt.Sprintf("unknown env subcommand %q", sub)}
}

func (a *App) configCmd(args []string) error {
	fs := a.newFlags("config")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	info := map[string]any{
		"config_dir":   config.Dir(),
		"config_file":  filepath.Join(config.Dir(), "config.yaml"),
		"credentials":  filepath.Join(config.Dir(), "credentials.yaml") + " (tokens, mode 0600; contents not shown)",
		"current":      cfg.Current,
		"environments": cfg.Environments,
	}
	if a.json {
		return a.printJSON(info)
	}
	fmt.Fprintf(a.Out, "Config directory  %s\nCurrent           %s\n", config.Dir(), cfg.Current)
	for _, n := range cfg.Names() {
		e := cfg.Environments[n]
		fmt.Fprintf(a.Out, "  %-12s %s", n, e.URL)
		if e.Workspace != "" {
			fmt.Fprintf(a.Out, " (workspace %s)", e.Workspace)
		}
		fmt.Fprintln(a.Out)
	}
	return nil
}

type check struct {
	name string
	ok   bool
	warn bool
	msg  string
}

// doctor checks the local machine and the selected server. It exits 1 if
// any check fails.
func (a *App) doctor(ctx context.Context, args []string) error {
	fs := a.newFlags("doctor")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	var checks []check
	add := func(name string, ok bool, msg string) { checks = append(checks, check{name: name, ok: ok, msg: msg}) }
	warn := func(name, msg string) { checks = append(checks, check{name: name, ok: true, warn: true, msg: msg}) }

	cfg, err := config.LoadClient()
	add("client config", err == nil, errText(err, config.Dir()))
	if p, err := exec.LookPath("git"); err == nil {
		add("git", true, p)
	} else {
		warn("git", "not found; needed to import agents on a local server")
	}
	if p, err := exec.LookPath("node"); err == nil {
		add("node", true, p)
	} else {
		warn("node", "not found; a local server needs Node.js 20+ to run gitagent agents")
	}
	if err == nil {
		name, env, eerr := cfg.Env(a.envName)
		if eerr != nil {
			add("environment", false, eerr.Error())
		} else {
			add("environment", true, name+" → "+env.URL)
			tok, _ := config.Token(name)
			c := client.New(env.URL, tok, env.Workspace)
			st, serr := c.Status(ctx)
			add("server reachable", serr == nil, errText(serr, env.URL))
			if serr == nil && st.Components == nil {
				warn("server details", "a server-mode luxd shows them only after you sign in")
			} else if serr == nil {
				add("server ready", st.Components["database"] == "ok", "database: "+st.Components["database"])
				orch := st.Components["orchestrator"]
				add("orchestrator", orch == "running", orch)
				rt := st.Components["runtime"]
				add("agent runtime", rt == "ok", rt)
				if len(st.ModelProviders) == 0 {
					warn("model providers", "none configured; agents cannot call models")
				} else {
					var names []string
					keyed := false
					for _, p := range st.ModelProviders {
						names = append(names, p.Name)
						keyed = keyed || p.HasKey
					}
					if keyed {
						add("model providers", true, strings.Join(names, ", "))
					} else {
						warn("model providers", strings.Join(names, ", ")+" (no API keys configured; only keyless providers such as Ollama)")
					}
				}
				for _, e := range st.Executors {
					if e.Available && e.Isolation == "none" {
						warn("executor "+e.Name, "agents run without isolation, as the server's OS user")
					}
				}
			}
			if tok == "" {
				add("signed in", false, "no token; run `lux login`")
			} else if _, merr := c.Me(ctx); merr != nil {
				add("signed in", false, merr.Error())
			} else {
				add("signed in", true, "")
			}
		}
	}
	failed := false
	for _, c := range checks {
		if !c.ok {
			failed = true
		}
	}
	if a.json {
		var out []map[string]any
		for _, c := range checks {
			status := "ok"
			if !c.ok {
				status = "fail"
			} else if c.warn {
				status = "warn"
			}
			out = append(out, map[string]any{"check": c.name, "status": status, "detail": c.msg})
		}
		a.printJSON(map[string]any{"ok": !failed, "checks": out})
	} else {
		for _, c := range checks {
			mark := "ok  "
			if !c.ok {
				mark = "FAIL"
			} else if c.warn {
				mark = "warn"
			}
			fmt.Fprintf(a.Out, "[%s] %-18s %s\n", mark, c.name, c.msg)
		}
	}
	if failed {
		return exitError{ExitError, ""}
	}
	return nil
}

func errText(err error, okText string) string {
	if err != nil {
		return err.Error()
	}
	return okText
}
