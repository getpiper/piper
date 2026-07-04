package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/getpiper/piper/internal/relay"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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

	tlsAddr := env("PIPER_RELAY_TLS_ADDR", ":443")
	tunnelAddr := env("PIPER_RELAY_TUNNEL_ADDR", ":7000")
	log.Printf("piper-relay: TLS %s, tunnel %s", tlsAddr, tunnelAddr)
	log.Fatal(relay.Serve(tlsAddr, tunnelAddr, st))
}
