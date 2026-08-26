package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/agent"
	"github.com/pmrrasmussen/symphony/internal/claude"
	"github.com/pmrrasmussen/symphony/internal/codex"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/mcpbridge"
	"github.com/pmrrasmussen/symphony/internal/observability"
	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/preflight"
	"github.com/pmrrasmussen/symphony/internal/service"
	"github.com/pmrrasmussen/symphony/internal/status"
	"github.com/pmrrasmussen/symphony/internal/tui"
	"github.com/pmrrasmussen/symphony/internal/workspace"
)

// endpointCloseTimeout bounds the capability endpoint's own shutdown. It is
// separate from the scheduler's shutdown budget on purpose: see the deferred
// close in run().
const endpointCloseTimeout = 20 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "service" {
		return runService(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "tui" {
		return runTUI(args[1:], os.Stdin, stdout, stderr, operator.Discover)
	}
	processStartedAt := time.Now()
	var workflowPath, logs, logLevelFlag, statusFile string
	var dry bool
	flags := flag.NewFlagSet("symphony", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&workflowPath, "workflow", "WORKFLOW.md", "path to WORKFLOW.md")
	flags.StringVar(&logs, "logs-root", ".symphony/logs", "structured log root")
	flags.StringVar(&statusFile, "status-file", "", "local runtime status JSON path (optional; see docs/runtime-status.md)")
	flags.StringVar(&logLevelFlag, "log-level", "info", "structured log level: info (concise, default) or debug (adds poll admission detail, tool/item lifecycle, and heartbeat/stall records; see docs/observability.md)")
	flags.BoolVar(&dry, "dry-run", false, "validate the full scheduler lifecycle without live side effects")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	logLevel, err := parseLogLevel(logLevelFlag)
	if err != nil {
		fmt.Fprintln(stderr, "symphony:", err)
		return 2
	}
	workflowFlagSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "workflow" {
			workflowFlagSet = true
		}
	})
	if flags.NArg() > 1 {
		fmt.Fprintln(stderr, "symphony: expected at most one positional WORKFLOW.md argument")
		return 2
	}
	if flags.NArg() == 1 {
		if workflowFlagSet {
			fmt.Fprintln(stderr, "symphony: workflow path may be provided positionally or with --workflow, not both")
			return 2
		}
		workflowPath = flags.Arg(0)
	}
	if dry {
		result := preflight.Run(context.Background(), workflowPath, logs)
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			fmt.Fprintln(stderr, "write preflight result:", err)
			return 1
		}
		if !result.OK() {
			return 1
		}
		return 0
	}
	store, err := config.NewStore(workflowPath, logs)
	if err != nil {
		fmt.Fprintln(stderr, "symphony startup configuration error:", err)
		return 2
	}
	s := store.Current().Config
	if err := os.MkdirAll(s.LogRoot, 0o700); err != nil {
		fmt.Fprintln(stderr, "log root:", err)
		return 2
	}
	if err := os.Chmod(s.LogRoot, 0o700); err != nil {
		fmt.Fprintln(stderr, "secure log root:", err)
		return 2
	}
	f, err := os.OpenFile(filepath.Join(s.LogRoot, "symphony.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "open log:", err)
		return 2
	}
	defer f.Close()
	log := observability.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: logLevel}), stderr)
	for _, warning := range s.Warnings {
		log.Warn("workflow configuration warning", "warning", warning)
	}
	settings := func() config.Settings {
		changed, err := store.ReloadIfChanged()
		if err != nil {
			log.Error("workflow reload rejected; retaining last valid configuration", "error", err)
		} else if changed {
			log.Info("workflow configuration reloaded")
			for _, warning := range store.Current().Config.Warnings {
				log.Warn("workflow configuration warning", "warning", warning)
			}
		}
		return store.Current().Config
	}
	tracker := linear.New(settings)
	tracker.SetLogger(slog.New(log.Handler()))
	if err := tracker.Validate(); err != nil {
		fmt.Fprintln(stderr, "symphony startup configuration error:", err)
		return 2
	}
	logStartupCredentialStatus(log, s)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ws := workspace.New(settings)
	backends, githubLifecycle, capabilityEndpoint, err := wire(settings, slog.New(log.Handler()))
	if err != nil {
		fmt.Fprintln(stderr, "symphony startup error:", err)
		return 2
	}
	// Deferred rather than closed inline after Shutdown, so it happens on every
	// path out of run() -- including the startup validation below, which would
	// otherwise leave the listener behind. It still runs after the scheduler has
	// shut down, so no session can be serving a capability call, and it gets a
	// budget of its own: sharing the shutdown deadline would let a slow Shutdown
	// turn this into a context-deadline error that reads as a report of leaked
	// registrations that do not exist. Its error is reported rather than dropped
	// because it counts registrations no session revoked, and each of those is a
	// deferred tracker transition that silently never fired.
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), endpointCloseTimeout)
		defer cancelClose()
		if err := capabilityEndpoint.Close(closeCtx); err != nil {
			log.Error("capability endpoint shutdown reported unfinished sessions", "error", err)
		}
	}()
	// Terminal cleanup may only discard a worktree's local commits once Symphony
	// itself verified them merged. The verifier is read-only and host-owned; when
	// GitHub is not configured it always answers no and cleanup stays as strict
	// as before.
	ws.SetLandingVerifier(githubLifecycle)
	go githubLifecycle.Run(ctx)
	var t domain.Tracker = tracker
	// Coordination talks to the router, not to a runtime: the router resolves
	// agent.backend for each new session and pins continuation and cancellation
	// to whichever backend started it.
	if err := agent.Validate(backends); err != nil {
		log.Error("agent backend registry is incomplete", "error", err)
		return 2
	}
	var a domain.AgentBackend = agent.NewRouter(settings, backends)
	var w domain.WorkspaceExecutor = ws
	c := coordinator.New(t, a, w, settings, slog.New(log.Handler()))
	// The scheduler is the only component that knows an issue is finished, and
	// the poll loop above is the one that would otherwise keep requesting that
	// issue's pull request, and holding its credential snapshot and Linear
	// session, for the rest of this process's life (PMR-112).
	c.SetIssueForgetter(githubLifecycle)
	var statusPublisher *status.Publisher
	var stopStatus context.CancelFunc
	var statusDone chan struct{}
	if statusFile != "" {
		path, err := filepath.Abs(statusFile)
		if err != nil {
			log.Warn("runtime status snapshots disabled", "error", err)
		} else if statusPublisher, err = status.New(path, status.Metadata{PID: os.Getpid(), StartedAt: processStartedAt, WorkflowPath: s.WorkflowPath, LogRoot: s.LogRoot}); err != nil {
			log.Warn("runtime status snapshots disabled", "error", err)
		} else {
			statusCtx, cancelStatus := context.WithCancel(context.Background())
			stopStatus = cancelStatus
			statusDone = make(chan struct{})
			go func() {
				defer close(statusDone)
				statusPublisher.Run(statusCtx, status.DefaultInterval, c.Snapshot, func(err error) {
					log.Warn("runtime status snapshot write failed", "error", err)
				})
			}()
		}
	}
	if terminals, err := tracker.ListTerminal(ctx, settings().Tracker.TerminalStates); err != nil {
		log.Warn("startup terminal cleanup query failed", "error", err)
	} else {
		for _, issue := range terminals {
			outcome, err := ws.Cleanup(ctx, issue)
			if err != nil {
				log.Warn("terminal workspace cleanup failed", "issue", issue.Identifier, "error", err)
				continue
			}
			if outcome == domain.CleanupLanded {
				log.Info("terminal workspace cleanup removed verified landed work", "issue", issue.Identifier)
			}
		}
	}
	c.Start(ctx)
	<-ctx.Done()
	if stopStatus != nil {
		stopStatus()
		<-statusDone
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown timed out", "error", err)
	}
	if statusPublisher != nil {
		if err := statusPublisher.Write(status.Stopped, c.Snapshot()); err != nil {
			log.Warn("final runtime status snapshot write failed", "error", err)
		}
	}
	return 0
}

