package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
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
	"github.com/getpiper/piper/internal/tunnel"
	"github.com/getpiper/piper/internal/webhook"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
)

const (
	drainTimeout    = 15 * time.Second
	shutdownTimeout = 20 * time.Second
)

type apiShutdowner interface {
	Shutdown(context.Context) error
	Close() error
}

type webhookLifecycle interface {
	stop(context.Context)
	close()
	wait(context.Context) bool
	cancel()
}

type listenerStopper interface{ Stop() }
type storeCloser interface{ Close() error }

type tokenStore interface {
	CreateToken(label, scope string) (string, error)
	ListTokens() ([]store.Token, error)
	RevokeToken(label string) error
}

// relayTokenStore is the store slice relay-control provisioning needs.
type relayTokenStore interface {
	ListTokens() ([]store.Token, error)
	CreateToken(label, scope string) (string, error)
	DeleteToken(label string) error
}

// provisionRelayControl mints a control-API token for the relay and pushes it
// over the tunnel, once per enrollment (agent-push Token B — see the
// control-stream routing design). The token row itself is the marker: any row
// labeled relay:<base>, live OR revoked, means "already provisioned" or "the
// owner cut the relay off" — never re-mint. A new `piper connect` creates a new
// enrollment (new base domain) and so a fresh mint. If the push fails, the
// just-minted row is deleted so the next connect retries.
func provisionRelayControl(st relayTokenStore, push func(string) error, baseDomain string) {
	label := "relay:" + baseDomain
	toks, err := st.ListTokens()
	if err != nil {
		log.Printf("relay control provision: list tokens: %v", err)
		return
	}
	for _, tk := range toks {
		if tk.Label == label {
			return
		}
	}
	tok, err := st.CreateToken(label, "admin")
	if err != nil {
		log.Printf("relay control provision: mint: %v", err)
		return
	}
	if err := push(tok); err != nil {
		log.Printf("relay control provision: push: %v (will retry next connect)", err)
		_ = st.DeleteToken(label)
		return
	}
	log.Printf("relay control provision: pushed control bearer for %s", baseDomain)
}

// runTokenCmd implements `piperd token <create|list|revoke>`, writing directly
// to the on-box store. It needs no auth: running it is proof of box ownership.
func runTokenCmd(st tokenStore, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: piperd token <create|list|revoke>")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		name := fs.String("name", "", "label for the token")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("token create: --name is required")
		}
		tok, err := st.CreateToken(*name, "admin")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, tok)
		return nil
	case "list":
		toks, err := st.ListTokens()
		if err != nil {
			return err
		}
		for _, tk := range toks {
			status := "active"
			if tk.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\n", tk.Label, tk.Scope, status)
		}
		return nil
	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: piperd token revoke <name>")
		}
		return st.RevokeToken(args[1])
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		dataDir := config.Load().DataDir
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			log.Fatalf("data dir: %v", err)
		}
		st, err := store.Open(filepath.Join(dataDir, "piper.db"))
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		defer st.Close()
		if err := runTokenCmd(st, os.Args[2:], os.Stdout); err != nil {
			log.Fatalf("token: %v", err)
		}
		return
	}

	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "piper.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Unless PIPER_SKIP_CADDY is set (e.g. a caddy is already running), manage one.
	var mgr *caddy.Manager
	if os.Getenv("PIPER_SKIP_CADDY") == "" {
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" && !cfg.Terminated {
			opts = append(opts, caddy.WithHTTPS(":443"))
		}
		mgr, err = caddy.StartManager(cfg.CaddyAdmin, ":80", opts...)
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
	}

	dep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)

	// Relay mode: dial the relay and forward its streams. Terminated (free-tier)
	// mode holds no box cert and serves apps on :80; the relay terminates TLS and
	// opens KindHTTP streams. Non-terminated (BYO-domain) mode obtains a wildcard
	// cert, serves :443, and answers KindPassthrough streams.
	var wh *webhookStarter
	if cfg.RelayAddr != "" {
		var dialLocal func(kind byte) (net.Conn, error)
		if cfg.Terminated {
			dialLocal = func(kind byte) (net.Conn, error) {
				switch kind {
				case tunnel.KindControlAPI:
					return net.Dial("tcp", cfg.APIAddr)
				case tunnel.KindHTTP:
					return net.Dial("tcp", "127.0.0.1:80")
				default:
					return net.Dial("tcp", "127.0.0.1:443")
				}
			}
		} else {
			if err := setupRelayTLS(ctx, cfg); err != nil {
				log.Fatalf("relay tls: %v", err)
			}
			dialLocal = func(kind byte) (net.Conn, error) {
				if kind == tunnel.KindControlAPI {
					return net.Dial("tcp", cfg.APIAddr)
				}
				return net.Dial("tcp", "127.0.0.1:443")
			}
		}
		tc := &agent.TunnelClient{}
		tc.OnConnect = func() { provisionRelayControl(st, tc.Provision, cfg.BaseDomain) }
		go tc.Run(ctx, cfg.RelayAddr, cfg.RelayToken, cfg.BaseDomain, dialLocal)
		if cfg.Terminated {
			dep.SetHostnameRegistrar(tc)
		}

		wh = newWebhookStarter(cfg, st, rt)
		if _, err := st.GetGitHubApp(); err == nil {
			wh.start()
		} else {
			log.Printf("no GitHub App configured; run `piper github setup` to enable git deploys")
		}
	}

	// After `piper github setup` stores App creds at runtime, start serving
	// webhooks immediately (relay mode only) instead of waiting for a restart.
	handler := api.RequireToken(st, api.New(st, dep, cfg.BaseDomain, "", func() {
		if wh != nil {
			wh.start()
		}
	}, nil))

	srv := &http.Server{Addr: cfg.APIAddr, Handler: handler}
	go func() {
		log.Printf("piperd listening on %s (apps at *.%s)", cfg.APIAddr, cfg.BaseDomain)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	var mgrStop listenerStopper
	if mgr != nil {
		mgrStop = mgr
	}
	var whLifecycle webhookLifecycle
	if wh != nil {
		whLifecycle = wh
	}
	shutdown(srv, whLifecycle, mgrStop, st)
	os.Exit(0)
}

