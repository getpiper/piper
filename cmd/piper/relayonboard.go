package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/relayclient"
)

// defaultRelayAPI is the hosted public relay's control API. Override with
// `piper login --relay <url>` for a self-hosted relay.
const defaultRelayAPI = "https://api.public.getpiper.dev"

// pollSleep is the device-flow poll delay; a seam so tests don't really sleep.
var pollSleep = time.Sleep

// relayLogin runs the GitHub device flow against the relay, printing the
// verification URL + user code, polling to completion, and storing the returned
// account credential (and relay API base) in the CLI config.
func relayLogin(relayAPI string, stdout, stderr io.Writer) int {
	// Interrupt-aware: Ctrl-C during the poll loop cancels the in-flight
	// request instead of waiting out the 30s client timeout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rc := relayclient.New(relayAPI)
	da, err := rc.LoginDevice(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "To log in, open:\n\n    %s\n\nand enter the code: %s\n\n", da.VerificationURI, da.UserCode)
	_ = openBrowserFn(da.VerificationURI)

	interval := da.Interval
	if interval <= 0 {
		interval = 5
	}
	expires := da.ExpiresIn
	if expires <= 0 {
		expires = 300
	}
	deadline := time.Now().Add(time.Duration(expires) * time.Second)
	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "error: login timed out; run `piper login` again")
			return 1
		}
		pollSleep(time.Duration(interval) * time.Second)
		acc, err := rc.LoginPoll(ctx, da.DeviceCode)
		if errors.Is(err, relayclient.ErrAuthPending) {
			continue
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc, err := config.LoadClient()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc.RelayAPI = relayAPI
		cc.AccountCredential = acc.AccountCredential
		if err := config.SaveClient(cc); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "logged in to relay as %s\n", acc.Username)
		if acc.InstallURL != "" {
			if err := waitForInstall(rc, acc.AccountCredential, acc.InstallURL); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
		}
		return 0
	}
}

// relayLoginWeb runs the one-trip brokered browser login (#291): the relay mints
// a handle + user code, the user enters the code in the browser and authorizes,
// and — for a first-timer — the same browser session is bounced to the install
// page. The box holds no loopback listener; it only polls the handle. Unlike the
// device flow, this ends with the install already underway, so a first-timer's
// login and install are one browser trip.
func relayLoginWeb(relayAPI string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rc := relayclient.New(relayAPI)
	handle, code, err := rc.CLILoginStart(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	verifyURL := strings.TrimRight(relayAPI, "/") + "/v1/login/cli"
	fmt.Fprintf(stdout, "To sign in, open:\n\n    %s\n\nand enter the code: %s\n\n", verifyURL, code)
	_ = openBrowserFn(verifyURL)

	deadline := time.Now().Add(10 * time.Minute)
	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "error: login timed out; run `piper login --web` again")
			return 1
		}
		pollSleep(2 * time.Second)
		acc, err := rc.CLILoginPoll(ctx, handle)
		if errors.Is(err, relayclient.ErrAuthPending) {
			continue
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc, err := config.LoadClient()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc.RelayAPI = relayAPI
		cc.AccountCredential = acc.AccountCredential
		if err := config.SaveClient(cc); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "logged in to relay as %s\n", acc.Username)
		if acc.InstallURL != "" {
			if err := waitForInstall(rc, acc.AccountCredential, acc.InstallURL); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
		}
		return 0
	}
}

