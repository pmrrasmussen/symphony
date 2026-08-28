package claude

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestATurnEndsEvenWhenADescendantEscapesTheProcessGroup is the regression that
// matters most. A group kill cannot reach a process that left the group -- via
// setsid, nohup, or any double fork -- and such a process keeps the inherited
// stdout write end open. Reading would then never see EOF, so the turn would
// hang forever with no terminal event and no closed channel, and the timeout
// would be unenforceable. Closing the parent's pipe ends is what ends the read.
func TestATurnEndsEvenWhenADescendantEscapesTheProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed to detach a descendant from the process group")
	}
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\n"+
		// The grandchild leaves the process group and holds the inherited stdout
		// open well past the turn timeout.
		"python3 -c 'import os,time; os.setsid(); time.sleep(60)' &\n"+
		"sleep 60\n")
	r := request(t, dir, script)
	r.TurnTimeout = 400 * time.Millisecond

	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	// drain fails the test if the stream never closes, which is exactly the
	// defect: before the fix this blocked until the grandchild exited.
	collected := drain(t, events)
	failure := lastKind(t, collected)
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "timeout") {
		t.Fatalf("terminal event=%+v", failure)
	}
}
