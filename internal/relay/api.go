package relay

import (
	"encoding/json"
	"errors"
	"net/http"
)

// NewAPI returns the relay's self-service control API: device-flow login and
// account-bound enrollment. TLS termination for this handler is a deployment
// concern (front it with the api.<apex> cert); the handler itself is plain HTTP.
func NewAPI(st *Store, v Verifier) http.Handler {
	a := &api{st: st, v: v}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("POST /v1/enroll", a.enroll) // implemented in Task 7
	return mux
}

type api struct {
	st *Store
	v  Verifier
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
	acc, err := a.st.UpsertAccount(id.Subject, id.Email)
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
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