func shutdown(api apiShutdowner, wh webhookLifecycle, mgr listenerStopper, st storeCloser) {
	shutdownWithTimeouts(api, wh, mgr, st, drainTimeout, shutdownTimeout)
}

func shutdownWithTimeouts(api apiShutdowner, wh webhookLifecycle, mgr listenerStopper, st storeCloser, drain, overall time.Duration) {
	overallCtx, cancelOverall := context.WithTimeout(context.Background(), overall)
	defer cancelOverall()
	drainCtx, cancelDrain := context.WithTimeout(overallCtx, drain)
	defer cancelDrain()

	var calls sync.WaitGroup
	if api != nil {
		calls.Add(1)
		go func() { defer calls.Done(); _ = api.Shutdown(drainCtx) }()
	}
	if wh != nil {
		calls.Add(1)
		go func() { defer calls.Done(); wh.stop(drainCtx) }()
	}
	entryDone := make(chan struct{})
	go func() { calls.Wait(); close(entryDone) }()

	entryDrained := false
	select {
	case <-entryDone:
		entryDrained = true
	case <-drainCtx.Done():
	}

	workDrained := entryDrained
	if wh != nil && entryDrained {
		workDrained = wh.wait(drainCtx)
	}
	if !workDrained {
		if api != nil {
			_ = api.Close()
		}
		if wh != nil {
			wh.close()
		}
	}
	if wh != nil {
		wh.cancel()
		if !workDrained {
			_ = wh.wait(overallCtx)
		}
	}
	if !workDrained {
		// API handlers are cancelled by Close but are not tracked separately.
		// Keep shared infrastructure alive for their reserved cleanup window.
		<-overallCtx.Done()
	}
	if mgr != nil {
		mgr.Stop()
	}
	if st != nil {
		_ = st.Close()
	}
}

// webhookStarter brings up the webhook listener and its Caddy route exactly
// once, from stored GitHub App creds. start() is safe to call both at boot (if
// creds already exist) and later from the exchange endpoint.
type webhookStarter struct {
	cfg     config.Config
	st      *store.Store
	rt      *runtime.DockerRuntime
	once    sync.Once
	srv     *http.Server
	handler *webhook.Handler
}

func newWebhookStarter(cfg config.Config, st *store.Store, rt *runtime.DockerRuntime) *webhookStarter {
	return &webhookStarter{cfg: cfg, st: st, rt: rt}
}

func (w *webhookStarter) start() { w.once.Do(w.run) }

func (w *webhookStarter) run() {
	gh, err := w.st.GetGitHubApp()
	if err != nil {
		log.Printf("webhook: no GitHub App configured: %v", err)
		return
	}
	prov, err := github.New(github.Config{
		AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
	})
	if err != nil {
		log.Printf("webhook: github provider: %v", err)
		return
	}
	wdep := deploy.New(w.st, w.rt, caddy.NewClient(w.cfg.CaddyAdmin), w.cfg.BaseDomain)
	w.handler = webhook.New(prov, w.st, wdep, w.cfg.BaseDomain)
	w.srv = &http.Server{Addr: w.cfg.WebhookAddr, Handler: w.handler}
	go func() {
		if err := w.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("webhook serve: %v", err)
		}
	}()
	_, portStr, _ := net.SplitHostPort(w.cfg.WebhookAddr)
	port, _ := strconv.Atoi(portStr)
	if err := caddy.NewClient(w.cfg.CaddyAdmin).UpsertRoute("hooks."+w.cfg.BaseDomain, port); err != nil {
		log.Printf("webhook route: %v", err)
	}
	log.Printf("webhook listening on %s (GitHub App %d)", w.cfg.WebhookAddr, gh.AppID)
}

func (w *webhookStarter) stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.once.Do(func() {})
	if w.handler != nil {
		w.handler.StopAccepting()
	}
	if w.srv != nil {
		_ = w.srv.Shutdown(ctx)
	}
}

func (w *webhookStarter) close() {
	if w != nil && w.srv != nil {
		_ = w.srv.Close()
	}
}

func (w *webhookStarter) wait(ctx context.Context) bool {
	return w == nil || w.handler == nil || w.handler.WaitContext(ctx)
}

func (w *webhookStarter) cancel() {
	if w != nil && w.handler != nil {
		w.handler.Cancel()
	}
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
