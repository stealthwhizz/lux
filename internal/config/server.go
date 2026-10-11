// Package config loads luxd server settings and the shared client config.
package config

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Server is luxd's configuration. Every value comes from the environment so
// secrets never need to live in a file in the repository.
type Server struct {
	Mode        string // "local" or "server"
	Listen      string // host:port
	PublicURL   string // URL runtimes use to reach the API and gateway
	DatabaseURL string
	DataDir     string // run directories, agent checkouts, repository mirrors
	RunnerCmd   []string
	// RunnerNote explains how RunnerCmd was found, or why it was not.
	RunnerNote    string
	MaxConcurrent int
	// AllowLocalExecutor permits the non-isolated local executor in server mode.
	AllowLocalExecutor bool
	// Bwrap is the bubblewrap binary for the sandbox executor ("" when it
	// is unavailable or LUX_SANDBOX=off); SandboxNote says why not.
	Bwrap, SandboxNote string
	// AllowLocalSources permits importing agents from paths on the luxd
	// host. Always on in local mode; off by default in server mode, where it
	// would let any workspace admin read repositories on the server.
	AllowLocalSources bool
	// OpenGAPCmd runs the reference validator, e.g. "npx --no-install opengap".
	OpenGAPCmd     string
	LogFormat      string // "json" or "text"
	Providers      Providers
	GitCredentials map[string]string // credential name -> token (from LUX_GIT_CREDENTIAL_<NAME>)
	// GoogleClientID and GoogleClientSecret are the OAuth client people use
	// to connect Google (LUX_GOOGLE_CLIENT_ID / LUX_GOOGLE_CLIENT_SECRET).
	GoogleClientID, GoogleClientSecret string
	// GitHubAPIURL is where pull requests are opened for github.com remotes.
	GitHubAPIURL string
}

// Providers holds model-provider endpoints and keys for the gateway.
type Providers struct {
	OpenAIKey          string
	OpenAIBase         string
	AnthropicKey       string
	AnthropicBase      string
	AnthropicWorkspace string
	LyzrKey            string
	LyzrBase           string
	OllamaBase         string
	OllamaDisabled     bool
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// LoadServer reads luxd settings from the environment.
func LoadServer() (Server, error) {
	mode := env("LUX_MODE", "local")
	if mode != "local" && mode != "server" {
		return Server{}, fmt.Errorf("LUX_MODE must be \"local\" or \"server\", got %q", mode)
	}
	listen := env("LUX_LISTEN", "127.0.0.1:7878")
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return Server{}, fmt.Errorf("LUX_LISTEN %q: %w", listen, err)
	}
	if mode == "local" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return Server{}, fmt.Errorf("local mode only listens on loopback; set LUX_MODE=server to listen on %s", host)
	}
	pubHost := host
	if pubHost == "" || pubHost == "0.0.0.0" || pubHost == "::" {
		pubHost = "127.0.0.1"
	}
	dataDir := env("LUX_DATA_DIR", DefaultDataDir())
	allowLocal, _ := strconv.ParseBool(env("LUX_ALLOW_LOCAL_EXECUTOR", "false"))
	allowLocalSources, _ := strconv.ParseBool(env("LUX_ALLOW_LOCAL_SOURCES", "false"))
	s := Server{
		Mode:               mode,
		Listen:             listen,
		PublicURL:          strings.TrimRight(env("LUX_PUBLIC_URL", "http://"+net.JoinHostPort(pubHost, port)), "/"),
		DatabaseURL:        os.Getenv("LUX_DATABASE_URL"),
		DataDir:            dataDir,
		AllowLocalExecutor: mode == "local" || allowLocal,
		AllowLocalSources:  mode == "local" || allowLocalSources,
		OpenGAPCmd:         os.Getenv("LUX_OPENGAP_CMD"),
		LogFormat:          env("LUX_LOG_FORMAT", map[bool]string{true: "text", false: "json"}[mode == "local"]),
		Providers: Providers{
			OpenAIKey:          firstEnv("LUX_OPENAI_API_KEY", "OPENAI_API_KEY"),
			OpenAIBase:         os.Getenv("LUX_OPENAI_BASE_URL"),
			AnthropicKey:       firstEnv("LUX_ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"),
			AnthropicBase:      os.Getenv("LUX_ANTHROPIC_BASE_URL"),
			AnthropicWorkspace: os.Getenv("LUX_ANTHROPIC_WORKSPACE_ID"),
			LyzrKey:            firstEnv("LUX_LYZR_API_KEY", "LYZR_API_KEY"),
			LyzrBase:           os.Getenv("LUX_LYZR_BASE_URL"),
			OllamaBase:         os.Getenv("LUX_OLLAMA_BASE_URL"),
			OllamaDisabled:     os.Getenv("LUX_OLLAMA_DISABLED") == "true",
		},
		GitCredentials:     map[string]string{},
		GoogleClientID:     os.Getenv("LUX_GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("LUX_GOOGLE_CLIENT_SECRET"),
		GitHubAPIURL:       strings.TrimRight(env("LUX_GITHUB_API_URL", "https://api.github.com"), "/"),
	}
	s.RunnerCmd, s.RunnerNote = ResolveRunner()
	s.MaxConcurrent = 4
	if v := os.Getenv("LUX_MAX_CONCURRENT_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 256 {
			return Server{}, fmt.Errorf("LUX_MAX_CONCURRENT_RUNS must be between 1 and 256")
		}
		s.MaxConcurrent = n
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if name, ok := strings.CutPrefix(k, "LUX_GIT_CREDENTIAL_"); ok && v != "" {
			s.GitCredentials[strings.ToLower(name)] = v
		}
	}
	if !strings.HasPrefix(s.GitHubAPIURL, "https://") {
		return s, fmt.Errorf("LUX_GITHUB_API_URL must be an https URL; it receives git tokens")
	}
	if s.DatabaseURL == "" {
		return s, fmt.Errorf("LUX_DATABASE_URL is not set (see docs/development.md)")
	}
	return s, nil
}

