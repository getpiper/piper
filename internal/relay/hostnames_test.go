package relay

import (
	"path/filepath"
	"strings"
	"testing"
)

func newAccountAgent(t *testing.T) (*Store, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("gh-1", "alice")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatalf("EnrollForAccount: %v", err)
	}
	return st, en.BaseDomain
}

func TestRegisterHostnameIdempotentAndDerived(t *testing.T) {
	st, base := newAccountAgent(t)
	h1, err := st.RegisterHostname(base, "blog")
	if err != nil {
		t.Fatalf("RegisterHostname: %v", err)
	}
	if !strings.HasSuffix(h1, "-alice.public.getpiper.co") {
		t.Fatalf("hostname = %q, want -alice.public.getpiper.co suffix", h1)
	}
	if strings.Count(strings.TrimSuffix(h1, ".public.getpiper.co"), ".") != 0 {
		t.Fatalf("hostname %q is not a single label under the apex", h1)
	}
	h2, err := st.RegisterHostname(base, "blog")
	if err != nil || h2 != h1 {
		t.Fatalf("re-register = %q,%v want %q (idempotent)", h2, err, h1)
	}
	h3, err := st.RegisterHostname(base, "api")
	if err != nil || h3 == h1 {
		t.Fatalf("distinct app got %q,%v (want a different hostname)", h3, err)
	}
}

func TestRegisterHostnameAppCap(t *testing.T) {
	st, base := newAccountAgent(t)
	st.Configure("public.getpiper.co", 3, 2, 5) // cap 2 apps
	if _, err := st.RegisterHostname(base, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "c"); err != ErrQuotaExceeded {
		t.Fatalf("third app err = %v, want ErrQuotaExceeded", err)
	}
	// A re-register of an existing app must not be blocked by the cap.
	if _, err := st.RegisterHostname(base, "a"); err != nil {
		t.Fatalf("re-register under cap: %v", err)
	}
}

func TestRegisterHostnameDisabledAccount(t *testing.T) {
	st, base := newAccountAgent(t)
	if err := st.DisableAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "blog"); err != ErrBadCredential {
		t.Fatalf("disabled-account register err = %v, want ErrBadCredential", err)
	}
}

func TestDeregisterHostname(t *testing.T) {
	st, base := newAccountAgent(t)
	h, err := st.RegisterHostname(base, "blog")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeregisterHostname(base, h); err != nil {
		t.Fatalf("DeregisterHostname: %v", err)
	}
	// Now under cap again and re-registerable.
	if err := st.DeregisterHostname(base, h); err != nil {
		t.Fatalf("deregister missing row should be no-op: %v", err)
	}
}
