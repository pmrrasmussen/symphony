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
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/codex"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/observability"
	"github.com/pmrrasmussen/symphony/internal/preflight"
	"github.com/pmrrasmussen/symphony/internal/workspace"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var workflowPath, logs string
	var dry bool
	flags := flag.NewFlagSet("symphony", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&workflowPath, "workflow", "WORKFLOW.md", "path to WORKFLOW.md")
	flags.StringVar(&logs, "logs-root", ".symphony/logs", "structured log root")
	flags.BoolVar(&dry, "dry-run", false, "validate the full scheduler lifecycle without live side effects")
	if err := flags.Parse(args); err != nil {
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
	log := observability.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}), stderr)
	settings := func() config.Settings {
		changed, err := store.ReloadIfChanged()
		if err != nil {
			log.Error("workflow reload rejected; retaining last valid configuration", "error", err)
		} else if changed {
			log.Info("workflow configuration reloaded")
		}
		return store.Current().Config
	}
	tracker := linear.New(settings)
	if err := tracker.Validate(); err != nil {
		fmt.Fprintln(stderr, "symphony startup configuration error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ws := workspace.New(settings)
	// The backend receives only the already-loaded settings callback. Its
	// optional Linear handoff capability stays disabled until WORKFLOW.md
	// explicitly declares tracker.provider.handoff_state.
	backend := codex.NewWithLinearHandoff(settings, "LINEAR_API_KEY")
	var t domain.Tracker = tracker
	var a domain.AgentBackend = backend
	var w domain.WorkspaceExecutor = ws
	c := coordinator.New(t, a, w, settings, slog.New(log.Handler()))
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown timed out", "error", err)
	}
	return 0
}
