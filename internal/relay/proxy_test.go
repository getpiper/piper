package relay

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/tunnel"
)

// pipeSession builds an in-memory relay↔agent tunnel pair whose relay-side
// session carries base as its BaseDomain.
func pipeSession(t *testing.T, base string) (relaySide, agentSide *tunnel.Session) {
	t.Helper()
	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	srvCh := make(chan *tunnel.Session, 1)
	go func() {
		s, err := tunnel.Serve(sc, func(_, _ string) error { return nil })
		if err == nil {
			srvCh <- s
		}
	}()
	agentSess, err := tunnel.Dial(cc, "tok", base)
	if err != nil {
		t.Fatal(err)
	}
	return <-srvCh, agentSess
}

// fakeBox answers KindControlAPI streams: one HTTP request per stream, echoing
// method, path and Authorization so tests see exactly what the proxy forwarded.
func fakeBox(sess *tunnel.Session) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		if kind != tunnel.KindControlAPI {
			stream.Close()
			continue
		}
		go func() {
			defer stream.Close()
			req, err := http.ReadRequest(bufio.NewReader(stream))
			if err != nil {
				return
			}
			body := req.Method + " " + req.URL.RequestURI() + " auth=" + req.Header.Get("Authorization")
			fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		}()
	}
}

// proxyFixture: alice owns an enrolled agent; mallory is another tenant.
func proxyFixture(t *testing.T) (api http.Handler, st *Store, router *Router, aliceCred, malloryCred, base string) {
	t.Helper()
	st = openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, err := st.UpsertAccount("sub-alice", "alice@x.com")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, _ = st.MintAccountCredential(alice.ID)
	mallory, err := st.UpsertAccount("sub-mallory", "mallory@x.com")
	if err != nil {
		t.Fatal(err)
	}
	malloryCred, _ = st.MintAccountCredential(mallory.ID)
	en, err := st.EnrollForAccount(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	base = en.BaseDomain
	router = NewRouter()
	api = NewAPIWithTunnel(st, NewFakeVerifier(), "", router)
	return
}

func proxyGet(t *testing.T, api http.Handler, path, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestControlProxyAuthz(t *testing.T) {
	api, _, router, aliceCred, malloryCred, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	// No credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	// Unknown credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", "bogus"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad cred: %d, want 401", rr.Code)
	}
	// Another tenant's credential → 404, indistinguishable from unknown agent.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant: %d, want 404", rr.Code)
	}
	// Unknown agent → 404.
	if rr := proxyGet(t, api, "/agents/nope.public.getpiper.co/v1/apps", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d, want 404", rr.Code)
	}
	// Path without /v1/ → 404.
	if rr := proxyGet(t, api, "/agents/"+base+"/secrets", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("non-v1 path: %d, want 404", rr.Code)
	}
}

func TestControlProxyOfflineAgent(t *testing.T) {
	api, _, _, aliceCred, _, base := proxyFixture(t)
	// Agent enrolled but no live session registered.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline agent: %d, want 503", rr.Code)
	}
}

func TestControlProxyForwardsWithTokenB(t *testing.T) {
	api, st, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)
	if err := st.SetControlToken(base, "boxtok"); err != nil {
		t.Fatal(err)
	}

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps?limit=2", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxied: %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "GET /v1/apps?limit=2 ") {
		t.Fatalf("prefix not stripped / query lost: %q", body)
	}
	if !strings.Contains(body, "auth=Bearer boxtok") {
		t.Fatalf("Token B not injected: %q", body)
	}
	if strings.Contains(body, aliceCred) {
		t.Fatalf("account credential leaked to the box: %q", body)
	}
}

func TestControlProxyNoTokenBForwardsBare(t *testing.T) {
	api, _, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxied: %d", rr.Code)
	}
	// Never provisioned: forwarded with NO Authorization (a real box would 401).
	if !strings.Contains(rr.Body.String(), "auth= ") && !strings.HasSuffix(strings.TrimSpace(rr.Body.String()), "auth=") {
		t.Fatalf("expected empty forwarded auth, got %q", rr.Body.String())
	}
}
