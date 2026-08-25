// The dashboard's detail pages. The plain renderer in tui.go keeps its prose:
// it is the scriptable, non-TTY surface, and pipes and scripts read its exact
// wording. These lay the same fields out as tables for a terminal.
package tui

import (
	"fmt"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

// definition is one captioned group of label and value rows.
type definition struct {
	caption string
	rows    [][2]string
}

// sized copies a theme for a narrower column, so the split layout can render
// the same tables against a pane width rather than the window width.
func (t theme) sized(width int) theme {
	t.width = width
	return t
}

// pane puts a divider down the right edge of the list column. One rule between
// the two halves is enough; boxing either of them would be the nested border
// the clutter audit warns about.
//
// It deliberately sets no width. Lipgloss counts padding inside Width, so
// pinning the column to the width its content already measured leaves the
// padding a column short and wraps every row and the border with it.
func (t theme) pane() lipgloss.Style {
	style := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, true, false, false).
		PaddingRight(1)
	if t.color {
		style = style.BorderForeground(lipgloss.BrightBlack)
	}
	return style
}

// stats renders label and value pairs as one strip. The same facts as sentences
// cost three rows and read slower, because the eye has to find the numbers
// inside the prose.
func (t theme) stats(pairs [][2]string) string {
	cells := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		cells = append(cells, t.muted.Render(pair[0])+" "+pair[1])
	}
	return strings.Join(cells, "   ")
}

// borderless draws a table with no border of its own, and right-aligns the
// named columns so magnitudes line up. The frame's rules already separate these
// pages from the rest of the screen.
func (t theme) borderless(headers []string, rows [][]string, right ...int) string {
	rightward := make(map[int]bool, len(right))
	for _, column := range right {
		rightward[column] = true
	}
	built := table.New().
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderHeader(false).
		Rows(rows...).
		StyleFunc(func(row, column int) lipgloss.Style {
			cell := lipgloss.NewStyle().PaddingRight(2)
			if row == table.HeaderRow {
				cell = t.emphasis.PaddingRight(2)
			}
			if rightward[column] {
				cell = cell.Align(lipgloss.Right)
			}
			return cell
		})
	if len(headers) > 0 {
		built = built.Headers(headers...)
	}
	drawn := built.String()
	if t.width > 0 && lipgloss.Width(drawn) > t.width {
		// Truncate rather than wrap inside cells; the full value is on the
		// page the field belongs to.
		drawn = built.Width(t.width).Wrap(false).String()
	}
	return strings.TrimRight(drawn, "\n")
}

// definitions renders captioned label and value groups. Grouping is what turns
// twenty configuration lines into five things to read.
func (t theme) definitions(sections []definition) string {
	blocks := make([]string, 0, len(sections)*3)
	for index, section := range sections {
		if index > 0 {
			blocks = append(blocks, "")
		}
		rows := make([][]string, 0, len(section.rows))
		for _, row := range section.rows {
			rows = append(rows, []string{t.muted.Render(row[0]), row[1]})
		}
		blocks = append(blocks, t.emphasis.Render(section.caption), t.borderless(nil, rows))
	}
	return strings.Join(blocks, "\n")
}

// detailBody renders one detail page and fits it to the row budget, reporting
// anything it could not draw rather than dropping it in silence.
func (m Model) detailBody(instance operator.Instance, current page, now time.Time, style theme, budget int) string {
	var body string
	switch current {
	case configPage:
		body = m.configPanel(instance, style)
	case validationPage:
		body = m.validationPanel(instance, style)
	default:
		body = m.statusPanel(instance, now, style, budget)
	}
	lines, hidden := clamp(strings.Split(strings.TrimRight(body, "\n"), "\n"), budget)
	drawn := strings.Join(lines, "\n")
	if note := style.more(hidden); note != "" {
		drawn += "\n" + note
	}
	return drawn
}

