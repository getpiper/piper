// Package api exposes piperd's HTTP control plane.
package api

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/getpiper/piper/internal/source/github"
	"github.com/getpiper/piper/internal/store"
)

type Deployerer interface {
	Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error)
}

// onGitHubApp, if non-nil, is invoked after a GitHub App is configured via the
// exchange endpoint, so the daemon can start serving webhooks without a restart.
func New(s *store.Store, d Deployerer, baseDomain, githubAPIBase string, onGitHubApp func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if in.Name == "hooks" {
			http.Error(w, "name reserved", http.StatusBadRequest)
			return
		}
		if in.Port == 0 {
			in.Port = 8080
		}
		if _, err := s.GetApp(in.Name); err == nil {
			http.Error(w, "app exists", http.StatusConflict)
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		app, err := s.CreateApp(in.Name, in.Port)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, app)
	})
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		apps, err := s.ListApps()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if apps == nil {
			apps = []store.App{}
		}
		writeJSON(w, http.StatusOK, apps)
	})
	mux.HandleFunc("GET /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetApp(r.PathValue("name"))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, app)
	})
	mux.HandleFunc("POST /v1/apps/{name}/deploy", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := s.GetApp(name); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dir, err := os.MkdirTemp("", "piper-src-*")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(dir)
		if err := untar(r.Body, dir); err != nil {
			http.Error(w, "bad tar: "+err.Error(), http.StatusBadRequest)
			return
		}
		dep, err := d.Deploy(r.Context(), name, dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, dep)
	})
	mux.HandleFunc("POST /v1/apps/{name}/link", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Repo   string `json:"repo"`
			Branch string `json:"branch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Repo == "" || in.Branch == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := s.UpdateAppRepo(r.PathValue("name"), in.Repo, in.Branch); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/github/manifest", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RedirectURL string `json:"redirect_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.RedirectURL == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		manifest, err := github.BuildManifest("piper-"+baseDomain, "https://hooks."+baseDomain, in.RedirectURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"manifest": string(manifest)})
	})
	mux.HandleFunc("POST /v1/github/exchange", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		creds, err := github.ExchangeCode(r.Context(), githubAPIBase, in.Code)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.SaveGitHubApp(store.GitHubApp{
			AppID: creds.AppID, PrivateKey: creds.PrivateKeyPEM, WebhookSecret: creds.WebhookSecret,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if onGitHubApp != nil {
			onGitHubApp()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.Clean(hdr.Name))
		if !isWithin(dir, target) {
			return errors.New("tar entry escapes destination")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

func isWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RequireToken wraps next so every request must carry a valid
// `Authorization: Bearer <token>`. Unknown, malformed, or revoked tokens get 401.
func RequireToken(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if _, err := s.AuthenticateToken(tok); err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return tok, tok != ""
}
