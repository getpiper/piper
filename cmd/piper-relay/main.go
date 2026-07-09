package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/getpiper/piper/internal/relay"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// adminStore is the slice of *relay.Store the admin subcommands need.
type adminStore interface {
	DisableAccount(username string) error
}

// apiAddrIsLoopback reports whether addr binds only the loopback interface.
// A bare ":8080" or "0.0.0.0:8080" binds all interfaces; "127.0.0.1:8080" /
// "[::1]:8080" / "localhost:8080" are loopback-only.
func apiAddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present; treat the whole string as the host.
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// runAdmin handles "piper-relay admin <cmd> ...". Currently: disable <username>.
func runAdmin(st adminStore, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: piper-relay admin disable <username>")
	}
	switch args[0] {
	case "disable":
		if len(args) != 2 {
			return fmt.Errorf("usage: piper-relay admin disable <username>")
		}
		return st.DisableAccount(args[1])
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func main() {
	dataDir := env("PIPER_RELAY_DATA_DIR", "./relay-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	st, err := relay.Open(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := runAdmin(st, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		fs := flag.NewFlagSet("enroll", flag.ExitOnError)
		domain := fs.String("domain", "", "base domain the agent may serve (e.g. alice.example.com)")
		// The flag package stops parsing at the first non-flag argument, so a
		// leading positional <name> before --domain would otherwise never be
		// recognized. Peel it off before handing the rest to fs.Parse.
		args := os.Args[2:]
		var name string
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			name = args[0]
			args = args[1:]
		}
		fs.Parse(args)
		if name == "" || *domain == "" {
			log.Fatal("usage: piper-relay enroll <name> --domain <base-domain>")
		}
		tok, err := st.Enroll(name, *domain)
		if err != nil {
			log.Fatalf("enroll: %v", err)
		}
		fmt.Printf("enrolled %s for %s\ntoken: %s\n", name, *domain, tok)
		return
	}

	st.Configure(
		env("PIPER_RELAY_APEX", "public.getpiper.co"),
		atoiOr(env("PIPER_RELAY_MAX_AGENTS", "3"), 3),
		atoiOr(env("PIPER_RELAY_MAX_APPS", "10"), 10),
	)

	tlsAddr := env("PIPER_RELAY_TLS_ADDR", ":443")
	tunnelAddr := env("PIPER_RELAY_TUNNEL_ADDR", ":7000")
	apiAddr := env("PIPER_RELAY_API_ADDR", ":8080")
	tunnelPublic := env("PIPER_RELAY_TUNNEL_PUBLIC", "")

	// Self-service login needs a GitHub OAuth app; without one the relay runs
	// operator-enroll-only (existing behaviour) and login completes only via
	// test approval.
	var v relay.Verifier
	if id := env("PIPER_RELAY_GITHUB_CLIENT_ID", ""); id != "" {
		v = relay.NewGitHubVerifier(id, env("PIPER_RELAY_GITHUB_CLIENT_SECRET", ""))
	} else if env("PIPER_RELAY_FAKE_APPROVE", "") == "1" {
		log.Print("piper-relay: PIPER_RELAY_FAKE_APPROVE=1 — device login auto-approves (TEST ONLY)")
		v = relay.NewAutoApproveVerifier("e2e-sub", "e2e")
	} else {
		log.Print("piper-relay: no PIPER_RELAY_GITHUB_CLIENT_ID; self-service login disabled")
		v = relay.NewFakeVerifier() // login routes exist but complete only via test approval
	}

	if !apiAddrIsLoopback(apiAddr) {
		log.Printf("piper-relay: WARNING control API %s is not loopback-only; it serves bearer credentials in cleartext HTTP and must be fronted with TLS", apiAddr)
	}

	// Browser (dashboard) login: allowed redirect_uri prefixes, comma-separated.
	// Empty — or a missing client secret — leaves web login disabled (503).
	var webRedirects []string
	for _, p := range strings.Split(env("PIPER_RELAY_WEB_REDIRECTS", ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			webRedirects = append(webRedirects, p)
		}
	}
	if len(webRedirects) > 0 && env("PIPER_RELAY_GITHUB_CLIENT_SECRET", "") == "" {
		log.Print("piper-relay: PIPER_RELAY_WEB_REDIRECTS set but no PIPER_RELAY_GITHUB_CLIENT_SECRET; web login disabled")
		webRedirects = nil
	}

	router := relay.NewRouter()
	apiHandler := relay.NewAPIWithTunnel(st, v, tunnelPublic, router, webRedirects)

	go func() {
		log.Printf("piper-relay: control API %s", apiAddr)
		if err := http.ListenAndServe(apiAddr, apiHandler); err != nil {
			log.Fatalf("control API: %v", err)
		}
	}()

	tlsCfg, err := relay.LoadWildcardConfig(env("PIPER_RELAY_TLS_CERT", ""), env("PIPER_RELAY_TLS_KEY", ""))
	if err != nil {
		log.Fatalf("wildcard cert: %v", err)
	}
	if tlsCfg == nil {
		log.Print("piper-relay: no wildcard cert (PIPER_RELAY_TLS_CERT/KEY); passthrough-only, shared-domain termination disabled")
	}

	log.Printf("piper-relay: TLS %s, tunnel %s", tlsAddr, tunnelAddr)
	log.Fatal(relay.Serve(tlsAddr, tunnelAddr, st, tlsCfg, router, apiHandler))
}
