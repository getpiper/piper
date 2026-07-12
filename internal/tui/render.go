package tui

import "fmt"

// appURL renders the URL a box serves an app on from its stored hostname. A
// relay-terminated box serves over HTTPS; a local/BYO box over HTTP. Empty
// hostname (never deployed) yields "".
func appURL(hostname string, remote bool) string {
	if hostname == "" {
		return ""
	}
	if remote {
		return "https://" + hostname
	}
	return "http://" + hostname
}

// pluralApps renders an app count for the status bar ("1 app", "3 apps").
func pluralApps(n int) string {
	if n == 1 {
		return "1 app"
	}
	return fmt.Sprintf("%d apps", n)
}

// statusIcon maps a deployment status to its one-glyph indicator; "" (never
// deployed) and unknown values render as "—".
func statusIcon(status string) string {
	switch status {
	case "running":
		return "●"
	case "building":
		return "◐"
	case "failed":
		return "✗"
	case "stopped":
		return "○"
	}
	return "—"
}
