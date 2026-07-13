package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// confirmMode distinguishes a y/n confirm from a type-the-name confirm.
type confirmMode int

const (
	confirmYesNo confirmMode = iota
	confirmTypeName
)

// confirmView is a modal confirm pushed over the app-detail view: y/n for stop,
// type-the-app-name for delete (matching the CLI's delete guard). On confirm it
// emits the pending intent; "no"/mismatch cancels via the root.
type confirmView struct {
	name   string
	prompt string
	mode   confirmMode
	intent func(string) tea.Msg
	input  textinput.Model
	err    error
}

func newStopConfirm(name string) confirmView {
	return confirmView{
		name:   name,
		prompt: fmt.Sprintf("Stop %s? Its running deployment will be halted.", name),
		mode:   confirmYesNo,
		intent: func(n string) tea.Msg { return stopAppMsg{n} },
	}
}

func newDeleteConfirm(name string) confirmView {
	in := textinput.New()
	in.Placeholder = name
	in.Focus()
	return confirmView{
		name:   name,
		prompt: fmt.Sprintf("Delete %s? This cannot be undone. Type the app name to confirm.", name),
		mode:   confirmTypeName,
		intent: func(n string) tea.Msg { return deleteAppMsg{n} },
		input:  in,
	}
}

func (v confirmView) Init() tea.Cmd { return nil }

func (v confirmView) title() string { return "confirm" }

func (v confirmView) refresh(API) tea.Cmd { return nil }

// capturesText is true only in type-name (delete) mode, where the app name is
// typed into a field; the y/n mode leaves the root's shortcuts active.
func (v confirmView) capturesText() bool { return v.mode == confirmTypeName }

func (v confirmView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		if v.mode == confirmYesNo {
			switch msg.String() {
			case "y":
				return v, func() tea.Msg { return v.intent(v.name) }
			case "n":
				return v, func() tea.Msg { return popMsg{1} }
			}
			return v, nil
		}
		// type-name mode
		if msg.Type == tea.KeyEnter {
			if strings.TrimSpace(v.input.Value()) == v.name {
				return v, func() tea.Msg { return v.intent(v.name) }
			}
			v.err = fmt.Errorf("that doesn't match %q", v.name)
			return v, nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return v, cmd
	}
	return v, nil
}

func (v confirmView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n\n", v.prompt)
	if v.mode == confirmYesNo {
		b.WriteString("  y  yes      n / esc  no")
	} else {
		fmt.Fprintf(&b, "  %s\n\n  ↵ confirm   esc cancel", v.input.View())
	}
	if v.err != nil {
		fmt.Fprintf(&b, "\n\n ⚠ %v", v.err)
	}
	return b.String()
}
