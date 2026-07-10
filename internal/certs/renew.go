package certs

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"
)

// NeedsRenewal reports whether the leaf certificate in certPEM expires within
// the given window as measured from now.
func NeedsRenewal(certPEM []byte, within time.Duration, now time.Time) (bool, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false, fmt.Errorf("no PEM block in cert")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, fmt.Errorf("parse cert: %w", err)
	}
	return now.Add(within).After(crt.NotAfter), nil
}

// NotAfter returns the leaf certificate's expiry.
func NotAfter(certPEM []byte) (time.Time, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return time.Time{}, fmt.Errorf("no PEM block in cert")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cert: %w", err)
	}
	return crt.NotAfter, nil
}
