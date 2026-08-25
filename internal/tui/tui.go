// Package tui provides Symphony's deliberately small, read-only operator UI.
// It consumes the operator discovery model and never opens a connection to a
// running daemon or any remote service.
package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

// Discover is the read-only boundary used by the UI. Keeping it injectable
// makes screen updates testable without a terminal or a local LaunchAgent.
type Discover func(context.Context, operator.Options) ([]operator.Instance, error)

type page uint8

const (
	overviewPage page = iota
	statusPage
	configPage
	validationPage
)

// Model is the complete UI state. It contains only display-safe operator data.
type Model struct {
	instances []operator.Instance
	selected  int
	page      page
	message   string
	updatedAt time.Time
}

// New builds an overview model from an already-discovered, sorted instance
// list. A copy prevents callers from mutating the UI's current frame.
func New(instances []operator.Instance, now time.Time) Model {
	copy := append([]operator.Instance(nil), instances...)
	sort.Slice(copy, func(i, j int) bool { return copy[i].ID < copy[j].ID })
	return Model{instances: copy, updatedAt: now}
}

// Update applies one small keyboard action. It returns true when the caller
// should leave the UI. Refresh is handled by Run so it can reread the host.
func (m Model) Update(key string) (Model, bool) {
	key = normalizeKey(key)
	switch key {
	case "q":
		if m.page == overviewPage {
			return m, true
		}
		m.page = overviewPage
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter":
		if m.page == overviewPage && len(m.instances) > 0 {
			m.page = statusPage
		}
	case "tab":
		if m.page != overviewPage {
			m.page++
			if m.page > validationPage {
				m.page = statusPage
			}
		}
	case "s":
		if m.page != overviewPage {
			m.page = statusPage
		}
	case "c":
		if m.page != overviewPage {
			m.page = configPage
		}
	case "v":
		if m.page != overviewPage {
			m.page = validationPage
		}
	}
	return m, false
}

func (m *Model) move(delta int) {
	if len(m.instances) == 0 {
		return
	}
	m.selected += delta
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.instances) {
		m.selected = len(m.instances) - 1
	}
}

func normalizeKey(key string) string {
	key = strings.TrimRight(key, "\r\n")
	switch key {
	case "", "\r":
		return "enter"
	case "\x1b[A":
		return "up"
	case "\x1b[B":
		return "down"
	case "\t":
		return "tab"
	default:
		return strings.ToLower(strings.TrimSpace(key))
	}
}

