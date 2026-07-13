package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// formView is the depth-1 new-app form: a name field and a port field
// (default 8080). On a valid submit it emits createAppMsg; the root runs the
// create and pops back to the apps list. esc cancels (the root pops).
type formView struct {
	name  textinput.Model
	port  textinput.Model
	focus int // 0 = name, 1 = port
	err   error
}

func newFormView() formView {
	name := textinput.New()
	name.Placeholder = "name"
	name.Focus()
	port := textinput.New()
	port.Placeholder = "8080"
	port.SetValue("8080")
	return formView{name: name, port: port}
}

func (v formView) Init() tea.Cmd { return nil }

func (v formView) title() string { return "new app" }

func (v formView) refresh(API) tea.Cmd { return nil }

// capturesText tells the root to hand this view every keystroke (including q
// and r), so the name/port fields receive them instead of the root shortcuts.
func (v formView) capturesText() bool { return true }

func (v *formView) applyFocus() {
	if v.focus == 0 {
		v.name.Focus()
		v.port.Blur()
	} else {
		v.port.Focus()
		v.name.Blur()
	}
}

func (v formView) submit() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(v.name.Value())
	if name == "" {
		v.err = fmt.Errorf("name is required")
		return v, nil
	}
	port, err := strconv.Atoi(strings.TrimSpace(v.port.Value()))
	if err != nil || port < 1 || port > 65535 {
		v.err = fmt.Errorf("port must be a number 1–65535")
		return v, nil
	}
	return v, func() tea.Msg { return createAppMsg{name: name, port: port} }
}

func (v formView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	// Any editing keystroke clears a stale validation banner.
	v.err = nil
	var cmd tea.Cmd
	if v.focus == 0 {
		v.name, cmd = v.name.Update(msg)
	} else {
		v.port, cmd = v.port.Update(msg)
	}
	return v, cmd
}

func (v formView) View() string {
	var b strings.Builder
	b.WriteString("  new app\n\n")
	fmt.Fprintf(&b, "  name  %s\n", v.name.View())
	fmt.Fprintf(&b, "  port  %s\n\n", v.port.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ create   tab switch   esc cancel")
	return b.String()
}
