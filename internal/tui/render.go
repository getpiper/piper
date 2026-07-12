package tui

// appURL renders the URL a local/BYO box serves an app on. Empty hostname
// (never deployed) yields "". Relay-terminated HTTPS rendering arrives with
// the boxes work (phase 5).
func appURL(hostname string) string {
	if hostname == "" {
		return ""
	}
	return "http://" + hostname
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
