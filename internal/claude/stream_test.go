package claude

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestStartRunsATurnAndNormalizesItsLifecycle is the happy path: a turn reports
// its session, pairs a tool call with its result, accumulates usage, and ends
// with exactly one terminal event before the stream closes.
func TestStartRunsATurnAndNormalizesItsLifecycle(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
		initLine(dir, allCodingTools)+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`+"\n"+
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`+"\n"+
		resultLine(false, "")+"\n"+
		"EOF\n")

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || session.ThreadID != session.ID || session.TurnID != "1" {
		t.Fatalf("session=%+v", session)
	}
	collected := drain(t, events)

	var started, item, usage, completed int
	for _, event := range collected {
		switch event.Kind {
		case domain.EventSessionStarted:
			started++
			if event.PID == 0 {
				t.Fatal("session start reported no pid")
			}
		case domain.EventItem:
			item++
		case domain.EventUsage:
			usage++
			// Input must fold the cache components: reporting input_tokens alone
			// would understate real input by orders of magnitude.
			if event.Usage.InputTokens != 2+5325+3289 || event.Usage.OutputTokens != 11 {
				t.Fatalf("usage=%+v", event.Usage)
			}
			if event.Usage.TotalTokens != event.Usage.InputTokens+event.Usage.OutputTokens {
				t.Fatalf("total=%+v", event.Usage)
			}
		case domain.EventCompleted:
			completed++
		case domain.EventFailed, domain.EventBlocked:
			t.Fatalf("unexpected terminal event: %+v", event)
		}
	}
	if started != 1 || item != 2 || usage != 1 || completed != 1 {
		t.Fatalf("started=%d item=%d usage=%d completed=%d (%v)", started, item, usage, completed, kinds(collected))
	}
	if lastKind(t, collected).Kind != domain.EventCompleted {
		t.Fatalf("terminal event was not last: %v", kinds(collected))
	}

	// The started and completed item records must pair by ID and be timed here,
	// because the CLI supplies neither discrete lifecycle events nor durations.
	var startedItem, finishedItem domain.Event
	for _, event := range collected {
		if event.Kind != domain.EventItem {
			continue
		}
		if event.Outcome == domain.ItemStarted {
			startedItem = event
		} else {
			finishedItem = event
		}
	}
	if startedItem.ItemID != "call-1" || finishedItem.ItemID != "call-1" {
		t.Fatalf("item ids: %q / %q", startedItem.ItemID, finishedItem.ItemID)
	}
	if startedItem.ItemType != "commandExecution" || startedItem.ToolName != "Bash" {
		t.Fatalf("item classification=%+v", startedItem)
	}
	if finishedItem.Outcome != domain.ItemCompleted {
		t.Fatalf("finished outcome=%q", finishedItem.Outcome)
	}
}

// TestMCPCapabilityToolCallIsIdentifiableByNameInTheEvent pins PMR-163: an
// operator debugging a run that spent its turn budget on one bound capability
// (for example github_publish_pr) must be able to tell which capability was
// called from the log alone, not only that "a toolCall" happened. This backend
// already carries the CLI's own MCP-framed tool name onto ToolName for every
// item it emits (permission_denied, tool_use, and tool_result alike); this
// pins that guarantee for a bound capability specifically, whose ToolName the
// coordinator's "agent item event" debug record surfaces as item_name
// (internal/coordinator/events.go) once it reaches here.
func TestMCPCapabilityToolCallIsIdentifiableByNameInTheEvent(t *testing.T) {
	capabilityTool := mcpToolName("github_publish_pr")
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
		initLine(dir, allCodingTools)+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"`+capabilityTool+`"}]}}`+"\n"+
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`+"\n"+
		resultLine(false, "")+"\n"+
		"EOF\n")

	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)

	var startedItem domain.Event
	found := false
	for _, event := range collected {
		if event.Kind == domain.EventItem && event.Outcome == domain.ItemStarted {
			startedItem, found = event, true
		}
	}
	if !found {
		t.Fatalf("no started item event: %v", kinds(collected))
	}
	if startedItem.ItemType != "mcpToolCall" || startedItem.ToolName != capabilityTool {
		t.Fatalf("capability call classification = %+v, want item_type=mcpToolCall tool_name=%q", startedItem, capabilityTool)
	}
}

