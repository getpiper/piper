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

	stack  []view
	loaded bool // at least one successful poll
	down   bool // last poll failed
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
			return m, m.refresh()
		case "esc":
			if len(m.stack) > 1 {
				m.stack = m.stack[:len(m.stack)-1]
				return m, m.refresh()
			}
			return m, nil
		case "r":
			return m, m.refresh()
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case pushMsg:
		m.stack = append(m.stack, msg.view)
		return m, m.refresh()
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
