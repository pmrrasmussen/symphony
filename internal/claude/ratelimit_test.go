package claude

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestARateLimitRejectionEndsTheTurnWithoutWaitingForAResult guards against a
// Claude quota rejection retrying as an ordinary agent failure (PMR-131): the
// rejection itself is the turn's one terminal event, carrying the backend's
// own status and reset time, even though the CLI still sends a failed result
// a moment later.
func TestARateLimitRejectionEndsTheTurnWithoutWaitingForAResult(t *testing.T) {
	dir := t.TempDir()
	resetsAt := time.Now().Add(90 * time.Minute).Unix()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","utilization":1,"resetsAt":`+strconv.FormatInt(resetsAt, 10)+`}}`+"\n"+
		resultLine(true, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	terminals := terminalEvents(collected)
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events: %v", len(terminals), kinds(collected))
	}
	terminal := terminals[0]
	if terminal.Kind != domain.EventRateLimited {
		t.Fatalf("terminal kind=%v, want EventRateLimited", terminal.Kind)
	}
	if terminal.RateLimitStatus != "rejected" {
		t.Fatalf("status=%q, want rejected", terminal.RateLimitStatus)
	}
	if !strings.Contains(terminal.Message, "rejected") || !strings.Contains(terminal.Message, "five_hour") {
		t.Fatalf("rate limit report lost its detail: %q", terminal.Message)
	}
	if terminal.RetryAfter <= 0 || terminal.RetryAfter > 91*time.Minute {
		t.Fatalf("retry_after=%s, want approximately 90m", terminal.RetryAfter)
	}
}

// TestARateLimitRejectionWithNoResetReportsAZeroRetryAfter covers the CLI
// reporting a rejection with no resetsAt at all: the scheduler's own floor
// covers this case (PMR-131), so the backend must not invent a reset time.
func TestARateLimitRejectionWithNoResetReportsAZeroRetryAfter(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","utilization":1}}`+"\n"+
		resultLine(true, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	terminals := terminalEvents(drain(t, events))
	if len(terminals) != 1 || terminals[0].Kind != domain.EventRateLimited {
		t.Fatalf("terminals=%v, want a single EventRateLimited", terminals)
	}
	if terminals[0].RetryAfter != 0 {
		t.Fatalf("retry_after=%s, want 0 with no resetsAt reported", terminals[0].RetryAfter)
	}
}

// TestAHealthyRateLimitStatusProducesNoEvent guards the benign half of this
// same seam (PMR-126): "allowed" is the default, healthy state on
// effectively every turn, so it must never reach even a diagnostic event.
func TestAHealthyRateLimitStatusProducesNoEvent(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","utilization":0.2}}`+"\n"+
		resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range drain(t, events) {
		if event.Kind == domain.EventDiagnostic || event.Kind == domain.EventRateLimited {
			t.Fatalf("a healthy allowed status reached an event: %+v", event)
		}
	}
}

// TestAThrottledRateLimitStatusIsANonTerminalDiagnostic covers
// "allowed_warning": worth an operator's attention, but not a rejection, so
// the turn must continue rather than end (PMR-126).
func TestAThrottledRateLimitStatusIsANonTerminalDiagnostic(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"five_hour","utilization":0.92}}`+"\n"+
		resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	var found bool
	for _, event := range collected {
		if event.Kind == domain.EventDiagnostic && event.RateLimitStatus == "allowed_warning" {
			found = true
			if !strings.Contains(event.Message, "allowed_warning") {
				t.Fatalf("rate limit diagnostic lost its detail: %q", event.Message)
			}
		}
	}
	if !found {
		t.Fatal("an allowed_warning status never reached a diagnostic")
	}
	terminals := terminalEvents(collected)
	if len(terminals) != 1 || terminals[0].Kind != domain.EventCompleted {
		t.Fatalf("terminals=%v, want a single EventCompleted", terminals)
	}
}

// TestAnUnrecognizedRateLimitStatusIsNormalizedBeforeItLeavesTheBackend keeps
// a future CLI status (which could contain arbitrary diagnostic text) out of
// domain.Event and therefore out of the coordinator's logs.
func TestAnUnrecognizedRateLimitStatusIsNormalizedBeforeItLeavesTheBackend(t *testing.T) {
	dir := t.TempDir()
	const rawStatus = "token=do-not-log-this"
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"`+rawStatus+`","rateLimitType":"five_hour","utilization":0.92}}`+"\n"+
		resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	var diagnostic *domain.Event
	for i := range collected {
		if collected[i].Kind == domain.EventDiagnostic {
			diagnostic = &collected[i]
			break
		}
	}
	if diagnostic == nil {
		t.Fatalf("events=%v, want an unrecognized rate-limit diagnostic", kinds(collected))
	}
	if diagnostic.RateLimitStatus != rateLimitStatusUnrecognized {
		t.Fatalf("status=%q, want %q", diagnostic.RateLimitStatus, rateLimitStatusUnrecognized)
	}
	if strings.Contains(diagnostic.Message, rawStatus) {
		t.Fatalf("raw rate-limit status crossed the backend boundary: %q", diagnostic.Message)
	}
}
