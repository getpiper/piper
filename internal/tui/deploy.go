package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// deployView is the depth-2 confirm gate before shipping the launch directory
// to an app. On "y" it emits deployMsg; the root kicks off Deploy (which
// returns fast with a building deployment) and replaces this view with a
// follow logs view on that build. It has no poll of its own.
type deployView struct {
	name       string
	cwd        string
	dockerfile bool
	shipping   bool
	err        error
}

func newDeployView(name, cwd string, dockerfile bool) deployView {
	return deployView{name: name, cwd: cwd, dockerfile: dockerfile}
}

func (v deployView) Init() tea.Cmd { return nil }

func (v deployView) title() string { return "deploy" }

func (v deployView) refresh(API) tea.Cmd { return nil }

func (v deployView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err, v.shipping = msg.err, false
		return v, nil
	case tea.KeyMsg:
		if msg.String() == "y" && !v.shipping {
			v.shipping = true
			name, cwd := v.name, v.cwd
			return v, func() tea.Msg { return deployMsg{name: name, cwd: cwd} }
		}
	}
	return v, nil
}

func (v deployView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  deploy %s\n\n", v.name)
	fmt.Fprintf(&b, "  source:     %s\n", v.cwd)
	dockerfile := "not found ✗"
	if v.dockerfile {
		dockerfile = "found ✓"
	}
	fmt.Fprintf(&b, "  Dockerfile: %s\n\n", dockerfile)
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if v.shipping {
		b.WriteString("  shipping…")
		return b.String()
	}
	b.WriteString("  y  ship it     esc  cancel")
	return b.String()
}
