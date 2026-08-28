package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

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
