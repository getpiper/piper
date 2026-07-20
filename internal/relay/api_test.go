package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestAPI(t *testing.T) (http.Handler, *Store, *FakeVerifier) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	return NewAPI(st, fv), st, fv
}

func TestLoginDeviceThenPoll(t *testing.T) {
	api, _, fv := newTestAPI(t)

	// Start device flow.
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/device", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("device status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dev struct {
		UserCode   string `json:"user_code"`
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dev); err != nil {
		t.Fatal(err)
	}
	if dev.UserCode == "" || dev.DeviceCode == "" {
		t.Fatalf("empty device response: %+v", dev)
	}

	// Poll before approval → 202 pending.
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("pending poll status = %d, want 202", rr.Code)
	}

	// Approve, then poll → 200 with a credential.
	fv.Approve(dev.DeviceCode, Identity{Subject: "sub-1", Login: "ivan"})
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("success poll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var ok struct {
		AccountCredential string `json:"account_credential"`
		Username          string `json:"username"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.AccountCredential == "" || ok.Username != "ivan" {
		t.Fatalf("poll success body = %+v", ok)
	}
}

func TestLoginPollUnknownHandle(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"nope"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown-handle poll status = %d, want 400", rr.Code)
	}
}

func TestEnrollWithAccountCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000", nil, nil, nil)

	acc, _ := st.UpsertAccount("sub-1", "judy")
	cred, _ := st.MintAccountCredential(acc.ID)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		EnrollmentToken string `json:"enrollment_token"`
		BaseDomain      string `json:"base_domain"`
		TunnelEndpoint  string `json:"tunnel_endpoint"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.EnrollmentToken == "" {
		t.Fatal("empty enrollment token")
	}
	if !strings.HasSuffix(out.BaseDomain, "-judy.public.getpiper.co") {
		t.Fatalf("base domain = %q", out.BaseDomain)
	}
	if out.TunnelEndpoint != "relay.getpiper.co:7000" {
		t.Fatalf("tunnel endpoint = %q", out.TunnelEndpoint)
	}
}

func TestEnrollReturnsWebhookSecretAndAppFlag(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000", nil, nil, app)

	acc, _ := st.UpsertAccount("sub-1", "judy")
	cred, _ := st.MintAccountCredential(acc.ID)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		WebhookSecret string `json:"webhook_secret"`
		GitHubApp     bool   `json:"github_app"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WebhookSecret == "" {
		t.Fatal("enroll returned no webhook_secret")
	}
	if !out.GitHubApp {
		t.Fatal("github_app flag not advertised despite a configured App")
	}
}

func TestEnrollRejectsBadCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000", nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-cred enroll status = %d, want 401", rr.Code)
	}
}

func TestEnrollOverCapReturns429(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 1, 10, 5)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000", nil, nil, nil)
	acc, _ := st.UpsertAccount("sub-1", "ken")
	cred, _ := st.MintAccountCredential(acc.ID)

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
		req.Header.Set("Authorization", "Bearer "+cred)
		rr := httptest.NewRecorder()
		api.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(); c != http.StatusOK {
		t.Fatalf("first enroll = %d, want 200", c)
	}
	if c := do(); c != http.StatusTooManyRequests {
		t.Fatalf("over-cap enroll = %d, want 429", c)
	}
}

// startWebLogin drives GET /v1/login/web and returns the minted state and the
// state cookie. The FakeVerifier's AuthCodeURL embeds the state, so it's
// recoverable from the redirect Location.
func startWebLogin(t *testing.T, api http.Handler, redirectURI string) (state string, cookie *http.Cookie) {
	t.Helper()
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape(redirectURI), nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("web login status = %d, body = %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	state = loc.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in redirect %q", loc)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "piper_login_state" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no piper_login_state cookie set")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags = %+v, want HttpOnly Secure SameSite=Lax", cookie)
	}
	return state, cookie
}

func newWebTestAPI(t *testing.T) (http.Handler, *FakeVerifier) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	api := NewAPIWithTunnel(st, fv, "", nil, []string{"https://dash.getpiper.co/"}, nil)
	return api, fv
}

func TestWebLoginCallbackHappyPath(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")

	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body = %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != "https://dash.getpiper.co/auth" {
		t.Fatalf("redirect target = %q", got)
	}
	frag, err := url.ParseQuery(loc.Fragment)
	if err != nil {
		t.Fatalf("bad fragment %q: %v", loc.Fragment, err)
	}
	if frag.Get("credential") == "" || frag.Get("username") != "ivan" {
		t.Fatalf("fragment = %q", loc.Fragment)
	}
	var stateCookieOut *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie {
			stateCookieOut = c
		}
	}
	if stateCookieOut == nil {
		t.Fatal("callback response did not clear the state cookie")
	}
	if stateCookieOut.MaxAge >= 0 {
		t.Fatalf("state cookie MaxAge = %d, want < 0 (expired)", stateCookieOut.MaxAge)
	}
}

func TestWebLoginRejectsDisallowedRedirect(t *testing.T) {
	api, _ := newWebTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://evil.example/auth"), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("disallowed redirect status = %d, want 400", rr.Code)
	}
}

func TestWebLoginRejectsFragmentRedirect(t *testing.T) {
	api, _ := newWebTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://dash.getpiper.co/#x"), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("fragment redirect status = %d, want 400", rr.Code)
	}
}

func TestWebLoginCallbackStateSingleUse(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})

	do := func() int {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		api.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(); c != http.StatusFound {
		t.Fatalf("first callback = %d, want 302", c)
	}
	if c := do(); c != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", c)
	}
}

func TestWebLoginCallbackRejectsCookieMismatch(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, _ := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})

	// No cookie at all.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("cookieless callback = %d, want 400", rr.Code)
	}

	// Wrong cookie value.
	req = httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(&http.Cookie{Name: "piper_login_state", Value: "someone-else"})
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("wrong-cookie callback = %d, want 400", rr.Code)
	}
}

func TestWebLoginCallbackExchangeFailure(t *testing.T) {
	api, _ := newWebTestAPI(t) // no GrantCode → Exchange fails
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=bad&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("failed-exchange callback = %d, want 502", rr.Code)
	}
}

func TestWebLoginSweepsExpiredStates(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	a := &api{st: st, v: NewFakeVerifier(), webv: NewFakeVerifier(),
		webRedirects: []string{"https://dash.getpiper.co/"}, webStates: map[string]webState{}}
	a.webStates["stale"] = webState{redirectURI: "https://dash.getpiper.co/x", expires: time.Now().Add(-time.Minute)}

	rr := httptest.NewRecorder()
	a.loginWeb(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://dash.getpiper.co/auth"), nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("web login status = %d", rr.Code)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.webStates["stale"]; ok {
		t.Fatal("expired state not swept on new login")
	}
	if len(a.webStates) != 1 {
		t.Fatalf("webStates size = %d, want 1 (only the fresh state)", len(a.webStates))
	}
}

func TestWebLoginNotConfigured(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	api := NewAPI(st, NewFakeVerifier()) // no webRedirects

	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://dash.getpiper.co/auth"), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured web login = %d, want 503", rr.Code)
	}
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=x&state=y", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured callback = %d, want 503", rr.Code)
	}
}
