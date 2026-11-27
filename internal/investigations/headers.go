package investigations

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContentSecurityPolicy allows only the embedded application's own assets. The
// dashboard ships no third-party script; inline styles are required by React.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; " +
	"font-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// securityHeaders applies the response policy to every route, including the
// probes and the dashboard.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		header := response.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		header.Set("Cross-Origin-Resource-Policy", "same-origin")
		if requestIsSecure(request) {
			header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(response, request)
	})
}

// loginLimiter bounds repeated token attempts per client address. The service
// is a single replica, so an in-process counter is the whole installation.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	now      func() time.Time
}

const (
	loginWindow    = 15 * time.Minute
	loginMaxFailed = 10
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string][]time.Time), now: time.Now}
}

// allow reports whether another attempt is permitted. A successful session
// resets the client's window.
func (l *loginLimiter) allow(request *http.Request) bool {
	address := clientAddress(request)
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-loginWindow)
	kept := l.attempts[address][:0]
	for _, attempt := range l.attempts[address] {
		if attempt.After(cutoff) {
			kept = append(kept, attempt)
		}
	}
	l.attempts[address] = kept
	return len(kept) < loginMaxFailed
}

func (l *loginLimiter) fail(request *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	address := clientAddress(request)
	l.attempts[address] = append(l.attempts[address], l.now())
}

func (l *loginLimiter) reset(request *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, clientAddress(request))
}

func clientAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(request.RemoteAddr))
	if err == nil {
		return host
	}
	if host := strings.TrimSpace(request.RemoteAddr); host != "" {
		return host
	}
	return "unknown"
}

// retryAfter seconds advertised by a refused login.
func retryAfterSeconds() string {
	return strconv.Itoa(int(loginWindow / time.Second))
}
