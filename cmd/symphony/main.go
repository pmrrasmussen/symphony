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

	"github.com/pmrrasmussen/symphony/internal/codex"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/observability"
	"github.com/pmrrasmussen/symphony/internal/preflight"
	"github.com/pmrrasmussen/symphony/internal/status"
	"github.com/pmrrasmussen/symphony/internal/workspace"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
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
	// Optional host capabilities stay disabled until WORKFLOW.md supplies their
	// fixed scope; resolved credentials are filtered from the Codex child.
	backend, githubLifecycle := codex.NewWithIntegrations(settings, slog.New(log.Handler()))
	go githubLifecycle.Run(ctx)
	var t domain.Tracker = tracker
	var a domain.AgentBackend = backend
	var w domain.WorkspaceExecutor = ws
	c := coordinator.New(t, a, w, settings, slog.New(log.Handler()))
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
			if err := ws.Cleanup(ctx, issue); err != nil {
				log.Warn("terminal workspace cleanup failed", "issue", issue.Identifier, "error", err)
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
