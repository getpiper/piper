package main

import (
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/getpiper/piper/internal/client"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/version"
)

var openBrowserFn = openBrowser

func dialClient(stderr io.Writer) (*client.Client, bool) {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, false
	}
	return client.New(cc.Addr, cc.Token), true
}

// login verifies token against the target (GET /v1/apps) and, on success,
// saves it to ~/.piper/piper/config.json.
func login(addr, token string, stdout, stderr io.Writer) int {
	if token == "" {
		fmt.Fprintln(stderr, "usage: piper login --token <token>  (create one with `piperd token create`)")
		return 2
	}
	if addr == "" {
		cc, err := config.LoadClient()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		addr = cc.Addr
	}
	if _, err := client.New(addr, token).ListApps(); err != nil {
		fmt.Fprintln(stderr, "error: token rejected:", err)
		return 1
	}
	if err := config.SaveClient(config.ClientConfig{Addr: addr, Token: token}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "logged in to %s\n", addr)
	return 0
}

func main() {
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr)
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		fs.SetOutput(stderr)
		token := fs.String("token", "", "API token from `piperd token create` (LAN login)")
		addr := fs.String("addr", "", "piperd address (LAN login)")
		relay := fs.String("relay", defaultRelayAPI, "relay control API base URL (device-flow login)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *token != "" {
			return login(*addr, *token, stdout, stderr)
		}
		return relayLogin(*relay, stdout, stderr)
	case "connect":
		fs := flag.NewFlagSet("connect", flag.ContinueOnError)
		fs.SetOutput(stderr)
		def := os.Getenv("PIPER_DATA_DIR")
		if def == "" {
			def = config.DefaultDataDir()
		}
		dataDir := fs.String("data-dir", def, "piperd data directory (where relay.json is written)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		return connect(*dataDir, stdout, stderr)
	case "create":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: piper create <name> [--port N]")
			return 2
		}
		name := args[1]
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		port := fs.Int("port", 8080, "container port the app listens on")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper create <name> [--port N]")
			return 2
		}
		c, ok := dialClient(stderr)
		if !ok {
			return 1
		}
		if err := c.CreateApp(name, *port); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "created app %q (port %d)\n", name, *port)
		return 0
	case "deploy":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: piper deploy <name> [--path DIR]")
			return 2
		}
		name := args[1]
		fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
		fs.SetOutput(stderr)
		path := fs.String("path", ".", "source directory containing a Dockerfile")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper deploy <name> [--path DIR]")
			return 2
		}
		c, ok := dialClient(stderr)
		if !ok {
			return 1
		}
		dep, err := c.Deploy(name, *path)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deployed %s: http://%s.piper.localhost (%s)\n", name, name, dep.Status)
		return 0
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: piper list")
			return 2
		}
		c, ok := dialClient(stderr)
		if !ok {
			return 1
		}
		apps, err := c.ListApps()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, app := range apps {
			fmt.Fprintf(stdout, "%s\tport=%d\n", app.Name, app.Port)
		}
		return 0
	case "app":
		return cmdApp(args[1:], stdout, stderr)
	case "github":
		return cmdGithub(args[1:], stdout, stderr)
	default:
		return usage(stderr)
	}
}

const appLinkUsage = "usage: piper app link <name> --repo owner/name [--branch main]"

func cmdApp(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "link" {
		fmt.Fprintln(stderr, appLinkUsage)
		return 2
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, appLinkUsage)
		return 2
	}
	name := args[1]
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "GitHub repo, owner/name")
	branch := fs.String("branch", "main", "tracked branch")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *repo == "" {
		fmt.Fprintln(stderr, appLinkUsage)
		return 2
	}
	c, ok := dialClient(stderr)
	if !ok {
		return 1
	}
	if err := c.LinkApp(name, *repo, *branch); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "linked %s -> %s (%s)\n", name, *repo, *branch)
	return 0
}

func cmdGithub(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "setup" {
		fmt.Fprintln(stderr, "usage: piper github setup [--org <name>]")
		return 2
	}
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	org := fs.String("org", "", "GitHub organization name")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: piper github setup [--org <name>]")
		return 2
	}
	return githubSetup(*org, stdout, stderr)
}

// githubSetup drives the GitHub App manifest flow: it asks piperd for a manifest,
// serves a tiny auto-submitting form that POSTs it to GitHub, catches the
// redirect ?code=, and exchanges it for App credentials stored on the box.
func githubSetup(org string, stdout, stderr io.Writer) int {
	c, ok := dialClient(stderr)
	if !ok {
		return 1
	}

	codeCh := make(chan string, 1)
	cbLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer cbLn.Close()
	redirect := "http://" + cbLn.Addr().String() + "/cb"

	manifest, err := c.Manifest(redirect)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	cbSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := r.URL.Query().Get("code"); code != "" {
			fmt.Fprintln(w, "Piper GitHub App created. You can close this tab.")
			codeCh <- code
		}
	})}
	go cbSrv.Serve(cbLn)
	defer cbSrv.Close()

	actionURL := "https://github.com/settings/apps/new"
	if org != "" {
		actionURL = fmt.Sprintf("https://github.com/organizations/%s/settings/apps/new", url.PathEscape(org))
	}

	// Auto-submitting form that POSTs the manifest to GitHub.
	page := fmt.Sprintf(`<form id="f" action="%s" method="post">`+
		`<input type="hidden" name="manifest" value='%s'></form><script>document.getElementById('f').submit()</script>`,
		html.EscapeString(actionURL), htmlEscape(manifest))
	formLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	formSrv := &http.Server{Handler: manifestFormHandler(page)}
	go formSrv.Serve(formLn)
	defer formSrv.Close()

	formURL := "http://" + formLn.Addr().String()
	fmt.Fprintf(stdout, "Opening %s — approve the App in your browser...\n", formURL)
	_ = openBrowserFn(formURL)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		fmt.Fprintln(stderr, "error: timed out waiting for GitHub App approval")
		return 1
	}
	if err := c.ExchangeGitHub(code); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, "GitHub App configured. Install it on your repo, then run: piper app link <name> --repo owner/name")
	return 0
}

// manifestFormHandler serves the auto-submitting manifest form. The Content-Type
// is set explicitly: the page starts with <form>, which Go's content sniffer does
// not recognize as HTML, so it would otherwise be served as text/plain and the
// browser would show the source instead of submitting it.
func manifestFormHandler(page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	}
}

func htmlEscape(s string) string { return strings.ReplaceAll(s, "'", "&#39;") }

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: piper <version|login|connect|create|deploy|list|app|github> [args]")
	return 2
}