// Refresh replaces the current frame. Selection stays on the same position
// where possible, so an instance restart cannot crash or disorient the UI.
func (m *Model) Refresh(instances []operator.Instance, err error, now time.Time) {
	if err != nil {
		m.message = "refresh failed: " + err.Error()
		return
	}
	m.instances = append(m.instances[:0], instances...)
	sort.Slice(m.instances, func(i, j int) bool { return m.instances[i].ID < m.instances[j].ID })
	if m.selected >= len(m.instances) {
		m.selected = len(m.instances) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.page != overviewPage && len(m.instances) == 0 {
		m.page = overviewPage
	}
	m.message = "refreshed"
	m.updatedAt = now
}

// View renders only fixed fields from operator.Instance. In particular, it
// never prints config credentials, arbitrary environment variables, prompts,
// protocol payloads, or log attributes.
func (m Model) View(now time.Time) string {
	if m.page == overviewPage {
		return m.overview(now)
	}
	if len(m.instances) == 0 {
		return "Symphony operator view\n\nNo configured Symphony instances.\n\nq: quit\n"
	}
	instance := m.instances[m.selected]
	var b strings.Builder
	fmt.Fprintf(&b, "Symphony operator view  %s\n", instance.ID)
	fmt.Fprintf(&b, "state: %s  launchd: %s  refreshed: %s\n\n", instance.Liveness, launchdText(instance.Launchd), formatAge(now, m.updatedAt))
	fmt.Fprintf(&b, "[s] Status  [c] Config  [v] Validation  [Tab] next  [r] refresh  [q] back\n\n")
	switch m.page {
	case statusPage:
		m.writeStatus(&b, instance, now)
	case configPage:
		m.writeConfig(&b, instance)
	case validationPage:
		m.writeValidation(&b, instance)
	}
	return b.String()
}

func (m Model) overview(now time.Time) string {
	var b strings.Builder
	running, stopped, stale, invalid := 0, 0, 0, 0
	for _, instance := range m.instances {
		switch instance.Liveness {
		case operator.LivenessRunning:
			running++
		case operator.LivenessStopped:
			stopped++
		case operator.LivenessStale:
			stale++
		default:
			invalid++
		}
	}
	fmt.Fprintf(&b, "Symphony operator view\n%d instances: %d running, %d stopped, %d stale, %d invalid  refreshed: %s\n\n", len(m.instances), running, stopped, stale, invalid, formatAge(now, m.updatedAt))
	if m.message != "" {
		fmt.Fprintf(&b, "%s\n\n", m.message)
	}
	if len(m.instances) == 0 {
		b.WriteString("No convention-matching LaunchAgents were found.\n\n")
	} else {
		b.WriteString("  INSTANCE                              STATE      AGENTS    RETRIES  CHECKS\n")
		for index, instance := range m.instances {
			mark := " "
			if index == m.selected {
				mark = ">"
			}
			capacity, active, retries := 0, 0, 0
			if instance.Config != nil {
				capacity = instance.Config.MaxConcurrentAgents
			}
			if instance.Snapshot != nil {
				active = len(instance.Snapshot.Coordinator.Running)
				retries = len(instance.Snapshot.Coordinator.Retrying)
			}
			fmt.Fprintf(&b, "%s %-36s %-10s %2d/%-3d    %-7d  %s\n", mark, truncate(instance.ID, 36), instance.Liveness, active, capacity, retries, findingsIndicator(instance.Findings))
		}
		b.WriteString("\n")
	}
	b.WriteString("up/down or j/k: select  Enter: inspect  r: refresh  q: quit\n")
	return b.String()
}

func (m Model) writeStatus(b *strings.Builder, instance operator.Instance, now time.Time) {
	if instance.Snapshot == nil {
		b.WriteString("No readable runtime snapshot. Launchd state remains an independent observation.\n")
		m.writeRecentLog(b, instance)
		return
	}
	snapshot := instance.Snapshot
	fmt.Fprintf(b, "Service: %s; launchd %s", snapshot.State, launchdText(instance.Launchd))
	if !snapshot.StartedAt.IsZero() {
		fmt.Fprintf(b, "; uptime %s", formatDuration(now.Sub(snapshot.StartedAt)))
	}
	if !snapshot.UpdatedAt.IsZero() {
		fmt.Fprintf(b, "; snapshot %s ago", formatDuration(now.Sub(snapshot.UpdatedAt)))
	}
	fmt.Fprintf(b, "\nClaims: %d; active: %d; retries: %d\n", snapshot.Coordinator.Claimed, len(snapshot.Coordinator.Running), len(snapshot.Coordinator.Retrying))
	for _, run := range snapshot.Coordinator.Running {
		fmt.Fprintf(b, "\n%s (%s)  run %s; activity %s ago; turns %d", run.IssueIdentifier, run.IssueState, formatDuration(now.Sub(run.StartedAt)), formatDuration(now.Sub(run.LastActivityAt)), run.TurnCount)
		if instance.Config != nil {
			fmt.Fprintf(b, "/%d", instance.Config.MaxTurns)
		}
		fmt.Fprintf(b, "\n  tokens: input %d, output %d, total %d", run.Usage.InputTokens, run.Usage.OutputTokens, run.Usage.TotalTokens)
		if run.OutstandingOperation != nil {
			fmt.Fprintf(b, "\n  waiting: %s%s (%s)", run.OutstandingOperation.Type, named(run.OutstandingOperation.Name), formatDuration(time.Duration(run.OutstandingOperation.AgeMS)*time.Millisecond))
		}
		if len(run.RateLimit) > 0 {
			fmt.Fprintf(b, "\n  rate limits: %s", rateLimits(run.RateLimit))
		}
		b.WriteString("\n")
	}
	for _, retry := range snapshot.Coordinator.Retrying {
		fmt.Fprintf(b, "\nRetry %s: %s/%s attempt %d, due %s\n", retry.IssueIdentifier, retry.Kind, retry.Reason, retry.Attempt, formatTimeOrAge(now, retry.Due))
	}
	m.writeRecentLog(b, instance)
}

func (m Model) writeRecentLog(b *strings.Builder, instance operator.Instance) {
	if len(instance.RecentLog) == 0 {
		return
	}
	b.WriteString("\nRecent redacted lifecycle activity:\n")
	for _, event := range instance.RecentLog {
		fmt.Fprintf(b, "  %s %-5s %s\n", event.Time.Local().Format("15:04:05"), event.Level, event.Message)
	}
}

func (m Model) writeConfig(b *strings.Builder, instance operator.Instance) {
	config := instance.Config
	if config == nil {
		b.WriteString("Effective configuration is unavailable; see Validation.\n")
		return
	}
	fmt.Fprintf(b, "Workflow: %s\nRepository: %s\nWorkspace root: %s\n", empty(instance.Paths.Workflow), empty(config.WorkspaceSource), empty(config.WorkspaceRoot))
	fmt.Fprintf(b, "\nLinear project: %s\nActive states: %s\nTerminal states: %s\nHandoff / merge: %s / %s\n", empty(config.ProjectSelector), comma(config.ActiveStates), comma(config.TerminalStates), empty(config.HandoffState), empty(config.MergeState))
	fmt.Fprintf(b, "\nPolling: %s\nAgent capacity: %d; max turns: %d\n", config.PollInterval, config.MaxConcurrentAgents, config.MaxTurns)
	if len(config.MaxConcurrentByState) > 0 {
		fmt.Fprintf(b, "State capacity: %s\n", rateLimitsInt(config.MaxConcurrentByState))
	}
	fmt.Fprintf(b, "\nCodex: %s\nTimeouts: turn %s; read %s; start %s; stall %s\nSandbox policy: approval %s; thread %s\n", empty(config.CodexCommand), config.TurnTimeout, config.ReadTimeout, config.StartTimeout, config.StallTimeout, empty(config.CodexApprovalPolicy), empty(config.CodexThreadSandbox))
	fmt.Fprintf(b, "\nGitHub: %s/%s base %s; merge %s\nRequired checks: %s\n", empty(config.GitHubOwner), empty(config.GitHubRepository), empty(config.GitHubBaseBranch), empty(config.GitHubMergeMethod), comma(config.GitHubRequiredChecks))
	fmt.Fprintf(b, "Credentials: Linear %s; GitHub %s\n", configured(config.Credentials.Tracker.Configured), configured(config.Credentials.GitHub.Configured))
}

func (m Model) writeValidation(b *strings.Builder, instance operator.Instance) {
	if instance.Liveness == operator.LivenessInvalid {
		b.WriteString("Overall result: INVALID\n")
	} else {
		b.WriteString("Overall result: valid for read-only inspection\n")
	}
	if len(instance.Findings) == 0 {
		b.WriteString("\nNo validation findings.\n")
		return
	}
	b.WriteString("\nFindings:\n")
	for _, finding := range instance.Findings {
		fmt.Fprintf(b, "  %s %s: %s\n", strings.ToUpper(string(finding.Severity)), finding.Code, finding.Message)
	}
}

// Run owns no persistent connection or background process. Each refresh
// re-invokes discovery, which only rereads local launchd, status, and log data.
func Run(ctx context.Context, input io.Reader, output io.Writer, discover Discover) error {
	instances, err := discover(ctx, operator.Options{})
	model := New(instances, time.Now())
	if err != nil {
		model.message = "initial discovery failed: " + err.Error()
	}
	reader := bufio.NewReader(input)
	for {
		if _, err := fmt.Fprint(output, "\x1b[2J\x1b[H", model.View(time.Now())); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				return nil
			}
			return err
		}
		key := normalizeKey(line)
		if key == "r" {
			instances, refreshErr := discover(ctx, operator.Options{})
			model.Refresh(instances, refreshErr, time.Now())
			continue
		}
		var quit bool
		model, quit = model.Update(key)
		if quit {
			return nil
		}
		if err == io.EOF {
			return nil
		}
	}
}

