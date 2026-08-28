package tui

import (
	"fmt"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

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
