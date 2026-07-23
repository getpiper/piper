// Package tui is the interactive full-screen frontend of the piper CLI: a
// Bubble Tea program over the same internal/client API the subcommands use.
// Bare `piper` in a terminal lands here; every subcommand stays untouched.
package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/domain"
	"github.com/getpiper/piper/internal/relayclient"
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

// RelayAPI is the slice of the relay control API the TUI consumes.
// *relayclient.Client satisfies it; tests inject fakes.
type RelayAPI interface {
	CLILoginStart(ctx context.Context) (handle, userCode string, err error)
	CLILoginPoll(ctx context.Context, handle string) (relayclient.Account, error)
	GitHubStatus(ctx context.Context, cred string) (relayclient.Status, error)
	GitHubRepos(ctx context.Context, cred, installationID string) ([]relayclient.Repo, error)
}

// RelayDialer builds a relay client for a base URL. cmd/piper supplies the
// real one; tests inject fakes. A factory, not a client: a fresh user logs in
// against the default relay, a configured user against their saved RelayAPI.
type RelayDialer func(base string) RelayAPI

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

	// wizStatusMsg is the github wizard's config+status probe result. noCred
	// means no account credential is saved (→ login step); base is the relay
	// base the probe used (saved RelayAPI or the default). Deliberately NOT a
	// pollResult: a relay error must not render the box status bar unreachable.
	wizStatusMsg struct {
		noCred bool
		base   string
		cred   string
		st     relayclient.Status
		err    error
	}

	// wizLoginStartedMsg carries the brokered-login handle + the user code the
	// human enters in the browser.
	wizLoginStartedMsg struct {
		handle string
		code   string
		err    error
	}

	// wizLoginDoneMsg is one brokered-login poll outcome. pending means the
	// user hasn't finished in the browser; on success the credential was
	// already saved to client config inside the cmd (off the UI thread).
	wizLoginDoneMsg struct {
		acc     relayclient.Account
		pending bool
		err     error
	}

	// wizReposMsg is one installation's repo listing for the wizard's pushed
	// repos sub-view.
	wizReposMsg struct {
		repos []relayclient.Repo
		err   error
	}
)

// pollResult is implemented by every message that is the outcome of a view's
// poll, so the root updates reachability without knowing the view type.
type pollResult interface{ reachable() bool }

func (appsLoadedMsg) reachable() bool         { return true }
func (errMsg) reachable() bool                { return false }
func (appDetailLoadedMsg) reachable() bool    { return true }
func (logsLoadedMsg) reachable() bool         { return true }
func (domainDetailLoadedMsg) reachable() bool { return true }
