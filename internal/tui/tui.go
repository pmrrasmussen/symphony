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
	// failed marks the last discovery as having failed, so the status line can
	// say so in the color that means it.
	failed bool
	// layout draws the terminal dashboard. It stays off for redirected output,
	// which keeps pipes, files, and tests on plain text.
	layout bool
	// color allows hue on the dashboard. NO_COLOR clears it without taking the
	// dashboard, its keys, or its alternate screen away.
	color bool
	// width and height are the terminal dimensions, or zero when they are not
	// known and the frame should size itself to its content and clamp nothing.
	width  int
	height int
	// offset is the first detail-body row drawn. The alternate screen has no
	// scrollback, so without this the rows past the window are unreachable
	// rather than merely off screen.
	offset int
}

// scrollToEnd is an offset past any possible content. The renderer knows the
// body's real length and the driver clamps to it on the next frame, so jumping
// to the bottom does not need the length here.
const scrollToEnd = 1 << 30

// viewportRows is how many rows a detail body may draw, or zero when the height
// is unknown and nothing is clipped.
func (m Model) viewportRows() int {
	if !m.layout || m.height <= 0 {
		return 0
	}
	// Three header rows, two rules, the status line, and the hint bar.
	return m.height - 7
}

// scrollStep is half a screen, which is the vim convention the scroll keys
// borrow. It falls back to a few lines when the height is unknown.
func (m Model) scrollStep() int {
	if rows := m.viewportRows(); rows > 0 {
		return max(1, rows/2)
	}
	return 5
}

// Layout bands. The dashboard has to read in a sixty-column tmux split as well
// as on an ultrawide, and below the floor there is no honest arrangement of the
// data at all.
const (
	splitWidth  = 120 // reserved for a side-by-side list and detail
	narrowWidth = 80  // below this the two numeric columns drop
	minWidth    = 60
	minHeight   = 14
)

type band uint8

const (
	bandNarrow band = iota
	bandStandard
	bandWide
)

// bandFor picks the layout band for a width. An unknown width is treated as the
// widest band, which keeps every width-unaware caller rendering every column.
func bandFor(width int) band {
	switch {
	case width <= 0 || width >= splitWidth:
		return bandWide
	case width >= narrowWidth:
		return bandStandard
	default:
		return bandNarrow
	}
}

// theme holds the styles one frame is drawn with, named by what they mean
// rather than by how they look, so a color decision lives in exactly one place.
// Every style is transparent in the plain theme, which is what lets the same
// renderer serve a terminal and a pipe.
type theme struct {
	// layout draws the dashboard: rules, tables, and a frame fitted to the
	// window. It stays off for redirected output, which keeps pipes, files, and
	// tests on plain text.
	layout bool
	// color emits hue. NO_COLOR turns it off without taking the dashboard away.
	color  bool
	width  int
	height int

	primary  lipgloss.Style // titles and the selected row
	muted    lipgloss.Style // metadata, timestamps, the hint bar
	emphasis lipgloss.Style // column headers and the open tab
	ok       lipgloss.Style
	warn     lipgloss.Style
	bad      lipgloss.Style
	rule     lipgloss.Style // the horizontal separators
}

// newTheme uses the sixteen named ANSI colors rather than fixed RGB, so the
// dashboard follows whatever palette the terminal is themed with and reads
// correctly on both light and dark backgrounds without querying either.
func newTheme(layout, color bool, width, height int) theme {
	style := theme{layout: layout, color: color, width: width, height: height}
	if !layout {
		return style
	}
	// Weight and dimming are not color, so they survive NO_COLOR: the hierarchy
	// stays readable when only hue is given up.
	style.primary = lipgloss.NewStyle().Bold(true)
	style.emphasis = lipgloss.NewStyle().Bold(true)
	style.muted = lipgloss.NewStyle().Faint(true)
	style.rule = lipgloss.NewStyle().Faint(true)
	if !color {
		return style
	}
	style.muted = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	style.emphasis = style.emphasis.Foreground(lipgloss.Cyan)
	style.rule = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	style.ok = lipgloss.NewStyle().Foreground(lipgloss.Green)
	style.warn = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	style.bad = lipgloss.NewStyle().Foreground(lipgloss.Red)
	return style
}