// waitForInstall polls the relay until the account's GitHub App installation is
// on record. Both login flows end here when the account has no installation yet:
// the device flow (which cannot install) and `login --web` (where the browser
// was already bounced to the install page). It is how the CLI learns the user
// finished installing — in another tab, or on another device for a headless box.
func waitForInstall(rc *relayclient.Client, cred, installURL string) error {
	fmt.Printf("Install the Piper GitHub App on the repos you want to deploy:\n  %s\n\nWaiting…", installURL)
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		insts, err := rc.GitHubStatus(context.Background(), cred)
		if err != nil {
			return err
		}
		if len(insts) > 0 {
			n := 0
			for _, in := range insts {
				// Best-effort repo count for the message; a transient error here
				// must not fail a login whose install already succeeded.
				if repos, err := rc.GitHubRepos(context.Background(), cred, in.ID); err == nil {
					n += len(repos)
				}
			}
			fmt.Printf("\rInstalled — %d repo(s) available.\n", n)
			return nil
		}
		fmt.Print(".")
		pollSleep(3 * time.Second)
	}
	return errors.New("timed out waiting for the GitHub App install")
}

// githubRepos lists the repositories the logged-in account's GitHub App
// installations can reach, read live from the relay across every installation.
func githubRepos(stdout, stderr io.Writer) int {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cc.RelayAPI == "" || cc.AccountCredential == "" {
		fmt.Fprintln(stderr, "error: not logged in to a relay; run `piper login` first")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rc := relayclient.New(cc.RelayAPI)
	insts, err := rc.GitHubStatus(ctx, cc.AccountCredential)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len(insts) == 0 {
		fmt.Fprintln(stdout, "No repositories yet — run `piper login` to install the Piper GitHub App on the repos you want to deploy.")
		return 0
	}
	for _, in := range insts {
		repos, err := rc.GitHubRepos(ctx, cc.AccountCredential, in.ID)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, r := range repos {
			if r.Visibility != "" && r.Visibility != "public" {
				fmt.Fprintf(stdout, "%s (%s)\n", r.FullName, r.Visibility)
				continue
			}
			fmt.Fprintln(stdout, r.FullName)
		}
	}
	return 0
}

// connectOpts are the inputs to `piper connect`. dataDir is where relay.json is
// written on a non-systemd (dev / per-user) install.
type connectOpts struct {
	dataDir string
}

// connect claims this box on the relay and installs the enrollment so piperd
// picks it up at startup (connect never restarts piperd).
//
// On a dev / per-user install the login user owns the data dir, so relay.json is
// written directly. On the shipped systemd install piperd runs as a DynamicUser
// whose StateDirectory the login user can't write; there the enrollment belongs
// in the root-owned EnvironmentFile /etc/piper/piperd.env, which systemd injects
// into the service at start — so connect prints a plain-sudo upsert of the three
// relay keys instead (the file may already hold ACME/DNS settings, so it must
// not be clobbered).
//
// Run off-box — no piperd install of any flavor on the machine — connect fails
// loudly instead of writing a relay.json nothing will read (#173).
func connect(o connectOpts, stdout, stderr io.Writer) int {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cc.RelayAPI == "" || cc.AccountCredential == "" {
		fmt.Fprintln(stderr, "error: not logged in to a relay; run `piper login` first")
		return 1
	}
	// Fail loudly off-box before enrolling: the enrollment would land in a
	// relay.json no piperd reads here, and the claim would still burn an
	// account quota slot (#173).
	if !config.SystemManaged() && !agentInstalled(o.dataDir) {
		fmt.Fprintln(stderr, "error: no piperd installation found on this machine — `piper connect` must be run on the box where piperd is installed")
		fmt.Fprintf(stderr, "(no systemd install, rootless user unit, launchd agent, or existing data dir %s found)\n", o.dataDir)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	en, err := relayclient.New(cc.RelayAPI).Enroll(ctx, cc.AccountCredential)
	switch {
	case errors.Is(err, relayclient.ErrBadCredential):
		fmt.Fprintln(stderr, "error: relay rejected your account credential; run `piper login` again")
		return 1
	case errors.Is(err, relayclient.ErrQuotaExceeded):
		fmt.Fprintln(stderr, "error: account agent quota exceeded; remove an existing box or upgrade")
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	// Systemd install: the login user can't write piperd's DynamicUser data dir,
	// but /etc/piper/piperd.env is a root-owned EnvironmentFile systemd injects at
	// start. Guide a plain-sudo upsert of the three relay keys (delete any prior
	// or commented copies, then append) rather than clobbering admin edits.
	if config.SystemManaged() {
		fmt.Fprintf(stdout, "box claimed: %s\n", en.BaseDomain)
		fmt.Fprintln(stdout, "\npiperd runs as a systemd DynamicUser; store the enrollment in its EnvironmentFile.")
		fmt.Fprintln(stdout, "\nNext step:")
		githubBrokered := 0
		if en.GitHubApp {
			githubBrokered = 1
		}
		fmt.Fprintf(stdout, "\n    sudo sh -c 'f=%s; \\\n"+
			"      sed -i -E \"/^#?(PIPER_RELAY_ADDR|PIPER_RELAY_TOKEN|PIPER_BASE_DOMAIN|PIPER_RELAY_TERMINATED|PIPER_WEBHOOK_SECRET|PIPER_GITHUB_BROKERED)=/d\" \"$f\"; \\\n"+
			"      { echo PIPER_RELAY_ADDR=%s; echo PIPER_RELAY_TOKEN=%s; echo PIPER_BASE_DOMAIN=%s; echo PIPER_RELAY_TERMINATED=1; echo PIPER_WEBHOOK_SECRET=%s; echo PIPER_GITHUB_BROKERED=%d; } >> \"$f\"'\n",
			config.SystemEnvFile(), en.TunnelEndpoint, en.EnrollmentToken, en.BaseDomain, en.WebhookSecret, githubBrokered)
		fmt.Fprintln(stdout, "\nthen: sudo systemctl restart piperd")
		return 0
	}

	if err := config.SaveRelayFile(o.dataDir, config.RelayFile{
		RelayAddr:      en.TunnelEndpoint,
		RelayToken:     en.EnrollmentToken,
		BaseDomain:     en.BaseDomain,
		Terminated:     true,
		WebhookSecret:  en.WebhookSecret,
		GitHubBrokered: en.GitHubApp,
	}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "box claimed: %s\n%s", en.BaseDomain, restartHint())
	return 0
}

// restartHint returns the "restart piperd to connect" line for the relay.json
// (non-systemd) branch of connect, choosing the restart command that matches how
// piperd is actually managed on this box. The system-wide systemd install never
// reaches here — it returns earlier on the config.SystemManaged() branch with its
// own `sudo systemctl restart piperd` guidance — so the only install flavors this
// branch can see are the rootless systemd user unit, the macOS launchd agent, or a
// bare data dir whose manager we can't identify. A wrong example (the old
// hardcoded `sudo systemctl restart piperd`) is worse than none, so the bare-data-
// dir fallback prints the plain instruction with no command (#248).
func restartHint() string {
	if unit, err := userUnitPath(); err == nil {
		if _, err := os.Stat(unit); err == nil {
			return "restart piperd to connect, e.g.:\n\n    systemctl --user restart piperd\n"
		}
	}
	if plist, err := launchdPlistPath(); err == nil {
		if _, err := os.Stat(plist); err == nil {
			// Match agent.go's gui/$UID/<label> target rendering (guiTarget()).
			return "restart piperd to connect, e.g.:\n\n    launchctl kickstart -k " + guiTarget() + "/" + launchdLabel + "\n"
		}
	}
	return "restart piperd to connect\n"
}

// agentInstalled reports whether any piperd install is detectable on this
// machine for the relay.json path: the data dir already exists (piperd has run
// here), a rootless systemd user unit is installed, or a macOS launchd agent
// is. connect uses it to fail loudly off-box (#173).
func agentInstalled(dataDir string) bool {
	if fi, err := os.Stat(dataDir); err == nil && fi.IsDir() {
		return true
	}
	if unit, err := userUnitPath(); err == nil {
		if _, err := os.Stat(unit); err == nil {
			return true
		}
	}
	if plist, err := launchdPlistPath(); err == nil {
		if _, err := os.Stat(plist); err == nil {
			return true
		}
	}
	return false
}
