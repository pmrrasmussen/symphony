package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

func TestOverviewSelectionAndDetailNavigation(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := New([]operator.Instance{{ID: "com.pmrrasmussen.symphony.alpha", Liveness: operator.LivenessRunning}, {ID: "com.pmrrasmussen.symphony.beta", Liveness: operator.LivenessStopped}}, now)

	model, quit := model.Update("j")
	if quit || model.selected != 1 {
		t.Fatalf("down selection = %d, quit=%v", model.selected, quit)
	}
	model, quit = model.Update("enter")
	if quit || model.page != statusPage {
		t.Fatalf("open detail page=%v quit=%v", model.page, quit)
	}
	model, _ = model.Update("c")
	if model.page != configPage {
		t.Fatalf("config page=%v", model.page)
	}
	model, quit = model.Update("q")
	if quit || model.page != overviewPage {
		t.Fatalf("back page=%v quit=%v", model.page, quit)
	}
	_, quit = model.Update("q")
	if !quit {
		t.Fatal("overview q did not quit")
	}
}

func TestRefreshHandlesInstancesAppearingAndDisappearing(t *testing.T) {
	now := time.Now()
	model := New([]operator.Instance{{ID: "first"}, {ID: "second"}}, now)
	model.selected = 1
	model.Refresh(nil, nil, now)
	if model.selected != 0 || model.page != overviewPage || len(model.instances) != 0 {
		t.Fatalf("empty refresh model=%#v", model)
	}
	model.Refresh(nil, context.DeadlineExceeded, now)
	if !strings.Contains(model.message, "refresh failed") {
		t.Fatalf("refresh error message=%q", model.message)
	}
}

func TestSplitLayoutAppearsOnlyFromItsThreshold(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	instances := []operator.Instance{{ID: "one", Liveness: operator.LivenessRunning}}
	for _, testCase := range []struct {
		width int
		split bool
	}{
		{width: 0, split: false},
		{width: 100, split: false},
		{width: splitWidth - 1, split: false},
		{width: splitWidth, split: true},
		{width: 200, split: true},
	} {
		model := styledFixture(instances, now)
		model.width = testCase.width
		if model.splitLayout() != testCase.split {
			t.Fatalf("width %d: split=%v, want %v", testCase.width, model.splitLayout(), testCase.split)
		}
		// The hint bar is the honest signal: the split layout quits outright,
		// the drill-down backs out first.
		view := model.View(now)
		wantHint := "q quit"
		if !testCase.split && model.page != overviewPage {
			wantHint = "q back"
		}
		if !strings.Contains(view, wantHint) {
			t.Fatalf("width %d: hint bar missing %q:\n%s", testCase.width, wantHint, view)
		}
	}
}

func TestSplitLayoutQuitsWithoutBackingOutFirst(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := splitFixture([]operator.Instance{{ID: "one", Liveness: operator.LivenessRunning}}, now)

	// Enter has nothing to open: the page it would drill into is already beside
	// the list.
	moved, quit := model.Update("enter")
	if quit || moved.page != overviewPage {
		t.Fatalf("enter changed the split layout: page=%v quit=%v", moved.page, quit)
	}

	// The detail keys still work without drilling in first.
	moved, _ = model.Update("c")
	if moved.page != configPage {
		t.Fatalf("c did not reach Config in the split layout: page=%v", moved.page)
	}
	moved, _ = moved.Update("tab")
	if moved.page != validationPage {
		t.Fatalf("Tab did not advance in the split layout: page=%v", moved.page)
	}
	// One q, not two: there is no overview to return to.
	if _, quit = moved.Update("q"); !quit {
		t.Fatal("q on a split detail page did not quit")
	}
	if _, quit = model.Update("q"); !quit {
		t.Fatal("q on the split layout did not quit")
	}
}

func TestScrollingReachesTheLastLine(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := logFixture(t, 20, 20)

	// The whole point of the issue: the alternate screen has no scrollback, so
	// the final entry has to be reachable from inside the view.
	if strings.Contains(model.View(now), "event 19") {
		t.Fatal("fixture is not tall enough to exercise scrolling")
	}
	bottom, _ := model.Update("G")
	view := bottom.View(now)
	if !strings.Contains(view, "event 19") {
		t.Fatalf("G did not reach the last line:\n%s", view)
	}
	if !strings.Contains(view, "▴") {
		t.Fatalf("indicator does not report content above at the bottom:\n%s", view)
	}
	if strings.Contains(view, "▾") {
		t.Fatalf("indicator still claims content below at the bottom:\n%s", view)
	}
	back, _ := bottom.Update("g")
	if back.offset != 0 {
		t.Fatalf("g did not return to the top: offset=%d", back.offset)
	}
}

func TestScrollKeysStepHalfAScreenAndStopAtTheTop(t *testing.T) {
	model := logFixture(t, 40, 21)
	step := model.scrollStep()
	if step < 1 {
		t.Fatalf("scroll step is %d", step)
	}
	down, _ := model.Update("ctrl+d")
	if down.offset != step {
		t.Fatalf("ctrl+d moved to %d, want %d", down.offset, step)
	}
	paged, _ := down.Update("pgdown")
	if paged.offset != 2*step {
		t.Fatalf("pgdown moved to %d, want %d", paged.offset, 2*step)
	}
	up, _ := paged.Update("ctrl+u")
	if up.offset != step {
		t.Fatalf("ctrl+u moved to %d, want %d", up.offset, step)
	}
	// Scrolling up at the top must not go negative.
	top, _ := up.Update("ctrl+u")
	top, _ = top.Update("ctrl+u")
	if top.offset != 0 {
		t.Fatalf("scrolling past the top reached %d", top.offset)
	}
}

func TestOffsetResetsWhenTheContentBehindItChanges(t *testing.T) {
	model := logFixture(t, 40, 20)
	scrolled, _ := model.Update("ctrl+d")
	if scrolled.offset == 0 {
		t.Fatal("fixture did not scroll")
	}
	// A different page and a different instance are different content, so the
	// viewport starts at the top rather than part way down someone else's page.
	if moved, _ := scrolled.Update("c"); moved.offset != 0 {
		t.Fatalf("changing page kept offset %d", moved.offset)
	}
	// A second instance, so that j has somewhere to move to.
	twoUp := scrolled
	twoUp.instances = append(append([]operator.Instance(nil), scrolled.instances...),
		operator.Instance{ID: "com.pmrrasmussen.symphony.other", Liveness: operator.LivenessStopped})
	if moved, _ := twoUp.Update("j"); moved.selected != 1 || moved.offset != 0 {
		t.Fatalf("changing instance left selected=%d offset=%d", moved.selected, moved.offset)
	}
	// Refreshing in place must not throw away the reader's position.
	held := scrolled
	held.Refresh(held.instances, nil, time.Now())
	if held.offset != scrolled.offset {
		t.Fatalf("refresh reset the offset from %d to %d", scrolled.offset, held.offset)
	}
}

func TestScrollKeysAreInertOnTheOverview(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := styledFixture([]operator.Instance{{ID: "one", Liveness: operator.LivenessRunning}}, now)
	model.height = 20
	for _, key := range []string{"ctrl+d", "ctrl+u", "g", "G", "pgdown", "pgup"} {
		if moved, quit := model.Update(key); moved.offset != 0 || quit {
			t.Fatalf("%s scrolled the overview: offset=%d quit=%v", key, moved.offset, quit)
		}
	}
}
