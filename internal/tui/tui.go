// Package tui is the interactive full-screen frontend of the piper CLI: a
// Bubble Tea program over the same internal/client API the subcommands use.
// Bare `piper` in a terminal lands here; every subcommand stays untouched.
package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
)

// API is the slice of the piperd control API the TUI consumes.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	ListApps() ([]api.App, error)
}

// view is a stack entry: a Bubble Tea model that refreshes its own data off the
// UI thread and names itself for the breadcrumb. The root owns the stack; a
// view never mutates it — it requests navigation with a pushMsg.
type view interface {
	tea.Model
	refresh(API) tea.Cmd
	title() string
}

// Messages flowing into Update. All API calls run as tea.Cmd goroutines and
// land as exactly one of these; the UI thread never blocks.
type (
	appsLoadedMsg struct{ apps []api.App }
	errMsg        struct{ err error }
	tickMsg       struct{}
	pushMsg       struct{ view view }
)

// pollResult is implemented by every message that is the outcome of a view's
// poll, so the root updates reachability without knowing the view type.
type pollResult interface{ reachable() bool }

func (appsLoadedMsg) reachable() bool { return true }
func (errMsg) reachable() bool        { return false }
