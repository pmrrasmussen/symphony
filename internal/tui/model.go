// Package tui provides Symphony's deliberately small, read-only operator UI.
// It consumes the operator discovery model and never opens a connection to a
// running daemon or any remote service.
package tui

import (
	"context"
	"sort"
	"strings"
	"time"

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
