// luxd is the Lux control plane: API, (later) orchestrator and model gateway.
//
//	luxd [serve]     run the server (default)
//	luxd migrate     apply database migrations and exit
//	luxd bootstrap   create the first owner and print an API token (server mode)
//	luxd version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/api"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/db"
	"github.com/open-gitagent/lux/internal/executor"
	"github.com/open-gitagent/lux/internal/gateway"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/orchestrator"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/version"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "migrate":
		err = migrate()
	case "bootstrap":
		err = bootstrap(args)
	case "version":
		fmt.Printf("luxd %s %s\n", version.Version, version.Commit)
	case "help", "-h", "--help":
		fmt.Println("usage: luxd [serve|migrate|bootstrap|version]")
	default:
		err = fmt.Errorf("unknown command %q (try: luxd help)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "luxd:", err)
		os.Exit(1)
	}
}

func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if os.Getenv("LUX_LOG_LEVEL") == "debug" {
		opts.Level = slog.LevelDebug
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func openDB(ctx context.Context, cfg config.Server) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return db.Open(ctx, cfg.DatabaseURL)
}

func migrate() error {
	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}
	pool, err := openDB(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := db.Migrate(context.Background(), pool)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("database is up to date")
	}
	for _, m := range applied {
		fmt.Println("applied", m)
	}
	return nil
}

// bootstrap creates the first owner and prints its token once. For server
// mode, where tokens must not be written to logs or shared files.
func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	username := fs.String("username", "admin", "username of the first owner")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	tok, err := store.New(pool).Bootstrap(ctx, *username)
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("this database already has users; create further tokens with the API")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Created owner %q in workspace \"default\". This token is shown once; store it securely:\n", *username)
	fmt.Println(tok)
	return nil
}

func serve(args []string) error {
	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogFormat)
	if err := executor.ProtectServerProcess(); err != nil {
		return fmt.Errorf("protecting the server process from agents: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data directory %s: %w", cfg.DataDir, err)
	}
	pool, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if os.Getenv("LUX_AUTO_MIGRATE") != "false" {
		applied, err := db.Migrate(ctx, pool)
		if err != nil {
			return err
		}
		for _, m := range applied {
			log.Info("applied migration", "name", m)
		}
	} else if v, err := db.SchemaVersion(ctx, pool); err != nil || v != db.LatestVersion() {
		return fmt.Errorf("database schema is at version %d, this luxd needs %d; run `luxd migrate`", v, db.LatestVersion())
	}

	st := store.New(pool)
	if cfg.Mode == "local" {
		if err := localBootstrap(ctx, st, cfg, log); err != nil {
			return err
		}
	} else if ok, _ := st.IsBootstrapped(ctx); !ok {
		log.Warn("no users exist yet; run `luxd bootstrap` to create the first owner and token")
	}

	// A publish takes at most a few minutes; older ones were cut off by a stop.
	if err := st.ResetStalePublishes(ctx, 15*time.Minute); err != nil {
		return err
	}

	hub := notify.NewHub(pool, log)
	go hub.Run(ctx)

	var validator *agentrepo.OpenGAPValidator
	if cfg.OpenGAPCmd != "" {
		args, err := config.SplitCommand(cfg.OpenGAPCmd)
		if err != nil || len(args) == 0 {
			return fmt.Errorf("LUX_OPENGAP_CMD is not a valid command line: %v", err)
		}
		validator = &agentrepo.OpenGAPValidator{Command: args, Timeout: 60 * time.Second}
	}
	importer := &agentrepo.Importer{DataDir: cfg.DataDir, Options: agentrepo.Options{OpenGAP: validator}}
	providers := map[string]executor.Provider{}
	if os.Getenv("LUX_SANDBOX") == "off" {
		cfg.SandboxNote = "turned off with LUX_SANDBOX=off"
	} else {
		cfg.Bwrap, cfg.SandboxNote = executor.FindBubblewrap()
	}
	if cfg.Bwrap != "" {
		providers["sandbox"] = executor.Sandbox{Bwrap: cfg.Bwrap, ReadOnly: executor.RunnerMounts(cfg.RunnerCmd)}
		log.Info("sandbox executor available", "bwrap", cfg.Bwrap)
	} else {
		log.Warn("sandbox executor unavailable", "reason", cfg.SandboxNote)
	}
	if cfg.AllowLocalExecutor {
		providers["local"] = executor.Local{}
	}
	if cfg.RunnerCmd == nil {
		log.Warn("no agent runtime available; tasks will fail until one is installed", "reason", cfg.RunnerNote)
	} else {
		log.Info("agent runtime", "command", cfg.RunnerCmd, "how", cfg.RunnerNote)
	}
	orch := &orchestrator.Orchestrator{
		Store: st, Pool: pool, Hub: hub, Log: log, Providers: providers,
		Cfg: orchestrator.Config{
			DataDir: cfg.DataDir, APIURL: cfg.PublicURL, RunnerCmd: cfg.RunnerCmd,
			MaxConcurrent: cfg.MaxConcurrent, MirrorPath: importer.MirrorPath,
			Sync: importer.Sync, GitCredentials: cfg.GitCredentials,
		},
	}
	orchDone := make(chan struct{})
	go func() { orch.Run(ctx); close(orchDone) }()

	gw := gateway.New(st, gateway.ProvidersFromConfig(cfg.Providers), log)
	box, err := connectors.LoadKey(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("connector key: %w", err)
	}
	var google *connectors.Google
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		google = &connectors.Google{ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret, Endpoints: connectors.GoogleEndpoints}
		log.Info("google connector available", "redirect", cfg.PublicURL+"/v1/connectors/google/callback")
	}
	srv := &api.Server{
		Google:              google,
		Box:                 box,
		Gateway:             gw,
		Store:               st,
		Importer:            importer,
		Hub:                 hub,
		Cfg:                 cfg,
		Log:                 log,
		OrchestratorRunning: orch.Running,
	}
	srv.ReloadProviders(ctx) // add keys saved in the app
	for _, p := range gateway.Describe(gw.Table()) {
		log.Info("model provider", "name", p.Name, "base_url", p.BaseURL, "has_key", p.HasKey)
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: event streams stay open. Handlers bound their own work.
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("luxd listening", "addr", cfg.Listen, "mode", cfg.Mode, "version", version.Version, "data_dir", cfg.DataDir)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Event streams never finish on their own; close them after the grace period.
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out; closing connections", "err", err)
		_ = httpSrv.Close()
	}
	// Runs keep going; the next luxd re-attaches to them.
	<-orchDone
	return nil
}