func launchdText(status operator.LaunchdStatus) string {
	if !status.Loaded {
		return "not loaded"
	}
	if status.Process {
		return fmt.Sprintf("running (pid %d)", status.PID)
	}
	return "loaded, no live process"
}

func findingsIndicator(findings []operator.Finding) string {
	errors, warnings := 0, 0
	for _, finding := range findings {
		if finding.Severity == operator.SeverityError {
			errors++
		} else {
			warnings++
		}
	}
	switch {
	case errors > 0:
		return fmt.Sprintf("%d error(s), %d warning(s)", errors, warnings)
	case warnings > 0:
		return fmt.Sprintf("%d warning(s)", warnings)
	default:
		return "ok"
	}
}

func formatDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	if value < time.Minute {
		return fmt.Sprintf("%ds", int(value.Seconds()))
	}
	if value < time.Hour {
		return fmt.Sprintf("%dm", int(value.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(value.Hours()), int(value.Minutes())%60)
}

func formatAge(now, then time.Time) string {
	if then.IsZero() {
		return "unknown"
	}
	return formatDuration(now.Sub(then)) + " ago"
}

func formatTimeOrAge(now time.Time, value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	if value.Before(now) {
		return formatDuration(now.Sub(value)) + " ago"
	}
	return "in " + formatDuration(value.Sub(now))
}

func named(value string) string {
	if value == "" {
		return ""
	}
	return " " + value
}

func rateLimits(values map[string]int64) string {
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, fmt.Sprintf("%s=%d", key, value))
	}
	sort.Strings(parts)
	return comma(parts)
}

func rateLimitsInt(values map[string]int) string {
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, fmt.Sprintf("%s=%d", key, value))
	}
	sort.Strings(parts)
	return comma(parts)
}

func comma(values []string) string {
	if len(values) == 0 {
		return "not configured"
	}
	return strings.Join(values, ", ")
}

func configured(value bool) string {
	if value {
		return "configured"
	}
	return "not configured"
}

func empty(value string) string {
	if value == "" {
		return "not configured"
	}
	return value
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max-1] + "…"
}
