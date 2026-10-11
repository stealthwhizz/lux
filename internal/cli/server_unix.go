//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/open-gitagent/lux/internal/config"
)

func pidFile() string { return filepath.Join(config.DefaultDataDir(), "luxd.pid") }
func logFile() string { return filepath.Join(config.DefaultDataDir(), "luxd.log") }

// runningPID returns the pid of a local luxd started by `lux server start`,
// or 0. It checks that the process is really luxd, not a reused pid.
func runningPID() int {
	b, err := os.ReadFile(pidFile())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return 0
	}
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil && !strings.Contains(string(cmdline), "luxd") {
		return 0
	}
	return pid
}

func findLuxd(cfg config.Client) (string, error) {
	if cfg.LocalServer.Binary != "" {
		return cfg.LocalServer.Binary, nil
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "luxd")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("luxd"); err == nil {
		return p, nil
	}
	return "", errors.New("luxd not found next to lux or on PATH (set local_server.binary in the config)")
}

func ready(ctx context.Context, url string) bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/readyz", nil)
	res, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == 200
}

func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (a *App) server(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	sub, rest := args[0], args[1:]
	fs := a.newFlags("server " + sub)
	dbURL := fs.String("database-url", "", "PostgreSQL URL for the local server (saved in the config)")
	envFile := fs.String("env-file", "", "settings and API keys for luxd (default: luxd.env in the config directory, if present)")
	if _, err := parse(fs, rest); err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	listen := cfg.LocalServer.Listen
	if listen == "" {
		listen = "127.0.0.1:7878"
	}
	url := "http://" + listen

	switch sub {
	case "status":
		pid := runningPID()
		ok := ready(ctx, url)
		if a.json {
			return a.printJSON(map[string]any{"pid": pid, "url": url, "ready": ok})
		}
		switch {
		case pid > 0 && ok:
			fmt.Fprintf(a.Out, "luxd is running (pid %d) at %s.\n", pid, url)
		case pid > 0:
			fmt.Fprintf(a.Out, "luxd is running (pid %d) but not ready at %s. Log: %s\n", pid, url, logFile())
			return exitError{ExitError, ""}
		case ok:
			fmt.Fprintf(a.Out, "A server is answering at %s, but it was not started by `lux server start`.\n", url)
		default:
			fmt.Fprintln(a.Out, "luxd is not running. Start it with `lux server start`.")
			return exitError{ExitError, ""}
		}
		return nil

	case "start":
		if pid := runningPID(); pid > 0 {
			fmt.Fprintf(a.Out, "luxd is already running (pid %d).\n", pid)
			return nil
		}
		if ready(ctx, url) {
			return exitError{ExitError, fmt.Sprintf("something else is already serving %s", url)}
		}
		if *dbURL != "" {
			cfg.LocalServer.DatabaseURL = *dbURL
			if err := cfg.Save(); err != nil {
				return err
			}
		}
		// Settings and API keys: the shell's environment wins over the file.
		// The default file is the user's own, never one in the current
		// directory: an env file decides which command luxd runs.
		path, explicit := *envFile, *envFile != ""
		if !explicit {
			path = filepath.Join(config.Dir(), "luxd.env")
		}
		fileEnv, err := loadEnvFile(path)
		if err != nil && (explicit || !os.IsNotExist(err)) {
			return exitError{ExitError, err.Error()}
		}
		db := os.Getenv("LUX_DATABASE_URL")
		if db == "" {
			db = envValue(fileEnv, "LUX_DATABASE_URL")
		}
		if db == "" {
			db = cfg.LocalServer.DatabaseURL
		}
		if db == "" {
			return usageError{"no database configured: pass --database-url postgres://… once (it is saved), or set LUX_DATABASE_URL"}
		}
		bin, err := findLuxd(cfg)
		if err != nil {
			return exitError{ExitError, err.Error()}
		}
		if err := os.MkdirAll(config.DefaultDataDir(), 0o700); err != nil {
			return err
		}
		logf, err := os.OpenFile(logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer logf.Close()
		cmd := exec.Command(bin, "serve")
		cmd.Env = append(append(fileEnv, os.Environ()...), "LUX_DATABASE_URL="+db, "LUX_LISTEN="+listen, "LUX_MODE=local")
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // keep running after this shell exits
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start luxd: %w", err)
		}
		pid := cmd.Process.Pid
		if err := os.WriteFile(pidFile(), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			return err
		}
		go cmd.Wait()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if ready(ctx, url) {
				if len(fileEnv) > 0 {
					fmt.Fprintf(a.Out, "Loaded %d settings from %s.\n", len(fileEnv), path)
				}
				fmt.Fprintf(a.Out, "luxd started (pid %d) at %s. Log: %s\n", pid, url, logFile())
				return nil
			}
			if err := syscall.Kill(pid, 0); err != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		_ = os.Remove(pidFile())
		return exitError{ExitError, "luxd did not become ready. Last log lines:\n" + tail(logFile(), 15)}

	case "stop":
		pid := runningPID()
		if pid == 0 {
			fmt.Fprintln(a.Out, "luxd is not running.")
			_ = os.Remove(pidFile())
			return nil
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			return fmt.Errorf("stop luxd: %w", err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if syscall.Kill(pid, 0) != nil {
				_ = os.Remove(pidFile())
				fmt.Fprintln(a.Out, "luxd stopped. Agent runs that were in progress keep going and are picked up when luxd starts again.")
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		return exitError{ExitError, fmt.Sprintf("luxd (pid %d) did not stop within 30s", pid)}
	}
	return usageError{fmt.Sprintf("unknown server subcommand %q", sub)}
}
