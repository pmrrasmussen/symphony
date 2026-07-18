package main

import (
	"context"
	"flag"
	"fmt"
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
	"github.com/pmrrasmussen/symphony/internal/workspace"
)

func main() {
	var workflowPath, logs string
	var dry bool
	flag.StringVar(&workflowPath, "workflow", "WORKFLOW.md", "path to WORKFLOW.md")
	flag.StringVar(&logs, "logs-root", ".symphony/logs", "structured log root")
	flag.BoolVar(&dry, "dry-run", false, "validate configuration and Linear connectivity without launching Codex")
	flag.Parse()
	store, err := config.NewStore(workflowPath, logs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "symphony startup configuration error:", err)
		os.Exit(2)
	}
	s := store.Current().Config
	if err := os.MkdirAll(s.LogRoot, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "log root:", err)
		os.Exit(2)
	}
	if err := os.Chmod(s.LogRoot, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "secure log root:", err)
		os.Exit(2)
	}
	f, err := os.OpenFile(filepath.Join(s.LogRoot, "symphony.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open log:", err)
		os.Exit(2)
	}
	defer f.Close()
	log := observability.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}), os.Stderr)
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
		fmt.Fprintln(os.Stderr, "symphony startup configuration error:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if dry {
		_, err := tracker.ListCandidates(ctx, settings().Tracker.ActiveStates)
		if err != nil {
			log.Error("dry-run poll failed", "error", err)
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("configuration and Linear poll succeeded; Codex was not started")
		return
	}
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
}
