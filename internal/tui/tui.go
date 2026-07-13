// Package tui is the interactive full-screen frontend of the piper CLI: a
// Bubble Tea program over the same internal/client API the subcommands use.
// Bare `piper` in a terminal lands here; every subcommand stays untouched.
package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/store"
)

// API is the slice of the piperd control API the TUI consumes.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	ListApps() ([]api.App, error)
	App(name string) (api.App, error)
	Deployments(name string) ([]store.Deployment, error)
	DeploymentLogs(name, id string) (string, error)
	CreateApp(name string, port int) error
	Deploy(name, srcDir string) (store.Deployment, error)
	StopApp(name string) error
	DeleteApp(name string) error
}

// Dialer builds a client for a saved box. cmd/piper supplies the real one
// (LAN path); tests inject a fake. addr identifies the box in the status bar;
// remote marks a relay-backed box (HTTPS app URLs).
type Dialer func(config.Box) (c API, addr string, remote bool, err error)

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
	appsLoadedMsg      struct{ apps []api.App }
	errMsg             struct{ err error }
	tickMsg            struct{}
	pushMsg            struct{ view view }
	appDetailLoadedMsg struct {
		app  api.App
		deps []store.Deployment
	}
	logsLoadedMsg struct {
		logs   string
		status string
	}

	// boxesLoadedMsg carries the client config the boxes view renders. It is a
	// local-config load, not a piperd poll, so it does not implement pollResult
	// (the status bar keeps its last-known reachability while browsing boxes).
	boxesLoadedMsg struct {
		boxes   []config.Box
		current string
	}

	// switchBoxMsg is the boxes view's connect intent; the root dials the box,
	// swaps the active client, and resets the stack to a fresh apps view.
	switchBoxMsg struct{ box config.Box }

	// Action intents: a mutating view emits one of these; the root owns the
	// client, runs the call off the UI thread, and reports the outcome.
	createAppMsg struct {
		name string
		port int
	}
	stopAppMsg   struct{ name string }
	deleteAppMsg struct{ name string }

	// actionResultMsg is a mutating action's outcome. On success the root pops
	// popLevels views and refreshes; on error it banners the top overlay.
	actionResultMsg struct {
		err       error
		popLevels int
	}

	// popMsg pops n views off the stack (e.g. a y/n confirm answered "no").
	popMsg struct{ n int }

	// deployMsg is the deploy confirm's intent; the root kicks off Deploy.
	deployMsg struct {
		name string
		cwd  string
	}

	// deployStartedMsg is the deploy kickoff's outcome. On success the root
	// replaces the deploy confirm with a follow logs view on the new build.
	deployStartedMsg struct {
		app string
		id  string
		err error
	}
)

// pollResult is implemented by every message that is the outcome of a view's
// poll, so the root updates reachability without knowing the view type.
type pollResult interface{ reachable() bool }

func (appsLoadedMsg) reachable() bool      { return true }
func (errMsg) reachable() bool             { return false }
func (appDetailLoadedMsg) reachable() bool { return true }
func (logsLoadedMsg) reachable() bool      { return true }
