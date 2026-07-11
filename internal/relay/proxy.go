package relay

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/getpiper/piper/internal/tunnel"
)

// NewControlProxy serves /agents and /agents/<base-domain>/v1/*: it
// authenticates the caller's relay account credential, authorizes that the
// account owns the agent, and reverse-proxies the request over the agent's
// tunnel as a KindControlAPI stream — swapping the caller's credential for the
// box's stored control bearer. The box still validates that bearer on every
// request (#77); the relay hop grants nothing at the box. Unknown and unowned
// agents are both 404 so existence is never leaked across tenants. Bare
// /agents lists the caller's own enrolled agents with liveness (#98).
func NewControlProxy(st *Store, router *Router) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing bearer credential", http.StatusUnauthorized)
			return
		}
		acc, err := st.AuthenticateAccount(cred)
		if err != nil {
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}

		// Path shape: /agents[/<base-domain>[/v1/...]]
		rest := strings.TrimPrefix(r.URL.Path, "/agents")
		rest = strings.TrimPrefix(rest, "/")
		base, tail, _ := strings.Cut(rest, "/")
		if base == "" {
			// List the account's own agents, each with liveness from the
			// in-memory session map — same answers as the per-agent endpoint,
			// and only ever the caller's rows, so nothing can leak.
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			visible, err := st.AgentsVisibleTo(acc.ID)
			if err != nil {
				http.Error(w, "list failed", http.StatusInternalServerError)
				return
			}
			agents := make([]map[string]any, 0, len(visible))
			for _, a := range visible {
				_, connected := router.Lookup(a.BaseDomain)
				agents = append(agents, map[string]any{"agent": a.BaseDomain, "connected": connected})
			}
			writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
			return
		}

		ownerID, _, err := st.AgentAccount(base)
		if err != nil || ownerID != acc.ID {
			http.NotFound(w, r)
			return
		}

		if tail == "" {
			// Liveness: answered by the relay itself from its in-memory
			// session map — never opens a tunnel stream. Offline is an
			// answer, not an error: 200 with connected:false.
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			_, connected := router.Lookup(base)
			writeJSON(w, http.StatusOK, map[string]any{"agent": base, "connected": connected})
			return
		}
		if !strings.HasPrefix(tail, "v1/") {
			http.NotFound(w, r)
			return
		}

		sess, ok := router.Lookup(base)
		if !ok {
			http.Error(w, "agent not connected", http.StatusServiceUnavailable)
			return
		}
		boxToken, err := st.ControlToken(base)
		if err != nil {
			http.Error(w, "agent lookup failed", http.StatusInternalServerError)
			return
		}

		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = base
				pr.Out.URL.Path = "/" + tail
				// Never forward the caller's account credential to the box.
				// Inject the box's own bearer; if the box never provisioned one,
				// forward bare and let its auth gate answer 401.
				pr.Out.Header.Del("Authorization")
				if boxToken != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+boxToken)
				}
			},
			Transport: &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return sess.OpenKind(tunnel.KindControlAPI)
				},
				// One tunnel stream per request: a pooled stream must never
				// outlive its session.
				DisableKeepAlives: true,
			},
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				http.Error(w, "box unreachable: "+err.Error(), http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}