// colorEnabled reports whether hue may be emitted. NO_COLOR is honored when it
// is set to a non-empty value, per no-color.org. The dashboard itself stays,
// because every state on it is paired with a word and a shape.
func colorEnabled() bool {
	value, present := os.LookupEnv("NO_COLOR")
	return !(present && value != "")
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

// livenessGlyph gives every state its own shape. One dot repeated on every row
// is texture rather than information, and a shape that differs per state keeps
// the distinction legible wherever color is unavailable.
func livenessGlyph(state operator.Liveness) string {
	switch state {
	case operator.LivenessRunning:
		return "\u25cf"
	case operator.LivenessStopped:
		return "\u25cb"
	case operator.LivenessStale:
		return "\u25d0"
	default:
		return "\u2715"
	}
}

func (t theme) livenessStyle(state operator.Liveness) lipgloss.Style {
	switch state {
	case operator.LivenessRunning:
		return t.ok
	case operator.LivenessStopped:
		return t.muted
	case operator.LivenessStale:
		return t.warn
	default:
		return t.bad
	}
}

// liveness renders a liveness as a shape and a word, colored to agree with
// both. Color is the fastest thing to read on this screen, and the glyph and
// the word are what remain when it is gone.
func (t theme) liveness(state operator.Liveness) string {
	label := livenessLabel(state)
	if !t.layout {
		return label
	}
	return t.livenessStyle(state).Render(livenessGlyph(state) + " " + label)
}

// checks colors a findings summary by its worst severity. The dashboard form is
// abbreviated because this cell is the widest on the row and wrapped once the
// window narrowed.
func (t theme) checks(findings []operator.Finding) string {
	if !t.layout {
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
	summary, style := "ok", t.ok
	switch {
	case errors > 0 && warnings > 0:
		summary, style = fmt.Sprintf("%d err, %d warn", errors, warnings), t.bad
	case errors > 0:
		summary, style = fmt.Sprintf("%d err", errors), t.bad
	case warnings > 0:
		summary, style = fmt.Sprintf("%d warn", warnings), t.warn
	}
	return style.Render(summary)
}

// tabs renders the detail page strip with the open page marked, which the
// header never showed before. The keys that drive it live in the hint bar, so
// the strip itself stays one line.
func (t theme) tabs(current page) string {
	if !t.layout {
		return "[s] Status  [c] Config  [v] Validation  [Tab] next  [r] refresh  [q] back"
	}
	labels := []struct {
		page  page
		label string
	}{
		{statusPage, "Status"},
		{configPage, "Config"},
		{validationPage, "Validation"},
	}
	rendered := make([]string, 0, len(labels))
	for _, entry := range labels {
		style := t.muted.Padding(0, 1)
		if entry.page == current {
			style = t.emphasis.Padding(0, 1)
		}
		rendered = append(rendered, style.Render(entry.label))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}

// horizontalRule draws one separator across the frame. Rules rather than a
// border: the terminal edge already frames the app, so a second frame drawn
// just inside it separates nothing and costs two columns and two rows.
func (t theme) horizontalRule() string {
	width := t.width
	if width <= 0 {
		width = narrowWidth
	}
	return t.rule.Render(strings.Repeat("\u2500", width))
}

// mainBudget is how many rows the main area may draw. Zero means the height is
// unknown -- a pipe, or a caller that never saw a window size -- and that
// nothing should be clamped.
func (t theme) mainBudget(header string) int {
	if !t.layout || t.height <= 0 {
		return 0
	}
	// Two rules, the status line, and the hint bar.
	return t.height - lipgloss.Height(header) - 4
}

// frame assembles the four sections every full-screen view shares: persistent
// context at the top, the main area, one line of ephemeral feedback, and the
// hint bar. Their order and positions are fixed, because a layout the eye can
// learn is most of what makes one feel calm.
func (t theme) frame(header, main, status, hints string) string {
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		t.horizontalRule(),
		main,
		t.horizontalRule(),
		status,
		hints,
	) + "\n"
}

// window returns the run of items that fits in limit rows, positioned so that
// index stays visible, and reports how many it left out. This is not scrolling:
// no offset is retained between frames. It only guarantees that the row the
// user has selected is one they can see.
func window[T any](items []T, index, limit int) ([]T, int) {
	if limit <= 0 || len(items) <= limit {
		return items, 0
	}
	if limit == 1 {
		return nil, len(items)
	}
	// One row of the budget goes to the line that reports the remainder.
	size := limit - 1
	start := 0
	if index >= size {
		start = index - size + 1
	}
	return items[start : start+size], len(items) - size
}

// more reports what clamp dropped. Silent truncation would read as a complete
// screen, which is the one thing an operator view must never do.
func (t theme) more(hidden int) string {
	if hidden <= 0 {
		return ""
	}
	return t.muted.Render(fmt.Sprintf("+%d more", hidden))
}

// tooSmall reports whether the window is below the size any honest layout
// needs. Naming the requirement beats drawing a wrapped, unreadable frame.
func (t theme) tooSmall() bool {
	if !t.layout {
		return false
	}
	return (t.width > 0 && t.width < minWidth) || (t.height > 0 && t.height < minHeight)
}

func (t theme) tooSmallFrame() string {
	message := t.warn.Render(fmt.Sprintf("terminal too small: need %dx%d", minWidth, minHeight))
	if t.width > 0 && t.height > 0 {
		return lipgloss.Place(t.width, t.height, lipgloss.Center, lipgloss.Center, message)
	}
	return message + "\n"
}

// New builds an overview model from an already-discovered, sorted instance
// list. A copy prevents callers from mutating the UI's current frame.
func New(instances []operator.Instance, now time.Time) Model {
	copy := append([]operator.Instance(nil), instances...)
	sort.Slice(copy, func(i, j int) bool { return copy[i].ID < copy[j].ID })
	return Model{instances: copy, updatedAt: now}
}

// splitLayout reports whether the window is wide enough to show the list and
// the selected instance at once. It is derived from the width rather than
// stored, so it cannot fall out of step with the last resize. An unknown width
// stays on the drill-down layout, which is what every width-unaware caller and
// the plain surface get.
func (m Model) splitLayout() bool {
	return m.layout && m.width >= splitWidth && len(m.instances) > 0
}

// detailPage is the page the detail body shows. The split layout has no
// separate overview to be on, so the selected instance's Status is its resting
// state.
func (m Model) detailPage() page {
	if m.page == overviewPage {
		return statusPage
	}
	return m.page
}

// inspecting reports whether the detail keys apply: either the user drilled
// into a page, or the split layout is already showing one beside the list.
func (m Model) inspecting() bool {
	return m.page != overviewPage || m.splitLayout()
}

// Update applies one small keyboard action. It returns true when the caller
// should leave the UI. Refresh is handled by Run so it can reread the host.
func (m Model) Update(key string) (Model, bool) {
	key = normalizeKey(key)
	page, selected := m.page, m.selected
	switch key {
	case "q":
		// There is nothing to back out to when the detail is already beside the
		// list, so the split layout quits on the first press.
		if m.page == overviewPage || m.splitLayout() {
			return m, true
		}
		m.page = overviewPage
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter":
		// Inert in the split layout: the page it would open is already open.
		if m.page == overviewPage && !m.splitLayout() && len(m.instances) > 0 {
			m.page = statusPage
		}
	case "tab":
		if m.inspecting() {
			m.page = m.detailPage() + 1
			if m.page > validationPage {
				m.page = statusPage
			}
		}
	case "s":
		if m.inspecting() {
			m.page = statusPage
		}
	case "c":
		if m.inspecting() {
			m.page = configPage
		}
	case "v":
		if m.inspecting() {
			m.page = validationPage
		}
	case "ctrl+d", "pgdown":
		if m.inspecting() {
			m.offset += m.scrollStep()
		}
	case "ctrl+u", "pgup":
		if m.inspecting() {
			m.offset = max(0, m.offset-m.scrollStep())
		}
	case "g":
		if m.inspecting() {
			m.offset = 0
		}
	case "G":
		if m.inspecting() {
			m.offset = scrollToEnd
		}
	}
	if m.page != page || m.selected != selected {
		// Different content behind the viewport, so start it at the top rather
		// than part way down someone else's page.
		m.offset = 0
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
	case "G", "shift+g":
		// Lowercasing below would make this indistinguishable from g.
		return "G"
	default:
		return strings.ToLower(strings.TrimSpace(key))
	}
}

// Refresh replaces the current frame. Selection stays on the same position
// where possible, so an instance restart cannot crash or disorient the UI.
func (m *Model) Refresh(instances []operator.Instance, err error, now time.Time) {
	if err != nil {
		m.message = "refresh failed: " + err.Error()
		m.failed = true
		return
	}
	m.failed = false
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
	frame, _ := m.render(now)
	return frame
}

// render draws the frame and reports the furthest detail offset its content
// allows. The driver uses that to correct a scroll that ran past the end, which
// is what lets the bottom key not need the body's length up front.
func (m Model) render(now time.Time) (string, int) {
	style := newTheme(m.layout, m.color, m.width, m.height)
	if style.tooSmall() {
		return style.tooSmallFrame(), 0
	}
	if m.splitLayout() {
		return m.splitView(now, style)
	}
	if m.page == overviewPage {
		return m.overview(now, style), 0
	}
	if len(m.instances) == 0 {
		return "Symphony operator view\n\nNo configured Symphony instances.\n\nq: quit\n", 0
	}
	instance := m.instances[m.selected]
	if !style.layout {
		return m.plainDetail(instance, now, style), 0
	}
	header := lipgloss.JoinVertical(lipgloss.Left,
		style.primary.Render("Symphony operator view")+"  "+style.emphasis.Render(instance.ID),
		style.liveness(instance.Liveness)+style.muted.Render(" · launchd "+launchdText(instance.Launchd)),
		style.tabs(m.page),
	)
	body, maxOffset := m.detailBody(instance, m.page, now, style, style.mainBudget(header))
	return style.frame(header, body, m.statusLine(now, style),
		style.muted.Render("j/k instance · s/c/v page · Tab next · r refresh · q back")), maxOffset
}

// plainDetail is the redirected detail page. Its wording is deliberately fixed:
// this is the surface pipes and scripts read, so the dashboard's tables are
// free to lay the same fields out differently without moving it.
func (m Model) plainDetail(instance operator.Instance, now time.Time, style theme) string {
	var b strings.Builder
	switch m.page {
	case statusPage:
		m.writeStatus(&b, instance, now)
	case configPage:
		m.writeConfig(&b, instance)
	case validationPage:
		m.writeValidation(&b, instance)
	}
	var plain strings.Builder
	fmt.Fprintf(&plain, "Symphony operator view  %s\n", instance.ID)
	fmt.Fprintf(&plain, "state: %s  launchd: %s  refreshed: %s\n\n", livenessLabel(instance.Liveness), launchdText(instance.Launchd), formatAge(now, m.updatedAt))
	fmt.Fprintf(&plain, "%s\n\n", style.tabs(m.page))
	return plain.String() + b.String()
}

// statusLine is the frame's one line of ephemeral feedback: what the last
// refresh did, and how long ago it happened. Errors take the line in the color
// that means it, rather than being appended somewhere quieter.
func (m Model) statusLine(now time.Time, style theme) string {
	text := "refreshed " + formatAge(now, m.updatedAt)
	if m.message != "" && m.message != "refreshed" {
		text = m.message + " · " + text
	}
	if m.failed {
		return style.bad.Render(text)
	}
	return style.muted.Render(text)
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
	fmt.Fprintf(b, "\n%s: %s\nTimeouts: turn %s; read %s; start %s; stall %s\nSandbox policy: approval %s; thread %s\n", backendLabel(config.AgentBackend), empty(config.CodexCommand), config.TurnTimeout, config.ReadTimeout, config.StartTimeout, config.StallTimeout, empty(config.CodexApprovalPolicy), empty(config.CodexThreadSandbox))
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
		model.failed = true
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
	model.layout = true
	model.color = colorEnabled()
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
		// The renderer sizes its rules and table to the width and budgets its
		// rows against the height.
		a.model.width, a.model.height = message.Width, message.Height
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
	frame, maxOffset := a.model.render(time.Now())
	// Correct a scroll that ran past the end, so scrolling back responds on the
	// first keypress rather than after as many as ran over.
	a.model.offset = min(a.model.offset, maxOffset)
	view := tea.NewView(frame)
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

// formatSince is how long ago a timestamp was, or unknown when the snapshot did
// not carry one. now.Sub(zero) is two and a half million hours, which is worse
// than admitting the field is missing.
func formatSince(now, then time.Time) string {
	if then.IsZero() {
		return "unknown"
	}
	return formatDuration(now.Sub(then))
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

// backendLabel names the agent runtime line after the selected backend. The
// empty case is defensive rather than expected: discovery always builds an
// EffectiveConfig from a freshly loaded workflow, where the resolved backend
// cannot be empty. It falls back to a neutral label rather than printing none.
func backendLabel(backend string) string {
	if backend == "" {
		return "Agent"
	}
	runes := []rune(backend)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
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