// TestASandboxDeniedLoopbackBindIsReportedAsADiagnostic covers the failure mode
// this backend cannot otherwise distinguish from a real test regression: a
// failed Bash call whose own output shows the sandbox refused a loopback
// bind. Without this, an operator reading only item outcomes sees the same
// "Bash failed" either way.
func TestASandboxDeniedLoopbackBindIsReportedAsADiagnostic(t *testing.T) {
	for name, resultContent := range map[string]string{
		"a plain string result": `"listen tcp 127.0.0.1:0: bind: operation not permitted\n"`,
		"a text content block":  `[{"type":"text","text":"panic: listen tcp6 [::1]:0: bind: operation not permitted"}]`,
		"an unrelated failure":  `"exit status 1: FAIL github.com/example/pkg"`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
				initLine(dir, allCodingTools)+"\n"+
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`+"\n"+
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true,"content":`+resultContent+`}]}}`+"\n"+
				resultLine(false, "")+"\n"+
				"EOF\n")

			backend := New(settingsFunc())
			_, events, err := backend.Start(context.Background(), request(t, dir, script))
			if err != nil {
				t.Fatal(err)
			}
			collected := drain(t, events)

			var diagnosed bool
			for _, event := range collected {
				if event.Kind == domain.EventDiagnostic && strings.Contains(event.Message, "loopback bind") {
					diagnosed = true
				}
			}
			wantDiagnosed := name != "an unrelated failure"
			if diagnosed != wantDiagnosed {
				t.Fatalf("diagnosed=%v, want %v (%v)", diagnosed, wantDiagnosed, kinds(collected))
			}
		})
	}
}

