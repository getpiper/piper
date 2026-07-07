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
