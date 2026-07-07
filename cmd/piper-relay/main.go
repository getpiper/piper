package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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

	st.Configure(env("PIPER_RELAY_APEX", "public.getpiper.co"), atoiOr(env("PIPER_RELAY_MAX_AGENTS", "3"), 3))

	tlsAddr := env("PIPER_RELAY_TLS_ADDR", ":443")
	tunnelAddr := env("PIPER_RELAY_TUNNEL_ADDR", ":7000")
	apiAddr := env("PIPER_RELAY_API_ADDR", ":8080")
	tunnelPublic := env("PIPER_RELAY_TUNNEL_PUBLIC", "")

	// Self-service login needs a Google OAuth client; without one the relay runs
	// operator-enroll-only (existing behaviour) and the API 503s login routes.
	var v relay.Verifier
	if id := env("PIPER_RELAY_GOOGLE_CLIENT_ID", ""); id != "" {
		gv, err := relay.NewGoogleVerifier(context.Background(), id, env("PIPER_RELAY_GOOGLE_CLIENT_SECRET", ""))
		if err != nil {
			log.Fatalf("google verifier: %v", err)
		}
		v = gv
	} else {
		log.Print("piper-relay: no PIPER_RELAY_GOOGLE_CLIENT_ID; self-service login disabled")
		v = relay.NewFakeVerifier() // login routes exist but complete only via test approval
	}

	go func() {
		log.Printf("piper-relay: control API %s", apiAddr)
		if err := http.ListenAndServe(apiAddr, relay.NewAPIWithTunnel(st, v, tunnelPublic)); err != nil {
			log.Fatalf("control API: %v", err)
		}
	}()

	log.Printf("piper-relay: TLS %s, tunnel %s", tlsAddr, tunnelAddr)
	log.Fatal(relay.Serve(tlsAddr, tunnelAddr, st))
}