// DefaultDataDir is where luxd keeps run directories and repository mirrors.
func DefaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "lux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "lux")
}

// ResolveRunner finds the gitagent runner: LUX_RUNNER_CMD if set, otherwise
// runtime/gitagent-runner next to the luxd binary (installed layout) or in
// the current directory (development), provided Node.js and the runner's
// dependencies are installed.
func ResolveRunner() ([]string, string) {
	if cmd := os.Getenv("LUX_RUNNER_CMD"); cmd != "" {
		args, err := SplitCommand(cmd)
		if err != nil || len(args) == 0 {
			return nil, fmt.Sprintf("LUX_RUNNER_CMD is not a valid command line: %v", err)
		}
		return args, "from LUX_RUNNER_CMD"
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, "Node.js (node) is not on PATH; it is needed to run gitagent agents"
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "..", "lib", "lux", "gitagent-runner"),
			filepath.Join(dir, "runtime", "gitagent-runner"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "runtime", "gitagent-runner"))
	}
	for _, c := range candidates {
		script := filepath.Join(c, "src", "runner.mjs")
		if _, err := os.Stat(script); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "node_modules", "@open-gitagent", "gitagent")); err != nil {
			return nil, "found " + c + " but its dependencies are not installed (run npm ci there)"
		}
		return []string{node, script}, "found " + c
	}
	return nil, "the gitagent runner was not found (install it or set LUX_RUNNER_CMD)"
}

// SplitCommand splits a command line into arguments. Single or double
// quotes keep spaces inside an argument (paths such as "/home/me/My
// Projects/runner.mjs"); a backslash escapes the next character outside
// single quotes. No other shell features are interpreted.
func SplitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg, quote := false, rune(0)
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inArg = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inArg = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if escaped {
		return nil, fmt.Errorf("trailing backslash")
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}
