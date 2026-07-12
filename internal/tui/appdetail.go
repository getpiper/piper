package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

// appDetailView is the depth-1 view: an app header plus its deployment history.
// It re-polls every tick while on top, so a deploy started elsewhere surfaces
// live. Read-only; actions arrive in phase 4.
type appDetailView struct {
	name   string
	remote bool
	app    api.App
	deps   []store.Deployment
	cursor int
	loaded bool
	err    error
}

func newAppDetailView(name string, remote bool) appDetailView {
	return appDetailView{name: name, remote: remote}
}

func (v appDetailView) Init() tea.Cmd { return nil }

func (v appDetailView) title() string { return v.name }

func (v appDetailView) refresh(c API) tea.Cmd {
	name := v.name
	return func() tea.Msg {
		app, err := c.App(name)
		if err != nil {
			return errMsg{err}
		}
		deps, err := c.Deployments(name)
		if err != nil {
			return errMsg{err}
		}
		return appDetailLoadedMsg{app: app, deps: deps}
	}
}

func (v appDetailView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case appDetailLoadedMsg:
		v.app, v.deps, v.loaded, v.err = msg.app, msg.deps, true, nil
		if v.cursor >= len(v.deps) {
			v.cursor = max(0, len(v.deps)-1)
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
			if v.cursor < len(v.deps)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.deps) > 0 {
				d := v.deps[v.cursor]
				return v, func() tea.Msg { return pushMsg{newLogsView(v.name, d.ID, d.Status)} }
			}
		}
	}
	return v, nil
}

func (v appDetailView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	url := appURL(v.app.Hostname, v.remote)
	if url == "" {
		url = "—"
	}
	fmt.Fprintf(&b, "  %s   %s   :%d   %s\n\n", v.name, url, v.app.Port, repoLine(v.app))
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if len(v.deps) == 0 {
		b.WriteString(" no deployments yet")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-14s %-12s %-10s %s\n", "DEPLOYMENT", "STATUS", "CREATED", "PR")
	for i, d := range v.deps {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		status := strings.TrimSpace(statusIcon(d.Status) + " " + d.Status)
		pr := ""
		if d.PR > 0 {
			pr = fmt.Sprintf("#%d", d.PR)
		}
		fmt.Fprintf(&b, "%s%-14s %-12s %-10s %s\n", cursor, shortID(d.ID), status, relTime(d.CreatedAt), pr)
	}
	return b.String()
}

// TEMP stub replaced by logs.go in the next task.
func newLogsView(app, id, status string) logsView { return logsView{app: app, id: id, status: status} }

type logsView struct{ app, id, status string }

func (logsView) Init() tea.Cmd                         { return nil }
func (v logsView) Update(tea.Msg) (tea.Model, tea.Cmd) { return v, nil }
func (v logsView) View() string                        { return "" }
func (logsView) title() string                         { return "logs" }
func (v logsView) refresh(API) tea.Cmd                 { return nil }
