package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/store"
)

func TestLogsFollowStartsWhileBuilding(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	if !v.follow {
		t.Fatal("follow should start on for a building deployment")
	}
	if v.refresh(fakeAPI{}) == nil {
		t.Fatal("first refresh must fetch even before load")
	}
}

func TestLogsTailAppendAndAutoStop(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	m, _ := v.Update(logsLoadedMsg{logs: "line1\n", status: "building"})
	m, _ = m.Update(logsLoadedMsg{logs: "line1\nline2\n", status: "building"})
	lv := m.(logsView)
	if lv.logs != "line1\nline2\n" {
		t.Fatalf("tail not appended: %q", lv.logs)
	}
	if !lv.follow {
		t.Fatal("should still follow while building")
	}
	// a shorter/equal payload must not shrink the buffer
	m, _ = m.Update(logsLoadedMsg{logs: "line1\n", status: "building"})
	if m.(logsView).logs != "line1\nline2\n" {
		t.Fatalf("buffer shrank on shorter payload: %q", m.(logsView).logs)
	}
	// leaving building auto-stops follow, and a non-following loaded view stops polling
	m, _ = m.Update(logsLoadedMsg{logs: "line1\nline2\ndone\n", status: "running"})
	lv = m.(logsView)
	if lv.follow {
		t.Fatal("follow must auto-stop when the deployment leaves building")
	}
	if lv.refresh(fakeAPI{}) != nil {
		t.Fatal("a loaded, non-following logs view must not poll")
	}
}

func TestLogsFollowAdoptsRotatedEqualLengthTail(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	m, _ := v.Update(logsLoadedMsg{logs: "[log truncated]\nold tail\n", status: "building"})
	m, _ = m.Update(logsLoadedMsg{logs: "[log truncated]\nnew tail\n", status: "building"})

	if got := m.(logsView).logs; got != "[log truncated]\nnew tail\n" {
		t.Fatalf("rotated equal-length tail was not adopted: %q", got)
	}
}

func TestLogsViewShowsContextAndFollowTag(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	m, _ := v.Update(logsLoadedMsg{logs: "hello\n", status: "building"})
	out := m.View()
	if !strings.Contains(out, "blog") || !strings.Contains(out, "dep-12345678") {
		t.Fatalf("missing context header:\n%s", out)
	}
	if !strings.Contains(out, "following") {
		t.Fatalf("missing follow indicator:\n%s", out)
	}
}

func TestLogsFKeyTogglesFollowAndIsNotForwarded(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	// Height 9 → viewport height 3 (chromeHeight 6); 20 lines overflow it.
	m, _ := v.Update(tea.WindowSizeMsg{Width: 80, Height: 9})
	// Load overflowing content; the loaded handler GotoBottoms (YOffset at max).
	m, _ = m.Update(logsLoadedMsg{logs: strings.Repeat("line\n", 20), status: "building"})
	// Scroll up off the bottom so PageDown ('f') would have room to move.
	for i := 0; i < 3; i++ {
		m, _ = m.Update(keyRunes('k'))
	}
	y0 := m.(logsView).vp.YOffset
	before := m.(logsView).follow
	m, _ = m.Update(keyRunes('f'))
	lv := m.(logsView)
	if lv.follow == before {
		t.Fatal("f should toggle follow")
	}
	// If "f" were forwarded to the viewport it would PageDown and YOffset would
	// grow; the early return in Update must consume it so the position holds.
	if lv.vp.YOffset != y0 {
		t.Fatalf("f leaked to viewport PageDown: YOffset moved %d → %d", y0, lv.vp.YOffset)
	}
}

func TestLogsRefreshMatchesDeploymentStatus(t *testing.T) {
	// refresh should report the status of THIS deployment id from Deployments()
	v := newLogsView("blog", "dep-2", "building")
	f := fakeAPI{
		logs: "building…\n",
		deps: []store.Deployment{{ID: "dep-1", Status: "running"}, {ID: "dep-2", Status: "running"}},
	}
	msg := v.refresh(f)()
	lm, ok := msg.(logsLoadedMsg)
	if !ok {
		t.Fatalf("want logsLoadedMsg, got %T", msg)
	}
	if lm.status != "running" {
		t.Fatalf("want status running for dep-2, got %q", lm.status)
	}
}
