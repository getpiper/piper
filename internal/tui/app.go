package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const pollInterval = 2 * time.Second

// Model is the root of the TUI: it owns the view stack, the poll tick, the
// active box's client, and the status bar. Child views handle their own
// messages; the root intercepts global keys and connectivity state.
type Model struct {
	box    string
	addr   string
	client API

	stack    []tea.Model
	loaded   bool // at least one successful poll
	down     bool // last poll failed
	appCount int
}

func NewModel(box, addr string, c API) Model {
	return Model{box: box, addr: addr, client: c, stack: []tea.Model{newAppsView()}}
}

// Run starts the interactive TUI against c, identified as box/addr in the
// status bar. It blocks until the user quits.
func Run(box, addr string, c API) error {
	_, err := tea.NewProgram(NewModel(box, addr, c), tea.WithAltScreen()).Run()
	return err
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// refresh polls the current view's data off the UI thread.
func (m Model) refresh() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		apps, err := c.ListApps()
		if err != nil {
			return errMsg{err}
		}
		return appsLoadedMsg{apps}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if len(m.stack) == 1 {
				return m, tea.Quit
			}
			m.stack = m.stack[:len(m.stack)-1]
			return m, nil
		case "esc":
			if len(m.stack) > 1 {
				m.stack = m.stack[:len(m.stack)-1]
			}
			return m, nil
		case "r":
			return m, m.refresh()
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case appsLoadedMsg:
		m.loaded, m.down, m.appCount = true, false, len(msg.apps)
	case errMsg:
		m.down = true
	}
	top, cmd := m.stack[len(m.stack)-1].Update(msg)
	m.stack[len(m.stack)-1] = top
	return m, cmd
}

func (m Model) View() string {
	header := lipgloss.NewStyle().Bold(true).Render(" piper ") + "· apps"
	return header + "\n\n" + m.stack[len(m.stack)-1].View() + "\n\n" + m.statusBar()
}

func (m Model) statusBar() string {
	switch {
	case m.down:
		return fmt.Sprintf(" ○ %s · %s · unreachable — retrying…", m.box, m.addr)
	case !m.loaded:
		return fmt.Sprintf(" … %s · %s · connecting…", m.box, m.addr)
	default:
		return fmt.Sprintf(" ● %s · %s · %d apps", m.box, m.addr, m.appCount)
	}
}
