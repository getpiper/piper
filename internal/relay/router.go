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
	byHost map[string]*tunnel.Session
}

func NewRouter() *Router {
	return &Router{
		byBase: map[string]*tunnel.Session{},
		byHost: map[string]*tunnel.Session{},
	}
}

func (r *Router) Register(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byBase[sess.BaseDomain] = sess
}

// RegisterHost maps an exact relay-terminated hostname to a session.
func (r *Router) RegisterHost(hostname string, sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byHost[hostname] = sess
}

// UnregisterHost removes a single terminated hostname.
func (r *Router) UnregisterHost(hostname string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byHost, hostname)
}

// LookupHost returns the session for an exact terminated hostname.
func (r *Router) LookupHost(hostname string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byHost[hostname]
	return s, ok
}

// RegisterCustom maps a BYO custom domain to sess. It shares byBase so
// Lookup's exact + subdomain matching applies unchanged.
func (r *Router) RegisterCustom(domain string, sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byBase[domain] = sess
}

// UnregisterCustom removes a custom-domain mapping.
func (r *Router) UnregisterCustom(domain string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byBase, domain)
}

func (r *Router) Unregister(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for base, s := range r.byBase {
		if s == sess {
			delete(r.byBase, base)
		}
	}
	for host, s := range r.byHost {
		if s == sess {
			delete(r.byHost, host)
		}
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
