package tui

import (
	"fmt"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

func (m Model) overview(now time.Time, style theme) string {
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
	if !style.layout {
		var b strings.Builder
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
				active, capacity, retries := instanceActivity(instance)
				fmt.Fprintf(&b, "%s %-36s %-10s %2d/%-3d    %-7d  %s\n", mark, truncate(instance.ID, 36), livenessLabel(instance.Liveness), active, capacity, retries, findingsIndicator(instance.Findings))
			}
			b.WriteString("\n")
		}
		b.WriteString("up/down or j/k: select  Enter: inspect  r: refresh  q: quit\n")
		return b.String()
	}
	header := lipgloss.JoinVertical(lipgloss.Left,
		style.primary.Render("Symphony operator view"),
		style.muted.Render(fmt.Sprintf("%d instances · %d running · %d stopped · %d stale · %d invalid",
			len(m.instances), running, stopped, stale, invalid)),
	)
	main := style.muted.Render("No convention-matching LaunchAgents were found.")
	if len(m.instances) > 0 {
		main = m.instanceTable(style, style.mainBudget(header))
	}
	return style.frame(header, main, m.statusLine(now, style),
		style.muted.Render("j/k select · ⏎ inspect · r refresh · q quit"))
}

// instanceRows builds the overview's columns and rows. Which columns appear
// depends on the width band: the two numeric columns go first, because they are
// one keypress away on the Status page and below eighty columns they cost the
// identifier the room it needs to stay legible.
//
// rowBudget is how many instance rows may be drawn; each renderer subtracts its
// own chrome from the frame's budget before calling.
func (m Model) instanceRows(style theme, rowBudget int) (headers []string, rows [][]string, hidden int) {
	numeric := bandFor(style.width) != bandNarrow
	headers = []string{"INSTANCE", "STATE"}
	if numeric {
		headers = append(headers, "AGENTS", "RETRIES")
	}
	headers = append(headers, "CHECKS")

	shown, hidden := window(m.instances, m.selected, rowBudget)
	selected := m.selected - (len(m.instances) - hidden - len(shown))

	nameWidth := 0
	if style.width > 0 {
		// PMR-88 measured a twenty-four column pin here as most of the old
		// seventy-six column floor, so the narrow band lowers it.
		floor := 24
		if !numeric {
			floor = 16
		}
		nameWidth = max(floor, style.width/3)
	}

	rows = make([][]string, 0, len(shown))
	for index, instance := range shown {
		marker, name := "  ", instance.ID
		if nameWidth > 0 {
			name = truncate(name, nameWidth)
		}
		if index == selected {
			// A marker and weight rather than a row background: the state and
			// checks cells carry their own foreground colors, and a row
			// background does not reach inside them, which renders patchy.
			marker = "▸ "
			name = style.primary.Render(name)
		}
		active, capacity, retries := instanceActivity(instance)
		row := []string{marker + name, style.liveness(instance.Liveness)}
		if numeric {
			row = append(row, fmt.Sprintf("%d/%d", active, capacity), fmt.Sprintf("%d", retries))
		}
		rows = append(rows, append(row, style.checks(instance.Findings)))
	}
	return headers, rows, hidden
}

// numericColumns are the row indexes to right-align, so magnitudes line up down
// the column. They exist only in the wider bands.
func numericColumns(style theme) []int {
	if bandFor(style.width) == bandNarrow {
		return nil
	}
	return []int{2, 3}
}

// instanceTable draws the overview as a real table, so the column widths are
// computed from the content rather than hand-counted. The old header was a
// literal string and had drifted one column out of step with its rows.
func (m Model) instanceTable(style theme, budget int) string {
	// The table spends three rows on its own border and header.
	headers, rows, hidden := m.instanceRows(style, budget-3)
	right := numericColumns(style)
	rightward := make(map[int]bool, len(right))
	for _, column := range right {
		rightward[column] = true
	}
	rendered := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(style.rule).
		BorderColumn(false).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, column int) lipgloss.Style {
			cell := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				cell = style.emphasis.Padding(0, 1)
			}
			if rightward[column] {
				cell = cell.Align(lipgloss.Right)
			}
			return cell
		})
	drawn := rendered.String()
	if style.width > 0 && lipgloss.Width(drawn) > style.width {
		// Only constrain the table when its natural width would overflow.
		// Forcing the full width otherwise spreads five columns of short values
		// across the window and strands each number far from its header.
		drawn = rendered.Width(style.width).Wrap(false).String()
	}
	if note := style.more(hidden); note != "" {
		return drawn + "\n" + note
	}
	return drawn
}

// instanceList draws the same rows without a box, for the split layout. The rule
// between the panes already separates them, and a bordered table inside a
// divided column is the second border the clutter audit counts.
func (m Model) instanceList(style theme, budget int) string {
	// Without a border the list spends one row, on its header.
	headers, rows, hidden := m.instanceRows(style, budget-1)
	drawn := style.borderless(headers, rows, numericColumns(style)...)
	if note := style.more(hidden); note != "" {
		return drawn + "\n" + note
	}
	return drawn
}

// instanceActivity reads the agent and retry counts an instance publishes,
// tolerating the snapshot and configuration both being absent.
func instanceActivity(instance operator.Instance) (active, capacity, retries int) {
	if instance.Config != nil {
		capacity = instance.Config.MaxConcurrentAgents
	}
	if instance.Snapshot != nil {
		active = len(instance.Snapshot.Coordinator.Running)
		retries = len(instance.Snapshot.Coordinator.Retrying)
	}
	return active, capacity, retries
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
	fmt.Fprintf(b, "\nClaims: %d; active: %d; retries: %d; waiting: %d\n", snapshot.Coordinator.Claimed, len(snapshot.Coordinator.Running), len(snapshot.Coordinator.Retrying), len(snapshot.Coordinator.Waiting))
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
	for _, wait := range snapshot.Coordinator.Waiting {
		if wait.Reason == "blocked_by_relation" {
			fmt.Fprintf(b, "\nWaiting %s (%s): blocked by %s; waiting %s\n", wait.IssueIdentifier, wait.IssueState, strings.Join(wait.BlockedBy, ","), formatDuration(now.Sub(wait.Since)))
			continue
		}
		fmt.Fprintf(b, "\nWaiting %s (%s): eligible, no capacity; waiting %s\n", wait.IssueIdentifier, wait.IssueState, formatDuration(now.Sub(wait.Since)))
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
	fmt.Fprintf(b, "\n%s:\n", backendLabel(config.AgentBackend))
	for _, row := range backendConfigRows(config) {
		fmt.Fprintf(b, "%s: %s\n", row[0], row[1])
	}
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
