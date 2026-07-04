package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/caddy"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/deploy"
	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/store"
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
		mgr, err := caddy.StartManager(ctx, cfg.CaddyAdmin, ":80")
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
		defer mgr.Stop()
	}

	dep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)
	handler := api.New(st, dep)

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
