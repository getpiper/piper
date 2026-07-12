// Package tui is the interactive full-screen frontend of the piper CLI: a
// Bubble Tea program over the same internal/client API the subcommands use.
// Bare `piper` in a terminal lands here; every subcommand stays untouched.
package tui

import "github.com/getpiper/piper/internal/api"

// API is the slice of the piperd control API the TUI consumes.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	ListApps() ([]api.App, error)
}

// Messages flowing into Update. All API calls run as tea.Cmd goroutines and
// land as exactly one of these; the UI thread never blocks.
type (
	appsLoadedMsg struct{ apps []api.App }
	errMsg        struct{ err error }
	tickMsg       struct{}
)
