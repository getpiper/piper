package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// NewAPI returns the account API without a tunnel endpoint, control proxy, or
// web login (tests / LAN). Use NewAPIWithTunnel in production.
func NewAPI(st *Store, v Verifier) http.Handler { return NewAPIWithTunnel(st, v, "", nil, nil) }

// NewAPIWithTunnel is the full account-facing API: device login, browser
// (authorization-code) login, enroll, and — when router is non-nil — the
// /agents/ control proxy (#73). webRedirects is the allowlist of redirect_uri
// prefixes for the browser flow; empty disables web login (503).
func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string, router *Router, webRedirects []string) http.Handler {
	a := &api{st: st, v: v, tunnelEndpoint: tunnelEndpoint,
		webRedirects: webRedirects, webStates: map[string]webState{}}
	if wv, ok := v.(WebVerifier); ok {
		a.webv = wv
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("GET /v1/login/web", a.loginWeb)
	mux.HandleFunc("GET /v1/login/callback", a.loginCallback)
	mux.HandleFunc("POST /v1/enroll", a.enroll)
	if router != nil {
		mux.Handle("/agents/", NewControlProxy(st, router))
	}
	return mux
}

type api struct {
	st             *Store
	v              Verifier
	webv           WebVerifier // nil ⇒ web login disabled
	tunnelEndpoint string
	webRedirects   []string // allowed redirect_uri prefixes; empty ⇒ web login disabled

	mu        sync.Mutex
	webStates map[string]webState // state → pending browser flow
}

// webState is a pending browser login: where to send the credential, and how
// long the state stays redeemable.
type webState struct {
	redirectURI string
	expires     time.Time
}

const stateCookie = "piper_login_state"

// webLoginEnabled gates both browser endpoints: a WebVerifier must be wired
// and at least one redirect prefix allowed.
func (a *api) webLoginEnabled() bool { return a.webv != nil && len(a.webRedirects) > 0 }

func (a *api) redirectAllowed(uri string) bool {
	if uri == "" {
		return false
	}
	for _, p := range a.webRedirects {
		if strings.HasPrefix(uri, p) {
			return true
		}
	}
	return false
}

// loginWeb starts the browser flow: bind a fresh state to the validated
// redirect_uri (server-side map + browser cookie), then hand the browser to
// the IdP.
func (a *api) loginWeb(w http.ResponseWriter, r *http.Request) {
	if !a.webLoginEnabled() {
		http.Error(w, "web login not configured", http.StatusServiceUnavailable)
		return
	}
	ru := r.URL.Query().Get("redirect_uri")
	if !a.redirectAllowed(ru) {
		http.Error(w, "redirect_uri not allowed", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	state := hex.EncodeToString(raw)
	a.mu.Lock()
	a.webStates[state] = webState{redirectURI: ru, expires: time.Now().Add(10 * time.Minute)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, MaxAge: 600, Path: "/v1/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.webv.AuthCodeURL(state), http.StatusFound)
}

// loginCallback finishes the browser flow: state must match both the
// server-side map (single-use, unexpired) and the browser's cookie
// (login-CSRF guard); then code → identity → account → credential, delivered
// in the URL fragment so it never reaches server logs.
func (a *api) loginCallback(w http.ResponseWriter, r *http.Request) {
	if !a.webLoginEnabled() {
		http.Error(w, "web login not configured", http.StatusServiceUnavailable)
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	c, err := r.Cookie(stateCookie)
	if state == "" || code == "" || err != nil || c.Value != state {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	ws, ok := a.webStates[state]
	delete(a.webStates, state) // single use
	a.mu.Unlock()
	if !ok || time.Now().After(ws.expires) {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	id, err := a.webv.Exchange(r.Context(), code)
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Login)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return
	}
	cred, err := a.st.MintAccountCredential(acc.ID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r,
		ws.redirectURI+"#credential="+url.QueryEscape(cred)+"&username="+url.QueryEscape(acc.Username),
		http.StatusFound)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (a *api) loginDevice(w http.ResponseWriter, r *http.Request) {
	handle, d, err := a.v.Start(r.Context())
	if err != nil {
		http.Error(w, "device flow start failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_code":        d.UserCode,
		"verification_uri": d.VerificationURI,
		"device_code":      handle,
		"interval":         d.Interval,
		"expires_in":       d.ExpiresIn,
	})
}

func (a *api) loginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, err := a.v.Poll(r.Context(), req.DeviceCode)
	if errors.Is(err, ErrAuthPending) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "authorization_pending"})
		return
	}
	if err != nil {
		http.Error(w, "unknown or failed device code", http.StatusBadRequest)
		return
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Login)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return
	}
	cred, err := a.st.MintAccountCredential(acc.ID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"account_credential": cred,
		"username":           acc.Username,
	})
}

func (a *api) enroll(w http.ResponseWriter, r *http.Request) {
	cred, ok := bearerToken(r)
	if !ok {
		http.Error(w, "missing bearer credential", http.StatusUnauthorized)
		return
	}
	acc, err := a.st.AuthenticateAccount(cred)
	if errors.Is(err, ErrBadCredential) {
		http.Error(w, "bad credential", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "auth error", http.StatusInternalServerError)
		return
	}
	en, err := a.st.EnrollForAccount(acc.ID)
	if errors.Is(err, ErrQuotaExceeded) {
		http.Error(w, "agent quota exceeded", http.StatusTooManyRequests)
		return
	}
	if err != nil {
		http.Error(w, "enroll error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"enrollment_token": en.Token,
		"base_domain":      en.BaseDomain,
		"tunnel_endpoint":  a.tunnelEndpoint,
	})
}

// bearerToken extracts a "Bearer <tok>" Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || h[:len(p)] != p {
		return "", false
	}
	return h[len(p):], true
}
