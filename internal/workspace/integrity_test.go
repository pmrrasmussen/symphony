package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// TestSourceIntegritySnapshotIgnoresSymphonyBranches unit-tests the PMR-65
// backstop's capture primitive: the snapshot excludes the symphony/* publish
// branches Symphony itself creates, but records any other branch head.
func TestSourceIntegritySnapshotIgnoresSymphonyBranches(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()
	base, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "branch", "symphony/pmr-1")
	afterSymphony, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterSymphony.Refs, base.Refs) {
		t.Fatalf("symphony/* branch changed the snapshot: got=%v base=%v", afterSymphony.Refs, base.Refs)
	}
	runGit(t, source, "branch", "feature")
	afterFeature, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(afterFeature.Refs, base.Refs) {
		t.Fatalf("a new non-symphony branch head was not recorded: %v", afterFeature.Refs)
	}
}

// TestDiffSourceRefsExplainsOperatorFastForwardPulls proves the PMR-145 fix:
// an operator fast-forwarding a branch to a commit reachable from its
// remote-tracking ref -- the ordinary `git pull --ff-only` workflow -- is
// explained rather than flagged, while an arbitrary local write that does not
// land a commit any remote knows about still alerts.
func TestDiffSourceRefsExplainsOperatorFastForwardPulls(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()

	baseline, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a second operator pushing to the remote, then this checkout
	// running `git pull --ff-only`: refs/heads/main moves forward to a commit
	// reachable from refs/remotes/origin/main.
	publisher := cloneRepository(t, source)
	if err := os.WriteFile(filepath.Join(publisher, "upstream.txt"), []byte("landed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "upstream.txt")
	runGit(t, publisher, "commit", "-m", "operator landed a PR")
	runGit(t, publisher, "push", "origin", "main")
	runGit(t, source, "pull", "--ff-only")

	pulled, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	alerts, explained := diffSourceRefs(ctx, config.Settings{}, source, baseline.Refs, pulled.Refs)
	if len(alerts) != 0 {
		t.Fatalf("operator fast-forward pull was flagged as an alert: %+v", alerts)
	}
	if len(explained) != 1 || explained[0].Name != "refs/heads/main" {
		t.Fatalf("operator fast-forward pull was not explained: %+v", explained)
	}

	// Now simulate the breach the backstop exists to catch: main advances to a
	// commit no remote-tracking ref has ever heard of.
	if err := os.WriteFile(filepath.Join(source, "escape.txt"), []byte("agent write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "escape.txt")
	runGit(t, source, "commit", "-m", "agent wrote the source repository")

	written, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	alerts, explained = diffSourceRefs(ctx, config.Settings{}, source, pulled.Refs, written.Refs)
	if len(explained) != 0 {
		t.Fatalf("an unreachable local write was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("an unreachable local write was not flagged: %+v", alerts)
	}
	if alerts[0].Before != pulled.Refs["refs/heads/main"] || alerts[0].After != written.Refs["refs/heads/main"] {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestDiffSourceRefsFailsClosedOnClassificationFailure proves the PMR-147 fix:
// when classifying a genuinely changed ref cannot be completed because a git
// subprocess call fails for a reason other than a negative ancestry answer --
// here, a baseline value that names no object in the repository, the same
// shape of failure a pruned or missing object would produce in production --
// the ref is still reported as an alert, naming the classification failure,
// rather than dropped the way an unrelated Warn would drop it.
func TestDiffSourceRefsFailsClosedOnClassificationFailure(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()

	current, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	head := current.Refs["refs/heads/main"]
	if head == "" {
		t.Fatalf("expected refs/heads/main in snapshot: %+v", current.Refs)
	}
	baseline := map[string]string{"refs/heads/main": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}

	alerts, explained := diffSourceRefs(ctx, config.Settings{}, source, baseline, current.Refs)
	if len(explained) != 0 {
		t.Fatalf("an unclassifiable ref change was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("an unclassifiable ref change was not flagged: %+v", alerts)
	}
	if alerts[0].Reason == "" {
		t.Fatalf("alert did not name the classification failure: %+v", alerts[0])
	}
	if alerts[0].Before != baseline["refs/heads/main"] || alerts[0].After != head {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestDiffSourceRefsTreatsNotAncestorAsNegativeAnswer proves that
// merge-base --is-ancestor's exit code 1 -- a legitimate negative answer, not
// a subprocess failure -- still alerts (the ref moved to a commit that is not
// a fast-forward) but without being mistaken for a classification failure: no
// Reason is set.
func TestDiffSourceRefsTreatsNotAncestorAsNegativeAnswer(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()

	baseline, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	before := baseline.Refs["refs/heads/main"]

	// Move main sideways to a commit that shares no ancestry with before: a
	// hard reset onto an orphan commit is neither a fast-forward from before
	// nor an ancestor of it, so isAncestor(before, after) returns false, err
	// nil -- a genuine negative answer, not a failure.
	runGit(t, source, "checkout", "--orphan", "orphan")
	if err := os.WriteFile(filepath.Join(source, "orphan.txt"), []byte("orphan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "orphan.txt")
	runGit(t, source, "commit", "-m", "orphan commit")
	runGit(t, source, "branch", "-f", "main", "orphan")
	runGit(t, source, "checkout", "main")
	runGit(t, source, "branch", "-D", "orphan")

	current, err := captureSourceIntegrity(ctx, config.Settings{}, source)
	if err != nil {
		t.Fatal(err)
	}
	after := current.Refs["refs/heads/main"]
	if after == before {
		t.Fatalf("main did not move: %s", after)
	}

	alerts, explained := diffSourceRefs(ctx, config.Settings{}, source, baseline.Refs, current.Refs)
	if len(explained) != 0 {
		t.Fatalf("an orphaned reset was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("an orphaned reset was not flagged: %+v", alerts)
	}
	if alerts[0].Reason != "" {
		t.Fatalf("a legitimate not-an-ancestor answer was reported as a classification failure: %+v", alerts[0])
	}
}

// TestDiffSourceRefsAlertsOnDeletedRef proves a deleted source branch head
// always alerts: diffSourceRefs skips the fast-forward branch entirely when
// the ref no longer exists on the current side, so there is no path by which
// a deletion could be explained away.
func TestDiffSourceRefsAlertsOnDeletedRef(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()
	baseline := map[string]string{"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	current := map[string]string{}

	alerts, explained := diffSourceRefs(ctx, config.Settings{}, source, baseline, current)
	if len(explained) != 0 {
		t.Fatalf("a deleted ref was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("a deleted ref was not flagged: %+v", alerts)
	}
	if alerts[0].Before != baseline["refs/heads/main"] || alerts[0].After != "" {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestDiffSourceRefsAlertsOnNewRef proves a brand-new source branch head
// always alerts: diffSourceRefs skips the fast-forward branch entirely when
// the ref did not exist on the baseline side, so there is no path by which a
// new ref could be explained away.
func TestDiffSourceRefsAlertsOnNewRef(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()
	baseline := map[string]string{}
	current := map[string]string{"refs/heads/feature": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}

	alerts, explained := diffSourceRefs(ctx, config.Settings{}, source, baseline, current)
	if len(explained) != 0 {
		t.Fatalf("a new ref was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/feature" {
		t.Fatalf("a new ref was not flagged: %+v", alerts)
	}
	if alerts[0].Before != "" || alerts[0].After != current["refs/heads/feature"] {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestSourceIntegrityAlertFiresOnClassificationFailure proves the PMR-147 fix
// end to end: when AfterRun's ref classification cannot be completed for a
// changed ref, the integrity alert still fires at Error and names the
// classification failure, rather than degrading to the Warn that used to
// drop the ref change entirely.
func TestSourceIntegrityAlertFiresOnClassificationFailure(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-147", Identifier: "PMR-147"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitIntegrityBaseline == "" {
		t.Fatal("expected Prepare to record an integrity baseline")
	}

	// Corrupt the recorded baseline so main's classification against the
	// unmodified current ref value cannot resolve the before commit: this
	// stands in for a pruned or missing object surfacing mid-run.
	var baseline sourceIntegritySnapshot
	if err := json.Unmarshal([]byte(ws.GitIntegrityBaseline), &baseline); err != nil {
		t.Fatal(err)
	}
	if _, ok := baseline.Refs["refs/heads/main"]; !ok {
		t.Fatalf("expected refs/heads/main in baseline: %+v", baseline.Refs)
	}
	baseline.Refs["refs/heads/main"] = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	corrupted, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	ws.GitIntegrityBaseline = string(corrupted)

	// Move main so the ref is genuinely "changed" and classification is
	// attempted, rather than skipped as unchanged.
	if err := os.WriteFile(filepath.Join(source, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "change.txt")
	runGit(t, source, "commit", "-m", "advance main")

	l.AfterRun(context.Background(), ws, issue)

	var record struct {
		Level       string `json:"level"`
		Msg         string `json:"msg"`
		ChangedRefs string `json:"changed_refs"`
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Msg == "workspace source integrity alert" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("classification failure did not fire the integrity alert: %s", logs.String())
	}
	if record.Level != "ERROR" {
		t.Fatalf("integrity alert logged at %q, want ERROR", record.Level)
	}
	if !strings.Contains(record.ChangedRefs, "refs/heads/main") || !strings.Contains(record.ChangedRefs, "classification_failed=") {
		t.Fatalf("integrity alert did not name the classification failure: %q", record.ChangedRefs)
	}
}

// TestSourceIntegrityAlertIsStructuredAndNeverReachesStderr proves the PMR-65
// backstop alert lands in the operator log -- with the dedicated Operation and
// the issue attributes an operator needs to query for it -- instead of
// launchd's stderr file, which is not queryable and carries no issue
// attribution.
func TestSourceIntegrityAlertIsStructuredAndNeverReachesStderr(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-65", Identifier: "PMR-65"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitIntegrityBaseline == "" {
		t.Fatal("expected Prepare to record an integrity baseline")
	}
	// Simulate the breach the backstop exists to catch: a source branch head
	// moves during the run, despite the narrowed sandbox grant.
	runGit(t, source, "branch", "unexpected")

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = stderrWrite
	l.AfterRun(context.Background(), ws, issue)
	os.Stderr = origStderr
	if err := stderrWrite.Close(); err != nil {
		t.Fatal(err)
	}
	leaked, err := io.ReadAll(stderrRead)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaked) != 0 {
		t.Fatalf("integrity alert wrote to os.Stderr: %q", leaked)
	}

	var record struct {
		Level           string `json:"level"`
		Msg             string `json:"msg"`
		Operation       string `json:"operation"`
		IssueID         string `json:"issue_id"`
		IssueIdentifier string `json:"issue_identifier"`
		SourceRoot      string `json:"source_root"`
		ChangedRefs     string `json:"changed_refs"`
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Msg == "workspace source integrity alert" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("integrity alert was not logged as a structured record: %s", logs.String())
	}
	if record.Level != "ERROR" {
		t.Fatalf("integrity alert logged at %q, want ERROR", record.Level)
	}
	if record.Operation != string(observability.OperationSourceIntegrityAlert) {
		t.Fatalf("integrity alert operation=%q, want %q", record.Operation, observability.OperationSourceIntegrityAlert)
	}
	if record.IssueID != issue.ID || record.IssueIdentifier != issue.Identifier {
		t.Fatalf("integrity alert missing issue attributes: %+v", record)
	}
	if record.SourceRoot == "" {
		t.Fatal("integrity alert missing source_root")
	}
	if !strings.Contains(record.ChangedRefs, "refs/heads/unexpected") || !strings.Contains(record.ChangedRefs, "(none)") {
		t.Fatalf("integrity alert did not name the changed ref with its before/after values: %q", record.ChangedRefs)
	}
}

// TestSourceIntegrityAlertIsSilentForOperatorFastForwardPulls proves the
// PMR-145 fix end to end: an operator running `git pull --ff-only` in the
// source checkout while a run is in flight -- the documented operator
// workflow, and the scenario this issue observed firing on every run -- does
// not trip the PMR-65 backstop.
func TestSourceIntegrityAlertIsSilentForOperatorFastForwardPulls(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-145", Identifier: "PMR-145"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitIntegrityBaseline == "" {
		t.Fatal("expected Prepare to record an integrity baseline")
	}

	// A second operator merges a PR while the run is in flight, and this
	// checkout runs its ordinary `git pull --ff-only`.
	publisher := cloneRepository(t, source)
	if err := os.WriteFile(filepath.Join(publisher, "landed.txt"), []byte("landed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "landed.txt")
	runGit(t, publisher, "commit", "-m", "operator landed a PR")
	runGit(t, publisher, "push", "origin", "main")
	runGit(t, source, "pull", "--ff-only")

	l.AfterRun(context.Background(), ws, issue)

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Msg == "workspace source integrity alert" {
			t.Fatalf("operator fast-forward pull was reported as an integrity alert: %s", logs.String())
		}
	}
}