// statusPanel is the page that changes while an operator watches it, so the
// fixed parts are drawn first and the log tail gives way to whatever room is
// left.
func (m Model) statusPanel(instance operator.Instance, now time.Time, style theme, budget int) string {
	sections := []string{}
	if instance.Snapshot == nil {
		sections = append(sections, style.warn.Render("No readable runtime snapshot.")+
			style.muted.Render(" Launchd state remains an independent observation."))
	} else {
		snapshot := instance.Snapshot
		pairs := [][2]string{}
		if snapshot.State != "" {
			pairs = append(pairs, [2]string{"service", snapshot.State})
		}
		if !snapshot.StartedAt.IsZero() {
			pairs = append(pairs, [2]string{"uptime", formatDuration(now.Sub(snapshot.StartedAt))})
		}
		if !snapshot.UpdatedAt.IsZero() {
			pairs = append(pairs, [2]string{"snapshot", formatDuration(now.Sub(snapshot.UpdatedAt)) + " ago"})
		}
		pairs = append(pairs,
			[2]string{"claims", fmt.Sprintf("%d", snapshot.Coordinator.Claimed)},
			[2]string{"active", fmt.Sprintf("%d", len(snapshot.Coordinator.Running))},
			[2]string{"retries", fmt.Sprintf("%d", len(snapshot.Coordinator.Retrying))},
		)
		sections = append(sections, style.stats(pairs))
		if agents := m.agentTable(instance, now, style); agents != "" {
			sections = append(sections, "", agents)
		}
		if retries := m.retryTable(instance, now, style); retries != "" {
			sections = append(sections, "", retries)
		}
	}
	fixed := strings.Join(sections, "\n")
	if len(instance.RecentLog) == 0 {
		return fixed
	}
	room := 0
	if budget > 0 {
		// Two rows for the blank line and the caption above the tail.
		room = budget - lipgloss.Height(fixed) - 2
		if room < 1 {
			return fixed
		}
	}
	events, hidden := clamp(instance.RecentLog, room)
	rows := make([][]string, 0, len(events))
	for _, event := range events {
		rows = append(rows, []string{
			style.muted.Render(event.Time.Local().Format("15:04:05")),
			style.muted.Render(event.Level),
			event.Message,
		})
	}
	tail := style.emphasis.Render("Recent redacted lifecycle activity") + "\n" + style.borderless(nil, rows)
	if note := style.more(hidden); note != "" {
		tail += "\n" + note
	}
	return fixed + "\n\n" + tail
}

// agentTable lists the running agents. One row per agent replaces four prose
// lines each, which is what lets a busy coordinator fit on one screen.
func (m Model) agentTable(instance operator.Instance, now time.Time, style theme) string {
	running := instance.Snapshot.Coordinator.Running
	if len(running) == 0 {
		return ""
	}
	turns := ""
	if instance.Config != nil {
		turns = fmt.Sprintf("/%d", instance.Config.MaxTurns)
	}
	rows := make([][]string, 0, len(running))
	for _, run := range running {
		waiting := ""
		if run.OutstandingOperation != nil {
			waiting = run.OutstandingOperation.Type + named(run.OutstandingOperation.Name) +
				" " + formatDuration(time.Duration(run.OutstandingOperation.AgeMS)*time.Millisecond)
		}
		rows = append(rows, []string{
			style.primary.Render(run.IssueIdentifier),
			run.IssueState,
			formatSince(now, run.StartedAt),
			formatSince(now, run.LastActivityAt),
			fmt.Sprintf("%d%s", run.TurnCount, turns),
			fmt.Sprintf("%d", run.Usage.TotalTokens),
			waiting,
		})
	}
	return style.borderless(
		[]string{"ISSUE", "STATE", "RUN", "QUIET", "TURNS", "TOKENS", "WAITING"},
		rows, 2, 3, 4, 5)
}

// retryTable lists the issues waiting for another attempt.
func (m Model) retryTable(instance operator.Instance, now time.Time, style theme) string {
	retrying := instance.Snapshot.Coordinator.Retrying
	if len(retrying) == 0 {
		return ""
	}
	rows := make([][]string, 0, len(retrying))
	for _, retry := range retrying {
		rows = append(rows, []string{
			style.primary.Render(retry.IssueIdentifier),
			retry.Kind + "/" + retry.Reason,
			fmt.Sprintf("%d", retry.Attempt),
			formatTimeOrAge(now, retry.Due),
		})
	}
	return style.borderless([]string{"RETRY", "REASON", "ATTEMPT", "DUE"}, rows, 2)
}

