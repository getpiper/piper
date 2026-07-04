package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/getpiper/piper/internal/client"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/version"
)

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
		if err := client.New(config.ClientAddr()).CreateApp(name, *port); err != nil {
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
		dep, err := client.New(config.ClientAddr()).Deploy(name, *path)
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
		apps, err := client.New(config.ClientAddr()).ListApps()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, app := range apps {
			fmt.Fprintf(stdout, "%s\tport=%d\n", app.Name, app.Port)
		}
		return 0
	default:
		return usage(stderr)
	}
}

func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: piper <version|create|deploy|list> [args]")
	return 2
}
