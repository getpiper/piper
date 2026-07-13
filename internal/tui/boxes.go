package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
)

// boxesView is the depth-1 box switcher/editor: a table of the configured boxes
// read fresh from the client config. ↵ connects (switches the active box), a/e
// add/edit via a form, x removes. It is the one view that owns local config
// state rather than piperd state. Relay boxes are listed but not switchable.
type boxesView struct {
	dial    Dialer
	boxes   []config.Box
	current string
	loaded  bool
	cursor  int
	err     error
}

func newBoxesView(dial Dialer) boxesView { return boxesView{dial: dial} }

func (v boxesView) Init() tea.Cmd { return nil }

func (v boxesView) title() string { return "boxes" }

func (v boxesView) footer() string {
	return "↵ connect · a add · e edit · x remove · esc back · ? help"
}

// refresh reloads the client config off the UI thread. (Per-box reachability
// probes are added in a later task.)
func (v boxesView) refresh(API) tea.Cmd {
	return func() tea.Msg {
		cf, err := config.LoadClientFile()
		if err != nil {
			return errMsg{err}
		}
		return boxesLoadedMsg{boxes: cf.Boxes, current: cf.Current}
	}
}

// isRelay reports whether the box at i is relay-backed (not switchable here).
func (v boxesView) isRelay(i int) bool { return v.boxes[i].RelayAPI != "" }

func (v boxesView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case boxesLoadedMsg:
		v.boxes, v.current, v.loaded, v.err = msg.boxes, msg.current, true, nil
		if v.cursor >= len(v.boxes) {
			v.cursor = max(0, len(v.boxes)-1)
		}
	case errMsg:
		v.err = msg.err
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.boxes)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.boxes) > 0 && !v.isRelay(v.cursor) {
				box := v.boxes[v.cursor]
				return v, func() tea.Msg { return switchBoxMsg{box: box} }
			}
		}
	}
	return v, nil
}

func (v boxesView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-16s %-22s %s\n", "NAME", "ADDR", "STATUS")
	for i, box := range v.boxes {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		fmt.Fprintf(&b, "%s%-16s %-22s %s\n", cursor, box.Name, box.Addr, v.status(i))
	}
	return b.String()
}

// status renders the STATUS column for row i: "current" for the active box, "—"
// for relay boxes (not switchable here); reachability probes fill the rest in a
// later task.
func (v boxesView) status(i int) string {
	switch {
	case v.boxes[i].Name == v.current:
		return "current"
	case v.isRelay(i):
		return "—"
	default:
		return ""
	}
}