// localBootstrap creates the first owner on a fresh local install and saves
// its token for the "local" environment, so the CLI and desktop app work
// without copying tokens by hand. It also repairs a missing local token.
func localBootstrap(ctx context.Context, st *store.Store, cfg config.Server, log *slog.Logger) error {
	ok, err := st.IsBootstrapped(ctx)
	if err != nil {
		return err
	}
	client, err := config.LoadClient()
	if err != nil {
		return err
	}
	envName := "local"
	env := client.Environments[envName]
	env.URL, env.Local = cfg.PublicURL, true
	client.Environments[envName] = env
	if err := client.Save(); err != nil {
		return fmt.Errorf("save client config: %w", err)
	}
	if ok {
		if tok, _ := config.Token(envName); tok == "" {
			log.Warn("the local credentials file has no token; create one with `lux login` or `luxd bootstrap` on a fresh database")
		}
		return nil
	}
	user := os.Getenv("USER")
	if user == "" || !validUsername(user) {
		user = "owner"
	}
	tok, err := st.Bootstrap(ctx, user)
	if err != nil {
		return err
	}
	// A local install has one person, who could otherwise never approve
	// their own agents' actions. This is written into the workspace policy,
	// visible and changeable; server installs keep maker-checker on.
	if wss, err := st.ListWorkspaces(ctx, mustUser(ctx, st, tok)); err == nil && len(wss) == 1 {
		if err := st.UpdateWorkspacePolicy(ctx, wss[0].ID, []byte(`{"require_distinct_approver": false}`)); err != nil {
			return err
		}
	}
	if err := config.SetToken(envName, tok); err != nil {
		return fmt.Errorf("save local token: %w", err)
	}
	log.Info("created local owner and saved its token", "user", user, "credentials", config.Dir()+"/credentials.yaml")
	return nil
}

func validUsername(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func mustUser(ctx context.Context, st *store.Store, tok string) string {
	p, err := st.Authenticate(ctx, tok)
	if err != nil {
		return ""
	}
	return p.UserID
}