func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: symphony service <install|migrate|status|restart|uninstall> [flags]")
		return 2
	}
	command := args[0]
	flags := flag.NewFlagSet("symphony service "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var options service.Options
	flags.StringVar(&options.Workflow, "workflow", "", "path to WORKFLOW.md (defaults to repository WORKFLOW.md)")
	flags.StringVar(&options.Name, "name", "", "stable local instance name")
	flags.StringVar(&options.Binary, "binary", "", "shared Symphony executable (defaults to ~/.local/bin/symphony)")
	flags.StringVar(&options.LinearKeyFile, "linear-api-key-file", "", "Linear credential file override")
	flags.StringVar(&options.GitHubTokenFile, "github-token-file", "", "repository-scoped GitHub credential file override")
	flags.StringVar(&options.LogLevel, "log-level", "info", "service log level: info or debug")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "symphony service: unexpected positional arguments")
		}
		return 2
	}
	ctx := context.Background()
	switch command {
	case "install":
		instance, changed, err := service.Install(ctx, options)
		if err != nil {
			fmt.Fprintln(stderr, "symphony service install:", err)
			return 1
		}
		if changed {
			fmt.Fprintln(stdout, "installed", instance.Label)
		} else {
			fmt.Fprintln(stdout, "already installed", instance.Label)
		}
		return 0
	case "migrate":
		migration, err := service.Migrate(ctx, options)
		if err != nil {
			fmt.Fprintln(stderr, "symphony service migrate:", err)
			return 1
		}
		if !migration.Changed {
			fmt.Fprintln(stdout, "already managed", migration.Label)
			return 0
		}
		fmt.Fprintln(stdout, "migrated", migration.Legacy, "to", migration.Label)
		fmt.Fprintln(stdout, "replaced", migration.LegacyPlist)
		fmt.Fprintln(stdout, "backup", migration.Backup)
		return 0
	case "status":
		instance, err := service.Status(ctx, options)
		if err != nil {
			fmt.Fprintln(stderr, "symphony service status:", err)
			return 1
		}
		if err := json.NewEncoder(stdout).Encode(instance); err != nil {
			fmt.Fprintln(stderr, "write service status:", err)
			return 1
		}
		return 0
	case "restart":
		instance, err := service.Restart(ctx, options)
		if err != nil {
			fmt.Fprintln(stderr, "symphony service restart:", err)
			return 1
		}
		fmt.Fprintln(stdout, "restarted", instance.Label)
		return 0
	case "uninstall":
		instance, err := service.Uninstall(ctx, options)
		if err != nil {
			fmt.Fprintln(stderr, "symphony service uninstall:", err)
			return 1
		}
		fmt.Fprintln(stdout, "uninstalled", instance.Label)
		return 0
	default:
		fmt.Fprintln(stderr, "usage: symphony service <install|migrate|status|restart|uninstall> [flags]")
		return 2
	}
}

