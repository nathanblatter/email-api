// Package ratelimit is a small in-memory per-IP token bucket for the public,
// internet-reachable OAuth endpoints.
package ratelimit

import (
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type visitor struct {
	tokens float64
	last   time.Time
}

// IPLimiter allows burst requests at once and refills rate per second per IP.
type IPLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rate     float64
	burst    float64
}

func New(ratePerSec float64, burst int) *IPLimiter {
	l := &IPLimiter{visitors: map[string]*visitor{}, rate: ratePerSec, burst: float64(burst)}
	go l.janitor()
	return l
}

func (l *IPLimiter) janitor() {
	for range time.Tick(10 * time.Minute) {
		l.mu.Lock()
		for ip, v := range l.visitors {
			if time.Since(v.last) > 10*time.Minute {
				delete(l.visitors, ip)
			}
		}
		l.mu.Unlock()
	}
}

func (l *IPLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	v, ok := l.visitors[ip]
	if !ok {
		l.visitors[ip] = &visitor{tokens: l.burst - 1, last: now}
		return true
	}
	v.tokens = math.Min(l.burst, v.tokens+now.Sub(v.last).Seconds()*l.rate)
	v.last = now
	if v.tokens >= 1 {
		v.tokens--
		return true
	}
	return false
}

func (l *IPLimiter) Middleware(reject func(http.ResponseWriter), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(ClientIP(r)) {
			reject(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP honours the first hop of X-Forwarded-For (cloudflared sets it).
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
