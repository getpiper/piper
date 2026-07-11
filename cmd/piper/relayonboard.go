package main

import (
	"errors"
	"fmt"
	"io"
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
	rc := relayclient.New(relayAPI)
	da, err := rc.LoginDevice()
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
		acc, err := rc.LoginPoll(da.DeviceCode)
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
		return 0
	}
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
	en, err := relayclient.New(cc.RelayAPI).Enroll(cc.AccountCredential)
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
		fmt.Fprintf(stdout, "box claimed: %s\n\n", en.BaseDomain)
		fmt.Fprintln(stdout, "piperd runs as a systemd DynamicUser; store the enrollment in its EnvironmentFile:")
		fmt.Fprintf(stdout, "\n    sudo sh -c 'f=%s; \\\n"+
			"      sed -i -E \"/^#?(PIPER_RELAY_ADDR|PIPER_RELAY_TOKEN|PIPER_BASE_DOMAIN|PIPER_RELAY_TERMINATED)=/d\" \"$f\"; \\\n"+
			"      { echo PIPER_RELAY_ADDR=%s; echo PIPER_RELAY_TOKEN=%s; echo PIPER_BASE_DOMAIN=%s; echo PIPER_RELAY_TERMINATED=1; } >> \"$f\"'\n\n",
			config.SystemEnvFile(), en.TunnelEndpoint, en.EnrollmentToken, en.BaseDomain)
		fmt.Fprintln(stdout, "then: sudo systemctl restart piperd")
		return 0
	}

	if err := config.SaveRelayFile(o.dataDir, config.RelayFile{
		RelayAddr:  en.TunnelEndpoint,
		RelayToken: en.EnrollmentToken,
		BaseDomain: en.BaseDomain,
		Terminated: true,
	}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "box claimed: %s\nrestart piperd to connect, e.g.:\n\n    sudo systemctl restart piperd\n", en.BaseDomain)
	return 0
}
