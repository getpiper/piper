package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/getpiper/piper/internal/agent"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/caddy"
	"github.com/getpiper/piper/internal/certs"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/deploy"
	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/source/github"
	"github.com/getpiper/piper/internal/store"
	"github.com/getpiper/piper/internal/webhook"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
)

func main() {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "piper.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Unless PIPER_SKIP_CADDY is set (e.g. a caddy is already running), manage one.
	if os.Getenv("PIPER_SKIP_CADDY") == "" {
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" {
			opts = append(opts, caddy.WithHTTPS(":443"))
		}
		mgr, err := caddy.StartManager(ctx, cfg.CaddyAdmin, ":80", opts...)
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
		defer mgr.Stop()
	}

	// Relay mode: obtain/serve TLS on :443, dial the relay, register the base domain.
	if cfg.RelayAddr != "" {
		if err := setupRelayTLS(ctx, cfg); err != nil {
			log.Fatalf("relay tls: %v", err)
		}
		go agent.RunTunnelClient(ctx, cfg.RelayAddr, cfg.RelayToken, cfg.BaseDomain,
			func() (net.Conn, error) { return net.Dial("tcp", "127.0.0.1:443") })

		if gh, err := st.GetGitHubApp(); err == nil {
			prov, err := github.New(github.Config{
				AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
			})
			if err != nil {
				log.Fatalf("github provider: %v", err)
			}
			wdep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)
			wh := webhook.New(prov, st, wdep, cfg.BaseDomain)
			whSrv := &http.Server{Addr: cfg.WebhookAddr, Handler: wh}
			go func() {
				if err := whSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Printf("webhook serve: %v", err)
				}
			}()
			_, portStr, _ := net.SplitHostPort(cfg.WebhookAddr)
			port, _ := strconv.Atoi(portStr)
			if err := caddy.NewClient(cfg.CaddyAdmin).UpsertRoute("hooks."+cfg.BaseDomain, port); err != nil {
				log.Printf("webhook route: %v", err)
			}
			log.Printf("webhook listening on %s (GitHub App %d)", cfg.WebhookAddr, gh.AppID)
		} else {
			log.Printf("no GitHub App configured; run `piper github setup` to enable git deploys")
		}
	}

	dep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)
	handler := api.New(st, dep, cfg.BaseDomain, "")

	srv := &http.Server{Addr: cfg.APIAddr, Handler: handler}
	go func() {
		log.Printf("piperd listening on %s (apps at *.%s)", cfg.APIAddr, cfg.BaseDomain)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	srv.Shutdown(context.Background())
}

// setupRelayTLS loads a wildcard cert into Caddy: a static PEM if configured
// (tests / BYO), otherwise ACME DNS-01 via lego.
func setupRelayTLS(ctx context.Context, cfg config.Config) error {
	cc := caddy.NewClient(cfg.CaddyAdmin)
	if cfg.TLSCertFile != "" {
		certPEM, err := os.ReadFile(cfg.TLSCertFile)
		if err != nil {
			return err
		}
		keyPEM, err := os.ReadFile(cfg.TLSKeyFile)
		if err != nil {
			return err
		}
		return cc.LoadCert(string(certPEM), string(keyPEM))
	}
	provider, err := newDNSProvider(cfg.DNSProvider)
	if err != nil {
		return err
	}
	acctKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mgr, err := certs.New(certs.Config{
		Email: cfg.ACMEEmail, CADirURL: cfg.ACMECA,
		DNSProvider: provider, AccountKey: acctKey,
	})
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := mgr.Obtain([]string{"*." + cfg.BaseDomain, cfg.BaseDomain})
	if err != nil {
		return err
	}
	go renewLoop(ctx, mgr, cc, cfg.BaseDomain, certPEM)
	return cc.LoadCert(string(certPEM), string(keyPEM))
}

func newDNSProvider(name string) (challenge.Provider, error) {
	switch name {
	case "", "cloudflare":
		return cloudflare.NewDNSProvider()
	default:
		return nil, fmt.Errorf("unsupported DNS provider %q", name)
	}
}

type certificateManager interface {
	Obtain([]string) ([]byte, []byte, error)
}

type certificateReplacer interface {
	ReplaceCert(certPEM, keyPEM string) error
}

// renewLoop re-obtains and reloads the cert when it nears expiry.
func renewLoop(ctx context.Context, mgr certificateManager, cc certificateReplacer, base string, certPEM []byte) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	runRenewLoop(ctx, mgr, cc, base, certPEM, ticker.C, time.Now)
}

func runRenewLoop(ctx context.Context, mgr certificateManager, cc certificateReplacer, base string, certPEM []byte, ticks <-chan time.Time, now func() time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			due, err := certs.NeedsRenewal(certPEM, 30*24*time.Hour, now())
			if err != nil || !due {
				continue
			}
			newCert, newKey, err := mgr.Obtain([]string{"*." + base, base})
			if err != nil {
				log.Printf("renew: %v", err)
				continue
			}
			if err := cc.ReplaceCert(string(newCert), string(newKey)); err != nil {
				log.Printf("renew load: %v", err)
				continue
			}
			certPEM = newCert
		}
	}
}
