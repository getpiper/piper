// Package version exposes the build version of Piper binaries.
package version

// value is overridable at build time via -ldflags "-X ...version.value=...".
var value = "0.0.0-dev"

// String returns the current build version.
func String() string { return value }
