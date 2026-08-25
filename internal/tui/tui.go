// Package tui provides Symphony's deliberately small, read-only operator UI.
// It consumes the operator discovery model and never opens a connection to a
// running daemon or any remote service.
package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

// Discover is the read-only boundary used by the UI. Keeping it injectable
// makes screen updates testable without a terminal or a local LaunchAgent.
type Discover func(context.Context, operator.Options) ([]operator.Instance, error)

// refreshInterval is how often an interactive view re-reads local discovery.
// Redirected output never polls, because nothing is watching it in real time.
const refreshInterval = 5 * time.Second

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
	// styled draws the terminal dashboard. It stays off for redirected
	// output, which keeps pipes, files, and tests on plain text.
	styled bool
	// width is the terminal width, or zero when it is not known and the
	// frame should size itself to its content.
	width int
}

// theme holds the styles one frame is drawn with. Every style is transparent
// in the plain theme, so the same renderer serves both surfaces.
type theme struct {
	styled  bool
	width   int
	title   lipgloss.Style
	subtle  lipgloss.Style
	heading lipgloss.Style
	panel   lipgloss.Style
	tab     lipgloss.Style
	current lipgloss.Style
}

// newTheme uses the sixteen named ANSI colors rather than fixed RGB, so the
// dashboard follows whatever palette the terminal is themed with and reads
// correctly on both light and dark backgrounds without querying either.
func newTheme(styled bool, width int) theme {
	if !styled {
		return theme{}
	}
	return theme{
		styled:  true,
		width:   width,
		title:   lipgloss.NewStyle().Bold(true),
		subtle:  lipgloss.NewStyle().Foreground(lipgloss.BrightBlack),
		heading: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan),
		panel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.BrightBlack).
			Padding(0, 1),
		tab:     lipgloss.NewStyle().Foreground(lipgloss.BrightBlack).Padding(0, 1),
		current: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan).Padding(0, 1),
	}
}

// livenessLabel names a liveness for display. The raw stale value is fourteen
// characters and overflowed its column, and it says nothing the short form
// does not.
func livenessLabel(state operator.Liveness) string {
	if state == operator.LivenessStale {
		return "stale"
	}
	return string(state)
}

// liveness renders a liveness as a colored marker. Color is the fastest thing
// to read on this screen, so it carries the same distinction as the word.
func (t theme) liveness(state operator.Liveness) string {
	label := livenessLabel(state)
	if !t.styled {
		return label
	}
	style := lipgloss.NewStyle()
	switch state {
	case operator.LivenessRunning:
		style = style.Foreground(lipgloss.Green)
	case operator.LivenessStopped:
		style = style.Foreground(lipgloss.BrightBlack)
	case operator.LivenessStale:
		style = style.Foreground(lipgloss.Yellow)
	default:
		style = style.Foreground(lipgloss.Red)
	}
	return style.Render("● " + label)
}

// checks colors a findings summary by its worst severity. The styled form is
// abbreviated because this cell is the widest on the row and wrapped once the
// window narrowed.
func (t theme) checks(findings []operator.Finding) string {
	if !t.styled {
		return findingsIndicator(findings)
	}
	errors, warnings := 0, 0
	for _, finding := range findings {
		if finding.Severity == operator.SeverityError {
			errors++
			continue
		}
		warnings++
	}
	summary := "ok"
	switch {
	case errors > 0 && warnings > 0:
		summary = fmt.Sprintf("%d err, %d warn", errors, warnings)
	case errors > 0:
		summary = fmt.Sprintf("%d err", errors)
	case warnings > 0:
		summary = fmt.Sprintf("%d warn", warnings)
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.Green)
	for _, finding := range findings {
		if finding.Severity == operator.SeverityError {
			style = style.Foreground(lipgloss.Red)
			break
		}
		style = style.Foreground(lipgloss.Yellow)
	}
	return style.Render(summary)
}

// tabs renders the detail page strip with the open page marked, which the
// header never showed before.
func (t theme) tabs(current page) string {
	labels := []struct {
		page  page
		label string
	}{
		{statusPage, "Status"},
		{configPage, "Config"},
		{validationPage, "Validation"},
	}
	if !t.styled {
		return "[s] Status  [c] Config  [v] Validation  [Tab] next  [r] refresh  [q] back"
	}
	rendered := make([]string, 0, len(labels))
	for _, entry := range labels {
		if entry.page == current {
			rendered = append(rendered, t.current.Render(entry.label))
			continue
		}
		rendered = append(rendered, t.tab.Render(entry.label))
	}
	strip := lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
	return strip + "\n" + t.subtle.Render("s/c/v jump · Tab next · r refresh · q back")
}

