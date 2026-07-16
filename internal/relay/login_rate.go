package relay

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Login rate limits (#106): the two unauthenticated login endpoints
// (POST /v1/login/device, GET /v1/login/web) share one per-IP token bucket.
// Real logins are human-paced — single-digit attempts per minute even with
// retries — and every device-flow start also costs an upstream IdP call, so
// these values give honest users ample headroom while capping scripted abuse.
const (
	loginLimitBurst   = 10               // requests one IP may fire at once
	loginLimitPerMin  = 30               // sustained refill
	loginLimitMaxIdle = 10 * time.Minute // idle buckets evicted (mirrors the web-state TTL)
)

// loginLimiter is a per-IP token bucket for the unauthenticated login
// endpoints. The zero value is ready to use. Buckets are swept inline on each
// check — the same opportunistic pattern as the web-state sweep in loginWeb —
// so there is no janitor goroutine.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginBucket
	now     func() time.Time // test seam; nil ⇒ time.Now
}

type loginBucket struct {
	lim  *rate.Limiter
	seen time.Time // last request, for idle eviction
}

// allow reports whether ip may make another login request at this moment.
func (l *loginLimiter) allow(ip string) bool {
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if now.Sub(b.seen) > loginLimitMaxIdle {
			delete(l.buckets, k)
		}
	}
	b, ok := l.buckets[ip]
	if !ok {
		if l.buckets == nil {
			l.buckets = map[string]*loginBucket{}
		}
		b = &loginBucket{lim: rate.NewLimiter(rate.Every(time.Minute/loginLimitPerMin), loginLimitBurst)}
		l.buckets[ip] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// clientIP derives the rate-limit key from the request's direct peer. The
// relay terminates TLS itself with no trusted proxy in front, and nothing in
// this codebase honors X-Forwarded-For, so RemoteAddr is the client IP.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
