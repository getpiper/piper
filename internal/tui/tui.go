// Package tui is the interactive full-screen frontend of the piper CLI: a
// Bubble Tea program over the same internal/client API the subcommands use.
// Bare `piper` in a terminal lands here; every subcommand stays untouched.
package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/domain"
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
	StartApp(name string) error
	DeleteApp(name string) error
	LinkApp(name, repo, branch, rootDir string) error
	AppDomains(app string) ([]domain.AppDomainStatus, error)
	AddAppDomain(app, dom string) (domain.AppDomainStatus, error)
	RemoveAppDomain(app, dom string) error
	Manifest(redirectURL string) (string, error)
	ExchangeGitHub(code string) (string, error)
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
		app     api.App
		deps    []store.Deployment
		domains []domain.AppDomainStatus
	}
	logsLoadedMsg struct {
		logs   string
		status string
	}

	// domainDetailLoadedMsg is the domain detail view's poll result. found is
	// false when the domain no longer exists; the view keeps its last-known
	// state (the box answered, so it still counts as reachable).
	domainDetailLoadedMsg struct {
		st    domain.AppDomainStatus
		found bool
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

	// boxProbeMsg is one box's reachability probe result, rendered in the boxes
	// view's STATUS column. Each box is probed by its own tea.Cmd, so a dead box
	// resolves after its client timeout without blocking the others.
	boxProbeMsg struct {
		name      string
		reachable bool
	}

	// boxSavedMsg is the box form's success outcome. The root pops back to the
	// boxes view; if the saved box is the current one, it re-dials (its addr or
	// token may have changed) via the same path as a switch. replacing is the
	// box's prior name (empty for an add), used so the root also re-dials when
	// an edit renamed the current box.
	boxSavedMsg struct {
		box       config.Box
		replacing string
	}

	// removeBoxMsg is the remove confirm's intent; the root drops the box.
	removeBoxMsg struct{ name string }

	// boxRemovedMsg is a successful removal. If it changed the current box the
	// root re-dials the promoted one; otherwise it pops back to the boxes view.
	boxRemovedMsg struct {
		current config.Box
		changed bool
	}

	// Action intents: a mutating view emits one of these; the root owns the
	// client, runs the call off the UI thread, and reports the outcome.
	createAppMsg struct {
		name string
		port int
	}
	stopAppMsg   struct{ name string }
	startAppMsg  struct{ name string }
	deleteAppMsg struct{ name string }
	// linkAppMsg is the link form's intent; the root runs LinkApp off the UI
	// thread and reports via actionResultMsg (pop back to app detail on success).
	linkAppMsg struct{ name, repo, branch string }

	// removeDomainMsg is the remove-domain confirm's intent; the root runs
	// RemoveAppDomain and pops back to app detail via actionResultMsg.
	removeDomainMsg struct{ app, domain string }

	// addDomainMsg is the domain form's intent; the root runs AddAppDomain off
	// the UI thread and reports via domainAddedMsg.
	addDomainMsg struct{ app, domain string }

	// domainAddedMsg is the add's outcome. On success the root replaces the
	// form with the domain detail view (CNAME + live status); on error it
	// banners the form.
	domainAddedMsg struct {
		app string
		st  domain.AppDomainStatus
		err error
	}

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

	// githubDoneMsg is the manifest flow's outcome: nil pops back to apps, an
	// error banners in the github view.
	githubDoneMsg struct{ err error }

	// githubFormReadyMsg carries the local form URL once the manifest flow's
	// servers are up, so the running github view can show a manual-open fallback
	// for headless boxes where openBrowser fails. wait is the cmd that blocks on
	// the GitHub callback and finishes the exchange (→ githubDoneMsg).
	githubFormReadyMsg struct {
		url  string
		wait tea.Cmd
	}

	// githubStartMsg is the github view's "run it" intent; the root owns the
	// client, so it launches the manifest flow.
	githubStartMsg struct{ org string }
)

// pollResult is implemented by every message that is the outcome of a view's
// poll, so the root updates reachability without knowing the view type.
type pollResult interface{ reachable() bool }

func (appsLoadedMsg) reachable() bool         { return true }
func (errMsg) reachable() bool                { return false }
func (appDetailLoadedMsg) reachable() bool    { return true }
func (logsLoadedMsg) reachable() bool         { return true }
func (domainDetailLoadedMsg) reachable() bool { return true }