// frame wraps body text in a panel sized to the terminal, and returns it
// unchanged when there is nothing to draw a panel with.
func (t theme) frame(body string) string {
	if !t.styled {
		return body
	}
	panel := t.panel
	if t.width > 0 {
		panel = panel.Width(t.width)
	}
	return panel.Render(strings.TrimRight(body, "\n"))
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
	style := newTheme(m.styled, m.width)
	if m.page == overviewPage {
		return m.overview(now, style)
	}
	if len(m.instances) == 0 {
		return "Symphony operator view\n\nNo configured Symphony instances.\n\nq: quit\n"
	}
	instance := m.instances[m.selected]
	var b strings.Builder
	switch m.page {
	case statusPage:
		m.writeStatus(&b, instance, now)
	case configPage:
		m.writeConfig(&b, instance)
	case validationPage:
		m.writeValidation(&b, instance)
	}
	if !style.styled {
		var plain strings.Builder
		fmt.Fprintf(&plain, "Symphony operator view  %s\n", instance.ID)
		fmt.Fprintf(&plain, "state: %s  launchd: %s  refreshed: %s\n\n", livenessLabel(instance.Liveness), launchdText(instance.Launchd), formatAge(now, m.updatedAt))
		fmt.Fprintf(&plain, "%s\n\n", style.tabs(m.page))
		return plain.String() + b.String()
	}
	header := lipgloss.JoinVertical(lipgloss.Left,
		style.title.Render("Symphony operator view")+"  "+style.heading.Render(instance.ID),
		style.subtle.Render(fmt.Sprintf("%s · launchd %s · refreshed %s",
			livenessLabel(instance.Liveness), launchdText(instance.Launchd), formatAge(now, m.updatedAt))),
	)
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		"",
		style.tabs(m.page),
		"",
		style.frame(b.String()),
	) + "\n"
}

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
	if !style.styled {
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
	sections := []string{
		lipgloss.JoinVertical(lipgloss.Left,
			style.title.Render("Symphony operator view"),
			style.subtle.Render(fmt.Sprintf("%d instances · %d running · %d stopped · %d stale · %d invalid · refreshed %s",
				len(m.instances), running, stopped, stale, invalid, formatAge(now, m.updatedAt))),
		),
		"",
	}
	if m.message != "" {
		sections = append(sections, style.subtle.Render(m.message), "")
	}
	if len(m.instances) == 0 {
		sections = append(sections, style.frame("No convention-matching LaunchAgents were found."))
	} else {
		sections = append(sections, m.instanceTable(style))
	}
	sections = append(sections, "", style.subtle.Render("j/k select · ⏎ inspect · r refresh · q quit"))
	return lipgloss.JoinVertical(lipgloss.Left, sections...) + "\n"
}

// instanceTable draws the overview as a real table, so the column widths are
// computed from the content rather than hand-counted. The old header was a
// literal string and had drifted one column out of step with its rows.
func (m Model) instanceTable(style theme) string {
	rows := make([][]string, 0, len(m.instances))
	for index, instance := range m.instances {
		marker := "  "
		name := instance.ID
		if style.width > 0 {
			name = truncate(name, max(24, style.width/3))
		}
		if index == m.selected {
			// A marker and weight rather than a row background: the state and
			// checks cells carry their own foreground colors, and a row
			// background does not reach inside them, which renders patchy.
			marker = "▸ "
			name = style.title.Render(name)
		}
		active, capacity, retries := instanceActivity(instance)
		rows = append(rows, []string{
			marker + name,
			style.liveness(instance.Liveness),
			fmt.Sprintf("%d/%d", active, capacity),
			fmt.Sprintf("%d", retries),
			style.checks(instance.Findings),
		})
	}
	rendered := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)).
		BorderColumn(false).
		Headers("INSTANCE", "STATE", "AGENTS", "RETRIES", "CHECKS").
		Rows(rows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan).Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1)
		})
	if style.width > 0 {
		rendered = rendered.Width(style.width)
	}
	return rendered.String()
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
//
// A terminal gets the interactive program, which reads single keypresses and
// refreshes on a timer. Any other writer gets plain frames driven by
// line-buffered input, so redirected output stays free of the control
// sequences an interactive renderer needs.
func Run(ctx context.Context, input io.Reader, output io.Writer, discover Discover) error {
	instances, err := discover(ctx, operator.Options{})
	model := New(instances, time.Now())
	if err != nil {
		model.message = "initial discovery failed: " + err.Error()
	}
	if isTerminal(output) {
		return runInteractive(ctx, input, output, discover, model)
	}
	return runPlain(ctx, input, output, discover, model)
}

