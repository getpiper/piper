package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// linkFormView attaches a git repo to an app: repo (owner/name) and branch
// (default main). On submit it emits linkAppMsg; the root runs LinkApp and pops
// back to app detail on success.
type linkFormView struct {
	app    string
	repo   textinput.Model
	branch textinput.Model
	focus  int // 0 repo, 1 branch
	err    error
}

func newLinkForm(app string) linkFormView {
	repo := textinput.New()
	repo.Placeholder = "owner/name"
	repo.Focus()
	branch := textinput.New()
	branch.Placeholder = "main"
	branch.SetValue("main")
	return linkFormView{app: app, repo: repo, branch: branch}
}

func (v linkFormView) Init() tea.Cmd { return nil }

func (v linkFormView) title() string { return "link" }

func (v linkFormView) refresh(API) tea.Cmd { return nil }

func (v linkFormView) capturesText() bool { return true }

func (v linkFormView) footer() string { return "↵ link · tab switch · esc cancel · ? help" }

func (v *linkFormView) applyFocus() {
	if v.focus == 0 {
		v.repo.Focus()
		v.branch.Blur()
	} else {
		v.branch.Focus()
		v.repo.Blur()
	}
}

func (v linkFormView) submit() (tea.Model, tea.Cmd) {
	repo := strings.TrimSpace(v.repo.Value())
	if repo == "" {
		v.err = fmt.Errorf("repo is required")
		return v, nil
	}
	branch := strings.TrimSpace(v.branch.Value())
	if branch == "" {
		branch = "main"
	}
	name := v.app
	return v, func() tea.Msg { return linkAppMsg{name: name, repo: repo, branch: branch} }
}

func (v linkFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "up", "down":
			v.focus = (v.focus + 1) % 2
			v.applyFocus()
			return v, nil
		case "enter":
			return v.submit()
		}
	}
	v.err = nil // any keystroke clears a stale banner
	var cmd tea.Cmd
	if v.focus == 0 {
		v.repo, cmd = v.repo.Update(msg)
	} else {
		v.branch, cmd = v.branch.Update(msg)
	}
	return v, cmd
}

func (v linkFormView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  link %s to a repo\n\n", v.app)
	fmt.Fprintf(&b, "  repo    %s\n", v.repo.View())
	fmt.Fprintf(&b, "  branch  %s\n\n", v.branch.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ link   tab switch   esc cancel")
	return b.String()
}
