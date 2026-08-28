package tui

import (
	"fmt"
	"os"
	"strings"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

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

// serviceStyle colors the daemon's own reported status.ProcessState, which is
// distinct from launchd's process observation above it: "stopping" is the
// graceful-shutdown window (PMR-119), during which a draining daemon must
// read differently from one that is simply running or already gone.
func (t theme) serviceStyle(state string) lipgloss.Style {
	switch state {
	case "running":
		return t.ok
	case "stopping":
		return t.warn
	case "stopped":
		return t.muted
	default:
		return lipgloss.NewStyle()
	}
}

// serviceState renders a status.ProcessState value with serviceStyle's color.
func (t theme) serviceState(state string) string {
	if !t.layout {
		return state
	}
	return t.serviceStyle(state).Render(state)
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
