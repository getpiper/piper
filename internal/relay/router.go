package relay

import (
	"strings"
	"sync"

	"github.com/getpiper/piper/internal/tunnel"
)

// Router maps an incoming SNI hostname to the agent session whose base domain
// owns it (exact match or subdomain). Registrations are keyed by base domain.
type Router struct {
	mu     sync.RWMutex
	byBase map[string]*tunnel.Session
}

func NewRouter() *Router { return &Router{byBase: map[string]*tunnel.Session{}} }

func (r *Router) Register(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byBase[sess.BaseDomain] = sess
}

func (r *Router) Unregister(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byBase[sess.BaseDomain] == sess {
		delete(r.byBase, sess.BaseDomain)
	}
}

// Lookup returns the session for an SNI equal to, or a subdomain of, a
// registered base domain.
func (r *Router) Lookup(sni string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.byBase[sni]; ok {
		return s, true
	}
	for base, s := range r.byBase {
		if strings.HasSuffix(sni, "."+base) {
			return s, true
		}
	}
	return nil, false
}
