package relay

import (
	"encoding/json"
	"errors"
	"net/http"
)

// NewAPI returns the account API without a tunnel endpoint or control proxy
// (tests / LAN). Use NewAPIWithTunnel in production.
func NewAPI(st *Store, v Verifier) http.Handler { return NewAPIWithTunnel(st, v, "", nil) }

// NewAPIWithTunnel is the full account-facing API: device login, enroll, and —
// when router is non-nil — the /agents/ control proxy (#73).
func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string, router *Router) http.Handler {
	a := &api{st: st, v: v, tunnelEndpoint: tunnelEndpoint}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("POST /v1/enroll", a.enroll)
	if router != nil {
		mux.Handle("/agents/", NewControlProxy(st, router))
	}
	return mux
}

type api struct {
	st             *Store
	v              Verifier
	tunnelEndpoint string
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