// TestAPolicyThatDidNotApplyFailsTheTurnClosed covers the only confirmation the
// CLI offers. A settings payload it cannot parse is ignored silently, so the init
// echo is the sole evidence the boundary is in force -- and a mismatch must end
// the turn rather than run under an unknown boundary.
func TestAPolicyThatDidNotApplyFailsTheTurnClosed(t *testing.T) {
	for name, build := range map[string]func(dir string) string{
		"permission mode was ignored": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"acceptEdits","tools":[` + allCodingTools + `],"mcp_servers":[]}`
		},
		"an extra tool is available": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"dontAsk","tools":[` + allCodingTools + `,"WebFetch"],"mcp_servers":[]}`
		},
		"the tool surface shrank": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"dontAsk","tools":["Read"],"mcp_servers":[]}`
		},
		"an MCP server is attached": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"dontAsk","tools":[` + allCodingTools + `],"mcp_servers":[{"name":"other","status":"connected"}]}`
		},
		"no working directory": func(string) string {
			return `{"type":"system","subtype":"init","cwd":"","permissionMode":"dontAsk","tools":[` + allCodingTools + `],"mcp_servers":[]}`
		},
		// A turn running somewhere other than this issue's worktree would write
		// outside the boundary the sandbox was built around.
		"a different working directory": func(string) string {
			return `{"type":"system","subtype":"init","cwd":"/somewhere/else","permissionMode":"dontAsk","tools":[` + allCodingTools + `],"mcp_servers":[]}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+build(dir)+"\n"+resultLine(false, "")+"\nEOF\n")
			backend := New(settingsFunc())
			_, events, err := backend.Start(context.Background(), request(t, dir, script))
			if err != nil {
				t.Fatal(err)
			}
			collected := drain(t, events)
			for _, event := range collected {
				if event.Kind == domain.EventCompleted {
					t.Fatalf("a turn whose policy did not apply completed: %v", kinds(collected))
				}
			}
			failure := lastKind(t, collected)
			if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "refused") {
				t.Fatalf("terminal event=%+v", failure)
			}
		})
	}
}

// TestAMissingInitEventFailsTheTurn covers a turn that reports a result without
// ever announcing its policy.
func TestAMissingInitEventFailsTheTurn(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	failure := lastKind(t, drain(t, events))
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "no init event") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestAnErrorResultFailsTheTurnEvenWhenTheSubtypeSaysSuccess is the
// authentication case: the CLI reports subtype "success" with is_error true, so
// reading subtype as the success signal would record a failed turn as completed.
func TestAnErrorResultFailsTheTurnEvenWhenTheSubtypeSaysSuccess(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		resultLine(true, `"terminal_reason":"api_error","api_error_status":"401"`)+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	failure := lastKind(t, collected)
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "api_error") {
		t.Fatalf("terminal event=%+v", failure)
	}
	for _, event := range collected {
		if event.Kind == domain.EventCompleted {
			t.Fatal("an error result was reported as a completed turn")
		}
	}
}

// TestMalformedAndOversizedOutputIsSkippedNotFatal keeps one bad line from
// ending a run that is otherwise progressing. An oversized line is normal
// traffic here: a single assistant message or tool result is one line.
func TestMalformedAndOversizedOutputIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, ""+
		"cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nnot json at all\n{\"type\":}\n[]\n\nEOF\n"+
		// A line past the scanner bound, emitted without a trailing newline
		// problem by padding a valid envelope.
		"printf '{\"type\":\"assistant\",\"pad\":\"'; head -c 9000000 /dev/zero | tr '\\0' 'x'; printf '\"}\\n'\n"+
		"cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	if lastKind(t, collected).Kind != domain.EventCompleted {
		t.Fatalf("run did not complete past malformed output: %v", kinds(collected))
	}
}

// TestTurnTimeoutIsReportedAndKillsTheProcessGroup bounds a turn that never
// produces a result.
func TestTurnTimeoutIsReportedAndKillsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\nsleep 120\n")
	r := request(t, dir, script)
	r.TurnTimeout = 300 * time.Millisecond
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	failure := lastKind(t, drain(t, events))
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "timeout") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestAnExitWithoutAResultFailsTheTurn keeps a silent child from looking like a
// completed turn.
func TestAnExitWithoutAResultFailsTheTurn(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\necho 'boom' >&2\nexit 3\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	failure := lastKind(t, collected)
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "exit status 3") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestUsageAccumulatesAcrossTurns matters because the CLI reports usage per turn
// while the scheduler keeps a component-wise maximum across a run: reporting the
// per-turn figure would make a resumed run under-report.
func TestUsageAccumulatesAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	firstUsage := usageOf(t, drain(t, events))
	events, err = backend.Continue(context.Background(), session, "again")
	if err != nil {
		t.Fatal(err)
	}
	secondUsage := usageOf(t, drain(t, events))
	if secondUsage.OutputTokens != 2*firstUsage.OutputTokens || secondUsage.InputTokens != 2*firstUsage.InputTokens {
		t.Fatalf("usage did not accumulate: first=%+v second=%+v", firstUsage, secondUsage)
	}
}

// TestUsageIsLiveAndSurvivesATimeout is the fix for the field that always read
// zero for a session actively spending tokens: usage must be observable while a
// turn is in flight, not only once it produces a result, and it must be the
// last thing reported even when the turn never gets that far because
// turn_timeout_ms killed it first (PMR-131's failure mode).
func TestUsageIsLiveAndSurvivesATimeout(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
		initLine(dir, allCodingTools)+"\n"+
		assistantUsageLine(2, 11, 5325, 3289)+"\n"+
		"EOF\nsleep 120\n")
	r := request(t, dir, script)
	r.TurnTimeout = 300 * time.Millisecond
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	usage := usageOf(t, collected)
	if usage.InputTokens != 2+5325+3289 || usage.OutputTokens != 11 {
		t.Fatalf("live usage=%+v", usage)
	}
	failure := lastKind(t, collected)
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "timeout") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestADeniedToolCallIsReportedWithoutItsArguments keeps a refusal observable
// while the denied arguments stay out of every event.
func TestADeniedToolCallIsReportedWithoutItsArguments(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"call-9","tool_input":{"command":"curl http://secret.example"}}`+"\n"+
		resultLine(false, `"permission_denials":[{"tool_name":"Bash","tool_use_id":"call-9","tool_input":{"command":"curl http://secret.example"}}]`)+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	var declined bool
	for _, event := range collected {
		if event.Kind == domain.EventItem && event.Outcome == domain.ItemDeclined {
			declined = true
			if event.ToolName != "Bash" || event.ItemID != "call-9" {
				t.Fatalf("declined item=%+v", event)
			}
		}
		// No event may carry the denied command.
		if strings.Contains(event.Message, "secret.example") || strings.Contains(event.ItemID, "secret.example") {
			t.Fatalf("event leaked denied tool arguments: %+v", event)
		}
	}
	if !declined {
		t.Fatalf("a denied call was not reported: %v", kinds(collected))
	}
}