func runTUI(args []string, input io.Reader, stdout, stderr io.Writer, discover tui.Discover) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "symphony tui: this read-only view accepts no flags")
		return 2
	}
	if err := tui.Run(context.Background(), input, stdout, discover); err != nil {
		fmt.Fprintln(stderr, "symphony tui:", err)
		return 1
	}
	return 0
}

// wire builds the agent backend registry together with the host providers those
// backends share, and hands back the one GitHub manager this process may hold.
// It exists as a seam rather than inline wiring so the sharing is asserted by a
// test: the manager comes back out of the backend that was given it, so the poll
// loop and the landing verifier cannot end up on a second manager that merely
// shares a configuration callback. That second manager is the whole hazard --
// it would own its own linked-pull-request table and its own exactly-once
// completion guard, so a merged pull request would leave its Linear issue
// unreconciled while the guard on the polled manager never fired.
//
// The same reasoning extends to the loopback MCP capability endpoint: it is one
// listener for the daemon's lifetime, shared by every concurrent session and
// separated by per-registration bearer tokens, so it is built here and handed
// back for the caller to close rather than bound inside a backend that has
// nothing to close it with.
//
// Optional host capabilities stay disabled until WORKFLOW.md supplies their
// fixed scope; resolved credentials are filtered from the agent child. Either
// backend may now bind them: the rendered guidance names a capability by the name
// its transport serves it under, and internal/claude refuses a launch whose
// prompt promises a capability its own registry does not advertise.
func wire(settings func() config.Settings, logger *slog.Logger) (map[string]domain.AgentBackend, *githubhost.Manager, *mcpbridge.Server, error) {
	handoff := linear.NewHandoff(settings)
	handoff.SetLogger(logger)
	endpoint, err := mcpbridge.Listen()
	if err != nil {
		return nil, nil, nil, err
	}
	sessions := codex.NewWithProviders(settings, handoff, githubhost.New(settings, logger))
	backends := map[string]domain.AgentBackend{
		config.DefaultAgentBackend: sessions,
		// The Claude backend is given the very providers Codex holds, never
		// copies: sessions.GitHubManager() is read back out rather than passing
		// github along, so a future backend that quietly minted its own would
		// fail the one-manager assertion instead of splitting the linked pull
		// request table in production.
		config.ClaudeAgentBackend: claude.NewWithProviders(settings, handoff, sessions.GitHubManager(), endpoint),
	}
	return backends, sessions.GitHubManager(), endpoint, nil
}

// logStartupCredentialStatus records whether startup resolved the credentials
// needed for Symphony's configured host integrations. It deliberately reports
// only booleans: successful configuration resolution is not a remote
// authentication check, and credentials must never appear in logs.
func logStartupCredentialStatus(log *observability.Logger, settings config.Settings) {
	log.Info("startup credential configuration",
		"linear_credentials_configured", true,
		"github_credentials_configured", settings.GitHub.Enabled,
	)
}

// parseLogLevel accepts the two documented, operator-facing levels. The
// default "info" keeps the log concise; "debug" is the opt-in level that adds
// poll admission detail, tool/item lifecycle records, and heartbeat/stall
// records, all still bound by the same redaction guarantees. See
// docs/observability.md.
func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	default:
		return 0, fmt.Errorf("invalid --log-level %q: supported values are info, debug", value)
	}
}
