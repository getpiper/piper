package tui

import tea "github.com/charmbracelet/bubbletea"

// helpView is a static, pushed overlay listing the full keymap. It holds no
// state and polls nothing; the root's esc/q pop dismisses it.
type helpView struct{}

func (helpView) Init() tea.Cmd { return nil }

func (helpView) title() string { return "help" }

func (helpView) refresh(API) tea.Cmd { return nil }

func (v helpView) Update(tea.Msg) (tea.Model, tea.Cmd) { return v, nil }

func (helpView) View() string {
	return "  Global      esc back/cancel · q quit (root) / back · r refresh · ctrl+c quit\n" +
		"  Apps list   ↑/k ↓/j move · enter open · n new app\n" +
		"  App detail  ↑/k ↓/j move · enter logs · d deploy · s stop · x delete\n" +
		"  Logs        f toggle follow · esc back\n\n" +
		"  esc back"
}