// isTerminal reports whether frames are going to a terminal. Only a terminal
// can use the interactive renderer: an interactive program writes cursor and
// key-protocol sequences unconditionally, and against a pipe it emits those
// sequences without ever delivering the frame text.
func isTerminal(output io.Writer) bool {
	file, ok := output.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runPlain repaints whole frames and reads one line per action. It stays the
// path for pipes, files, and tests, none of which can use raw mode.
func runPlain(ctx context.Context, input io.Reader, output io.Writer, discover Discover, model Model) error {
	reader := bufio.NewReader(input)
	for {
		if _, err := fmt.Fprint(output, model.View(time.Now())); err != nil {
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

// runInteractive drives the same Model from a terminal. Interruption is a
// normal way to close a read-only view, so it is not reported as a failure.
func runInteractive(ctx context.Context, input io.Reader, output io.Writer, discover Discover, model Model) error {
	model.styled = true
	view := &app{model: model, discover: discover, ctx: ctx}
	program := tea.NewProgram(view,
		tea.WithContext(ctx),
		// Input must be passed explicitly. Left to itself the program ignores
		// a redirected stdin, opens /dev/tty, and fails outright where there
		// is no controlling terminal.
		tea.WithInput(input),
		tea.WithOutput(output),
	)
	if _, err := program.Run(); err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// app adapts the pure Model to the interactive runtime. It contributes only
// key translation and scheduling; every state transition stays in
// Model.Update and Model.Refresh, which are testable without a terminal.
type app struct {
	model    Model
	discover Discover
	ctx      context.Context
	width    int
	height   int
}

// tickMsg asks for a scheduled refresh.
type tickMsg time.Time

// discoveredMsg carries the result of one completed discovery.
type discoveredMsg struct {
	instances []operator.Instance
	err       error
}

func (a *app) Init() tea.Cmd { return tickCmd() }

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(now time.Time) tea.Msg { return tickMsg(now) })
}

// discoverCmd runs discovery off the UI goroutine. Discovery shells out to
// plutil and launchctl and runs a preflight per instance, so on the UI
// goroutine it would freeze the view for the whole sweep.
func (a *app) discoverCmd() tea.Cmd {
	return func() tea.Msg {
		instances, err := a.discover(a.ctx, operator.Options{})
		return discoveredMsg{instances: instances, err: err}
	}
}

func (a *app) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tickMsg:
		return a, tea.Batch(a.discoverCmd(), tickCmd())
	case discoveredMsg:
		a.model.Refresh(message.instances, message.err, time.Now())
		return a, nil
	case tea.WindowSizeMsg:
		a.width, a.height = message.Width, message.Height
		// The renderer sizes its panels and table to the window.
		a.model.width = message.Width
		return a, nil
	case tea.KeyPressMsg:
		return a.press(message.String())
	}
	return a, nil
}

// press maps one keystroke onto the Model's existing key vocabulary.
func (a *app) press(pressed string) (tea.Model, tea.Cmd) {
	key := normalizeKey(pressed)
	switch key {
	case "ctrl+c":
		// Raw mode suppresses the interrupt signal, so the terminal's own
		// cancel key only reaches the program as an ordinary keypress.
		return a, tea.Quit
	case "r":
		return a, a.discoverCmd()
	}
	model, quit := a.model.Update(key)
	a.model = model
	if quit {
		return a, tea.Quit
	}
	return a, nil
}

func (a *app) View() tea.View {
	view := tea.NewView(a.model.View(time.Now()))
	view.AltScreen = true
	return view
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

// truncate shortens a value to a display width. It counts display cells and
// cuts on rune boundaries, so a multibyte identifier is neither mismeasured
// nor split part way through a character.
func truncate(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= max {
		return value
	}
	runes := []rune(value)
	if len(runes) > max-1 {
		runes = runes[:max-1]
	}
	return string(runes) + "…"
}
