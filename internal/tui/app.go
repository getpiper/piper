package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const pollInterval = 2 * time.Second

// Model is the root of the TUI: it owns the view stack, the poll tick, the
// active box's client, and the status bar. The top view handles its own
// messages; the root intercepts global keys, navigation, and connectivity.
type Model struct {
	box    string
	addr   string
	remote bool
	client API

	stack         []view
	loaded        bool // at least one successful poll
	down          bool // last poll failed
	width, height int
}

func NewModel(box, addr string, remote bool, c API) Model {
	return Model{box: box, addr: addr, remote: remote, client: c, stack: []view{newAppsView(remote)}}
}

// Run starts the interactive TUI against c, identified as box/addr in the
// status bar. remote marks a relay-backed box (HTTPS URLs). It blocks until quit.
func Run(box, addr string, remote bool, c API) error {
	_, err := tea.NewProgram(NewModel(box, addr, remote, c), tea.WithAltScreen()).Run()
	return err
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) top() view { return m.stack[len(m.stack)-1] }

// refresh polls the top view's data off the UI thread.
func (m Model) refresh() tea.Cmd { return m.top().refresh(m.client) }

// topCapturesText reports whether the top view wants raw keystrokes (a text
// field), so the root suppresses its single-letter shortcuts (q, r) for it.
func (m Model) topCapturesText() bool {
	if c, ok := m.top().(interface{ capturesText() bool }); ok {
		return c.capturesText()
	}
	return false
}

// popN drops n views off the top of the stack without ever removing the root.
func (m Model) popN(n int) Model {
	if n > len(m.stack)-1 {
		n = len(m.stack) - 1
	}
	m.stack = m.stack[:len(m.stack)-n]
	return m
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if len(m.stack) > 1 {
				m.stack = m.stack[:len(m.stack)-1]
				return m, m.refresh()
			}
			return m, nil
		}
		if !m.topCapturesText() {
			switch msg.String() {
			case "q":
				if len(m.stack) == 1 {
					return m, tea.Quit
				}
				m.stack = m.stack[:len(m.stack)-1]
				return m, m.refresh()
			case "r":
				return m, m.refresh()
			}
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case pushMsg:
		m.stack = append(m.stack, msg.view)
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
	case createAppMsg:
		name, port, c := msg.name, msg.port, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.CreateApp(name, port), popLevels: 1} }
	case stopAppMsg:
		name, c := msg.name, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.StopApp(name), popLevels: 1} }
	case deleteAppMsg:
		name, c := msg.name, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.DeleteApp(name), popLevels: 2} }
	case actionResultMsg:
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m = m.popN(msg.popLevels)
		return m, m.refresh()
	case popMsg:
		m = m.popN(msg.n)
		return m, m.refresh()
	case deployMsg:
		name, cwd, c := msg.name, msg.cwd, m.client
		return m, func() tea.Msg {
			dep, err := c.Deploy(name, cwd)
			return deployStartedMsg{app: name, id: dep.ID, err: err}
		}
	case deployStartedMsg:
		if _, ok := m.top().(deployView); !ok {
			return m, nil // user navigated away before the kickoff returned
		}
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m.stack[len(m.stack)-1] = newLogsView(msg.app, msg.id, "building")
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// no return: fall through to forward the size to the top view
	}
	if pr, ok := msg.(pollResult); ok {
		if pr.reachable() {
			m.loaded = true
		}
		m.down = !pr.reachable()
	}
	next, cmd := m.top().Update(msg)
	m.stack[len(m.stack)-1] = next.(view)
	return m, cmd
}

func (m Model) View() string {
	crumbs := make([]string, len(m.stack))
	for i, v := range m.stack {
		crumbs[i] = v.title()
	}
	header := lipgloss.NewStyle().Bold(true).Render(" piper ") + "· " + strings.Join(crumbs, " › ")
	return header + "\n\n" + m.top().View() + "\n\n" + m.statusBar()
}

func (m Model) statusBar() string {
	loc := fmt.Sprintf("%s · %s", m.box, m.addr)
	switch {
	case m.down:
		return fmt.Sprintf(" ○ %s · unreachable — retrying…", loc)
	case !m.loaded:
		return fmt.Sprintf(" … %s · connecting…", loc)
	default:
		return fmt.Sprintf(" ● %s · %s", loc, pluralApps(m.stack[0].(appsView).count()))
	}
}
