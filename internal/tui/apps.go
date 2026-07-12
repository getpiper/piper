package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
)

// appsView is the depth-0 home view: a read-only table of apps. Selection
// and drill-down arrive in phase 3.
type appsView struct {
	apps   []api.App
	err    error
	loaded bool
}

func newAppsView() appsView { return appsView{} }

func (v appsView) Init() tea.Cmd { return nil }

func (v appsView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case appsLoadedMsg:
		v.apps, v.err, v.loaded = msg.apps, nil, true
	case errMsg:
		v.err = msg.err
	}
	return v, nil
}

func (v appsView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if len(v.apps) == 0 {
		b.WriteString(" no apps yet — create one with `piper create <name>`")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-16s %-12s %s\n", "NAME", "STATUS", "URL")
	for _, a := range v.apps {
		status := strings.TrimSpace(statusIcon(a.Status) + " " + a.Status)
		fmt.Fprintf(&b, "  %-16s %-12s %s\n", a.Name, status, appURL(a.Hostname))
	}
	return b.String()
}