// configPanel groups the effective configuration. It renders only
// CredentialPresence.Configured; the environment names and file references that
// operator.EffectiveConfig also carries stay unprinted on every surface.
func (m Model) configPanel(instance operator.Instance, style theme) string {
	config := instance.Config
	if config == nil {
		return style.warn.Render("Effective configuration is unavailable; see Validation.")
	}
	scheduling := [][2]string{
		{"Poll interval", config.PollInterval.String()},
		{"Agent capacity", fmt.Sprintf("%d", config.MaxConcurrentAgents)},
		{"Max turns", fmt.Sprintf("%d", config.MaxTurns)},
	}
	if len(config.MaxConcurrentByState) > 0 {
		scheduling = append(scheduling, [2]string{"State capacity", rateLimitsInt(config.MaxConcurrentByState)})
	}
	return style.definitions([]definition{
		{caption: "Delivery", rows: [][2]string{
			{"Workflow", empty(instance.Paths.Workflow)},
			{"Repository", empty(config.WorkspaceSource)},
			{"Workspace root", empty(config.WorkspaceRoot)},
		}},
		{caption: "Tracker", rows: [][2]string{
			{"Linear project", empty(config.ProjectSelector)},
			{"Active states", comma(config.ActiveStates)},
			{"Terminal states", comma(config.TerminalStates)},
			{"Handoff state", empty(config.HandoffState)},
			{"Merge state", empty(config.MergeState)},
		}},
		{caption: "Scheduling", rows: scheduling},
		{caption: backendLabel(config.AgentBackend), rows: [][2]string{
			{"Command", empty(config.CodexCommand)},
			{"Timeouts", fmt.Sprintf("turn %s; read %s; start %s; stall %s",
				config.TurnTimeout, config.ReadTimeout, config.StartTimeout, config.StallTimeout)},
			{"Approval policy", empty(config.CodexApprovalPolicy)},
			{"Thread sandbox", empty(config.CodexThreadSandbox)},
		}},
		{caption: "GitHub", rows: [][2]string{
			{"Repository", empty(config.GitHubOwner) + "/" + empty(config.GitHubRepository)},
			{"Base branch", empty(config.GitHubBaseBranch)},
			{"Merge method", empty(config.GitHubMergeMethod)},
			{"Required checks", comma(config.GitHubRequiredChecks)},
		}},
		{caption: "Credentials", rows: [][2]string{
			{"Linear", style.presence(config.Credentials.Tracker.Configured)},
			{"GitHub", style.presence(config.Credentials.GitHub.Configured)},
		}},
	})
}

// presence colors a credential's presence. It never renders the name or path
// behind it, which is the whole point of the presence type.
func (t theme) presence(value bool) string {
	if value {
		return t.ok.Render("configured")
	}
	return t.muted.Render("not configured")
}

// validationPanel tabulates the findings. Severity is colored and worded, so
// the distinction holds without color and for a red-green colorblind reader.
func (m Model) validationPanel(instance operator.Instance, style theme) string {
	overall := style.ok.Render("valid for read-only inspection")
	if instance.Liveness == operator.LivenessInvalid {
		overall = style.bad.Render("INVALID")
	}
	head := style.muted.Render("Overall result") + " " + overall
	if len(instance.Findings) == 0 {
		return head + "\n\n" + style.muted.Render("No validation findings.")
	}
	rows := make([][]string, 0, len(instance.Findings))
	for _, finding := range instance.Findings {
		severity := strings.ToUpper(string(finding.Severity))
		if finding.Severity == operator.SeverityError {
			severity = style.bad.Render(severity)
		} else {
			severity = style.warn.Render(severity)
		}
		rows = append(rows, []string{severity, finding.Code, finding.Message})
	}
	return head + "\n\n" + style.borderless([]string{"SEVERITY", "CODE", "MESSAGE"}, rows)
}

// splitView shows the list and the selected instance side by side. A window
// this wide can hold both, which removes the drill-in keypress between seeing
// an instance and reading it.
func (m Model) splitView(now time.Time, style theme) string {
	instance := m.instances[m.selected]
	current := m.detailPage()
	header := lipgloss.JoinVertical(lipgloss.Left,
		style.primary.Render("Symphony operator view")+"  "+style.emphasis.Render(instance.ID),
		style.liveness(instance.Liveness)+style.muted.Render(" · launchd "+launchdText(instance.Launchd)),
		style.tabs(current),
	)
	// The list is offered a third of the window, capped so an ultrawide terminal
	// spends its extra columns on the detail rather than on padding. It is then
	// measured rather than assumed: sizing the pane to a width the table did not
	// take wraps every row and the border with it.
	budget := style.mainBudget(header)
	list := m.instanceList(style.sized(min(44, style.width/3)-2), budget)
	// Three columns go to the two gutters and the rule between them.
	detail := m.detailBody(instance, current, now, style.sized(style.width-lipgloss.Width(list)-3), budget)
	// The divider spans whichever half is taller, so it reads as one boundary
	// down the screen rather than a rule that stops where the list happens to
	// end.
	main := lipgloss.JoinHorizontal(lipgloss.Top,
		style.pane().Height(max(lipgloss.Height(list), lipgloss.Height(detail))).Render(list),
		lipgloss.NewStyle().PaddingLeft(1).Render(detail),
	)
	return style.frame(header, main, m.statusLine(now, style),
		style.muted.Render("j/k select · s/c/v page · Tab next · r refresh · q quit"))
}
