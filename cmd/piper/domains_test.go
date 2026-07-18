package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/domain"
	"github.com/getpiper/piper/internal/store"
)

func TestRunDomainsAddPrintsDNSRecordAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/domains" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Domain != "myshop.com" {
			t.Errorf("body = %+v (err %v)", body, err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(domain.AppDomainStatus{
			Domain: "myshop.com", App: "blog", Status: "pending",
			DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: "myshop.com", Value: "relay.example.net"}},
		})
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "add", "myshop.com", "--app", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "attached myshop.com to blog (status: pending)") {
		t.Errorf("stdout = %q, want the attach line with the initial status", got)
	}
	// The exact record to create at the DNS host: <domain> CNAME <relay-host>.
	if !strings.Contains(got, "myshop.com") || !strings.Contains(got, "CNAME") || !strings.Contains(got, "relay.example.net") {
		t.Errorf("stdout = %q, want the CNAME record to create", got)
	}
	if !strings.Contains(got, "issuance starts") {
		t.Errorf("stdout = %q, want a note that issuance waits on DNS", got)
	}
}

func TestRunDomainsAddSurfacesConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "domain already attached", http.StatusConflict)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "add", "myshop.com", "--app", "blog"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "domain already attached") {
		t.Errorf("stderr = %q, want the API's conflict body", stderr.String())
	}
}

func TestRunDomainsAddRequiresApp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "add", "myshop.com"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: piper domains add") {
		t.Errorf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunDomainsListWithApp(t *testing.T) {
	notAfter := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/blog/domains" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]domain.AppDomainStatus{
			{Domain: "myshop.com", App: "blog", Status: "active", CertNotAfter: &notAfter, DNSOK: true},
			{Domain: "other.dev", App: "blog", Status: "pending"},
		})
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "list", "--app", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "myshop.com\tapp=blog\tstatus=active\tcert_expires=2026-10-15\tdns=ok\n" +
		"other.dev\tapp=blog\tstatus=pending\tcert_expires=-\tdns=no\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunDomainsListAggregatesAllApps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{
				{App: store.App{Name: "blog"}}, {App: store.App{Name: "shop"}},
			})
		case "/v1/apps/blog/domains":
			_ = json.NewEncoder(w).Encode([]domain.AppDomainStatus{})
		case "/v1/apps/shop/domains":
			_ = json.NewEncoder(w).Encode([]domain.AppDomainStatus{
				{Domain: "myshop.com", App: "shop", Status: "issuing", DNSOK: true},
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "myshop.com\tapp=shop\tstatus=issuing\tcert_expires=-\tdns=ok\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunDomainsRemoveWithAppSkipsLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/blog/domains/myshop.com" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "remove", "myshop.com", "--app", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed myshop.com from blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDomainsRemoveResolvesOwningApp(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{
				{App: store.App{Name: "blog"}}, {App: store.App{Name: "shop"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/apps/blog/domains":
			_ = json.NewEncoder(w).Encode([]domain.AppDomainStatus{})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/apps/shop/domains":
			_ = json.NewEncoder(w).Encode([]domain.AppDomainStatus{
				{Domain: "myshop.com", App: "shop", Status: "active"},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/apps/shop/domains/myshop.com":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "remove", "myshop.com"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !deleted {
		t.Fatal("DELETE was not sent to the owning app's collection")
	}
	if !strings.Contains(stdout.String(), "removed myshop.com from shop") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDomainsRemoveUnattachedDomainErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{{App: store.App{Name: "blog"}}})
		case "/v1/apps/blog/domains":
			_ = json.NewEncoder(w).Encode([]domain.AppDomainStatus{})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains", "remove", "ghost.com"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not attached to any app") {
		t.Errorf("stderr = %q, want a clear not-attached error", stderr.String())
	}
}

func TestRunDomainsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"domains"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: piper domains") {
		t.Errorf("stderr = %q, want usage", stderr.String())
	}
}
