package tui

import "errors"

// isUnauthorized reports whether err (or an error it wraps) is an HTTP-401
// rejection. It matches on behaviour, not type, so the tui classifies a
// *client.StatusError without importing internal/client (preserving the
// API/Dialer seam the earlier phases established).
func isUnauthorized(err error) bool {
	var u interface{ Unauthorized() bool }
	return errors.As(err, &u) && u.Unauthorized()
}
