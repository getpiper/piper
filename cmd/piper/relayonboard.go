package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/relayclient"
)

// defaultRelayAPI is the hosted public relay's control API. Override with
// `piper login --relay <url>` for a self-hosted relay.
const defaultRelayAPI = "https://api.public.getpiper.co"

// pollSleep is the device-flow poll delay; a seam so tests don't really sleep.
var pollSleep = time.Sleep

// relayLogin runs the Google device flow against the relay, printing the
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

// connectOpts are the inputs to `piper connect`. In the normal path only
// dataDir is set; installOnly + the relay* fields drive the login-less write
// used by the privileged systemd-run install step (see connect).
type connectOpts struct {
	dataDir     string
	installOnly bool
	relayAddr   string
	relayToken  string
	baseDomain  string
}

// connect claims this box on the relay and installs the enrollment into
// piperd's data dir as relay.json; piperd reads it at startup and dials the
// tunnel (connect never restarts piperd).
//
// Two contexts, split along a permission boundary: `piper login` stores the
// account credential in the invoking user's home, but under the shipped systemd
// unit piperd runs as a DynamicUser whose data dir (/var/lib/piper) the login
// user cannot write. So the normal path enrolls as the user, then — when the
// target is that protected system dir — prints a ready `sudo systemd-run …
// piper connect --install-only …` command that performs the write as the
// DynamicUser (mirroring how `piper-relay enroll` writes the relay's state
// dir). installOnly is that second step: no login, no network, just the write.
func connect(o connectOpts, stdout, stderr io.Writer) int {
	if o.installOnly {
		if o.relayAddr == "" || o.relayToken == "" || o.baseDomain == "" {
			fmt.Fprintln(stderr, "error: --install-only requires --relay-addr, --relay-token and --base-domain")
			return 1
		}
		if err := config.SaveRelayFile(o.dataDir, config.RelayFile{
			RelayAddr: o.relayAddr, RelayToken: o.relayToken, BaseDomain: o.baseDomain,
		}); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "relay.json written to %s\n", o.dataDir)
		return 0
	}

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

	// Protected systemd install: the login user can't write piperd's DynamicUser
	// data dir, so guide the privileged install rather than fail on it. The
	// enrolled values are passed through so no second login/enrollment is needed.
	if o.dataDir == config.SystemDataDir {
		self, err := os.Executable()
		if err != nil || self == "" {
			self = "piper"
		}
		fmt.Fprintf(stdout, "box claimed: %s\n\n", en.BaseDomain)
		fmt.Fprintln(stdout, "piperd runs as a systemd DynamicUser; install the enrollment as it:")
		fmt.Fprintf(stdout, "\n    sudo systemd-run --pipe --wait --collect \\\n"+
			"      --property=DynamicUser=yes --property=StateDirectory=piper \\\n"+
			"      --setenv=PIPER_DATA_DIR=%s \\\n"+
			"      %s connect --install-only \\\n"+
			"      --relay-addr %s --relay-token %s --base-domain %s\n\n",
			o.dataDir, self, en.TunnelEndpoint, en.EnrollmentToken, en.BaseDomain)
		fmt.Fprintln(stdout, "then: sudo systemctl restart piperd")
		return 0
	}

	if err := config.SaveRelayFile(o.dataDir, config.RelayFile{
		RelayAddr:  en.TunnelEndpoint,
		RelayToken: en.EnrollmentToken,
		BaseDomain: en.BaseDomain,
	}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "box claimed: %s\nrestart piperd to connect, e.g.:\n\n    sudo systemctl restart piperd\n", en.BaseDomain)
	return 0
}
