package status

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestWriteSerializesOnlyTheVersionedSafeSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "status.json")
	started := time.Date(2026, time.August, 25, 10, 0, 0, 0, time.UTC)
	publisher, err := New(path, Metadata{PID: 42, StartedAt: started, WorkflowPath: "/repo/WORKFLOW.md", LogRoot: "/repo/.symphony/logs"})
	if err != nil {
		t.Fatal(err)
	}
	publisher.now = func() time.Time { return started.Add(time.Minute) }
	snapshot := coordinator.Snapshot{
		Claimed: 1,
		Running: []coordinator.RunningSnapshot{{
			IssueIdentifier: "PMR-73", IssueState: "In Progress", LastEventAt: started,
			Usage:                domain.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
			OutstandingOperation: &coordinator.OutstandingOperationSnapshot{Type: "dynamicToolCall", Name: "github_publish_pr", StartedAt: started, AgeMS: 60000},
		}},
		Waiting: []coordinator.WaitingSnapshot{{IssueIdentifier: "PMR-77", IssueState: "Merging", Since: started, WaitingMS: 60000}},
	}
	if err := publisher.Write(Running, snapshot); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	if err := json.Unmarshal(contents, &got); err != nil {
		t.Fatalf("snapshot is not parseable JSON: %v\n%s", err, contents)
	}
	if got.SchemaVersion != SchemaVersion || got.PID != 42 || got.State != Running || got.Coordinator.Running[0].IssueState != "In Progress" || got.Coordinator.Running[0].OutstandingOperation.Name != "github_publish_pr" {
		t.Fatalf("snapshot=%+v", got)
	}
	if len(got.Coordinator.Waiting) != 1 || got.Coordinator.Waiting[0].IssueIdentifier != "PMR-77" || got.Coordinator.Waiting[0].IssueState != "Merging" {
		t.Fatalf("snapshot missing waiting entry=%+v", got.Coordinator)
	}
	for _, prohibited := range []string{"description", "prompt", "workspace", "arguments", "api_key", "credential"} {
		if strings.Contains(strings.ToLower(string(contents)), prohibited) {
			t.Fatalf("snapshot included prohibited %q: %s", prohibited, contents)
		}
	}
}

func TestWriteAtomicallyReplacesSnapshotsDuringConcurrentReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "status.json")
	publisher, err := New(path, Metadata{PID: 99, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Write(Running, coordinator.Snapshot{Claimed: 1}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	errs := make(chan error, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				var snapshot Snapshot
				if err := json.Unmarshal(contents, &snapshot); err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		if err := publisher.Write(Running, coordinator.Snapshot{Claimed: i}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-errs:
		t.Fatalf("reader observed partial replacement: %v", err)
	default:
	}
}

func TestWriteSecuresPathAndIndependentPublishers(t *testing.T) {
	dir := t.TempDir()
	first, err := New(filepath.Join(dir, "one", "status.json"), Metadata{PID: 101, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(filepath.Join(dir, "two", "status.json"), Metadata{PID: 202, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Write(Running, coordinator.Snapshot{Claimed: 1}); err != nil {
		t.Fatal(err)
	}
	if err := second.Write(Stopped, coordinator.Snapshot{Claimed: 2, Stopping: true}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		pid  int
	}{
		{first.path, 101},
		{second.path, 202},
	} {
		contents, err := os.ReadFile(test.path)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot Snapshot
		if err := json.Unmarshal(contents, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.PID != test.pid {
			t.Fatalf("%s pid=%d want %d", test.path, snapshot.PID, test.pid)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("status mode=%#o want 0600", info.Mode().Perm())
			}
			dirInfo, err := os.Stat(filepath.Dir(test.path))
			if err != nil {
				t.Fatal(err)
			}
			if dirInfo.Mode().Perm() != 0o700 {
				t.Fatalf("runtime directory mode=%#o want 0700", dirInfo.Mode().Perm())
			}
		}
	}
}

// TestRunSuppressesRepeatedIdenticalWriteFailures pins the shape of PMR-125: a
// structural failure like a non-owner-only status directory cannot self-heal,
// so it must not report once per tick for as long as the daemon runs.
func TestRunSuppressesRepeatedIdenticalWriteFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission semantics")
	}
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	publisher, err := New(filepath.Join(dir, "status.json"), Metadata{PID: 1, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reports []error
	interval := 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*interval)
	defer cancel()
	publisher.Run(ctx, interval, func() coordinator.Snapshot { return coordinator.Snapshot{} }, func(err error) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, err)
	})

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("got %d reports for an unchanging failure across %d ticks, want 1: %v", len(reports), 20, reports)
	}
	if !strings.Contains(reports[0].Error(), "owner-only") {
		t.Fatalf("report=%v, want owner-only directory refusal", reports[0])
	}
}

func TestWriteRejectsExistingDirectoryThatIsNotOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission semantics")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	publisher, err := New(filepath.Join(dir, "status.json"), Metadata{PID: 1, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Write(Running, coordinator.Snapshot{}); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("Write error=%v, want owner-only directory refusal", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("Write changed existing directory mode to %#o", info.Mode().Perm())
	}
}
