package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/domain"
)

const domainsUsage = "usage: piper domains <add <domain> --app <name> | list [--app <name>] | remove <domain> [--app <name>]>"

// cmdDomains drives the per-app custom-domains collection (#232): a thin
// client over /v1/apps/<app>/domains.
func cmdDomains(remote string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, domainsUsage)
		return 2
	}
	switch args[0] {
	case "add":
		usage := "usage: piper domains add <domain> --app <name>"
		if len(args) < 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		dom := args[1]
		fs := flag.NewFlagSet("domains add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		app := fs.String("app", "", "app to attach the domain to")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if *app == "" || fs.NArg() != 0 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		c, ok := dialClient(remote, stderr)
		if !ok {
			return 1
		}
		st, err := c.AddAppDomain(*app, dom)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "attached %s to %s (status: %s)\n", st.Domain, st.App, st.Status)
		fmt.Fprintln(stdout, "create this record at your DNS host:")
		for _, rec := range st.DNSRecords {
			fmt.Fprintf(stdout, "  %s\t%s\t%s\n", rec.Name, rec.Type, rec.Value)
		}
		fmt.Fprintln(stdout, "issuance starts once DNS points at the relay; watch `piper domains list`")
		return 0
	case "list":
		fs := flag.NewFlagSet("domains list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		app := fs.String("app", "", "only this app's domains")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper domains list [--app <name>]")
			return 2
		}
		c, ok := dialClient(remote, stderr)
		if !ok {
			return 1
		}
		ds, err := listDomains(c, *app)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, d := range ds {
			expires := "-"
			if d.CertNotAfter != nil {
				expires = d.CertNotAfter.Format("2006-01-02")
			}
			dns := "no"
			if d.DNSOK {
				dns = "ok"
			}
			fmt.Fprintf(stdout, "%s\tapp=%s\tstatus=%s\tcert_expires=%s\tdns=%s\n", d.Domain, d.App, d.Status, expires, dns)
		}
		return 0
	case "remove":
		usage := "usage: piper domains remove <domain> [--app <name>]"
		if len(args) < 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		dom := args[1]
		fs := flag.NewFlagSet("domains remove", flag.ContinueOnError)
		fs.SetOutput(stderr)
		app := fs.String("app", "", "app the domain is attached to (skips the lookup)")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		c, ok := dialClient(remote, stderr)
		if !ok {
			return 1
		}
		owner := *app
		if owner == "" {
			ds, err := listDomains(c, "")
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
			for _, d := range ds {
				if d.Domain == dom {
					owner = d.App
					break
				}
			}
			if owner == "" {
				fmt.Fprintf(stderr, "error: %s is not attached to any app\n", dom)
				return 1
			}
		}
		if err := c.RemoveAppDomain(owner, dom); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %s from %s\n", dom, owner)
		return 0
	default:
		fmt.Fprintln(stderr, domainsUsage)
		return 2
	}
}

// listDomains returns one app's domains, or every app's when app is empty.
func listDomains(c *client.Client, app string) ([]domain.AppDomainStatus, error) {
	if app != "" {
		return c.AppDomains(app)
	}
	apps, err := c.ListApps()
	if err != nil {
		return nil, err
	}
	var all []domain.AppDomainStatus
	for _, a := range apps {
		ds, err := c.AppDomains(a.Name)
		if err != nil {
			return nil, err
		}
		all = append(all, ds...)
	}
	return all, nil
}
