// Package web serves the panel: the embedded frontend, a small JSON API,
// session authentication and the rate limiting that keeps both honest.
//
// The API is strictly read-only. There is no endpoint that changes the
// state of the machine, by design: a monitoring panel that can stop
// services is a remote shell with a nice font.
package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/config"
)

// SessionCookie is the name of the session cookie. It carries no data
// beyond the signature and the expiry.
const SessionCookie = "eol_session"

// authState holds the password check, the signing key and the login
// throttle.
type authState struct {
	passwordHash string
	secret       []byte
	ttl          time.Duration
	trustProxy   bool

	mu       sync.Mutex
	attempts map[string]*attempts
}

type attempts struct {
	count int
	until time.Time
}

func newAuth(cfg config.Config) *authState {
	return &authState{
		passwordHash: cfg.Password,
		secret:       []byte(cfg.SessionSecret),
		ttl:          cfg.SessionTTL.D(),
		trustProxy:   cfg.TrustProxy,
		attempts:     make(map[string]*attempts, 32),
	}
}

// loginLimit and loginWindow bound how often a client may try a password.
const (
	loginLimit  = 8
	loginWindow = 15 * time.Minute
)

// checkLogin reports whether the password is right and whether the client
// is currently throttled.
func (a *authState) checkLogin(ip, password string) (ok bool, retryAfter time.Duration) {
	if wait := a.throttled(ip); wait > 0 {
		return false, wait
	}
	if !config.VerifyPassword(a.passwordHash, password) {
		a.recordFailure(ip)
		return false, 0
	}
	a.clearFailures(ip)
	return true, 0
}

func (a *authState) throttled(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.attempts[ip]
	if !ok {
		return 0
	}
	if time.Now().After(rec.until) {
		delete(a.attempts, ip)
		return 0
	}
	if rec.count >= loginLimit {
		return time.Until(rec.until)
	}
	return 0
}

func (a *authState) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.attempts[ip]
	if !ok || time.Now().After(rec.until) {
		rec = &attempts{}
		a.attempts[ip] = rec
	}
	rec.count++
	rec.until = time.Now().Add(loginWindow)
	// Keep the map from growing without bound on a hostile network.
	if len(a.attempts) > 4096 {
		for k, v := range a.attempts {
			if time.Now().After(v.until) {
				delete(a.attempts, k)
			}
		}
	}
}

func (a *authState) clearFailures(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.attempts, ip)
}

// newSession returns a signed cookie value for the given expiry.
func (a *authState) newSession(expiry time.Time) string {
	payload := expiry.UTC().Format(time.RFC3339)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return payload + "|" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validSession reports whether a cookie value carries a good signature and
// has not expired.
func (a *authState) validSession(value string) bool {
	payload, sig, ok := strings.Cut(value, "|")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return false
	}
	expiry, err := time.Parse(time.RFC3339, payload)
	if err != nil {
		return false
	}
	return time.Now().Before(expiry)
}

// setSession writes the session cookie.
func (a *authState) setSession(w http.ResponseWriter, r *http.Request, basePath string) {
	expiry := time.Now().Add(a.ttl)
	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookie,
		Value: a.newSession(expiry),
		Path:  cookiePath(basePath),
		// HttpOnly keeps the cookie away from scripts; SameSite=Strict
		// keeps it away from cross site requests, which is the whole of
		// the CSRF story for a panel that only reads.
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		Expires:  expiry,
		MaxAge:   int(a.ttl.Seconds()),
	})
}

// clearSession expires the session cookie.
func (a *authState) clearSession(w http.ResponseWriter, r *http.Request, basePath string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     cookiePath(basePath),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

func cookiePath(basePath string) string {
	if basePath == "" {
		return "/"
	}
	return basePath
}

// authenticated reports whether the request carries a valid session.
func (a *authState) authenticated(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return a.validSession(c.Value)
}

// clientIP returns the address a request came from, honouring
// X-Forwarded-For only when the operator said a proxy is in front.
func (a *authState) clientIP(r *http.Request) string {
	if a.trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if first, _, ok := strings.Cut(fwd, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(fwd)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// csrfOK rejects state changing requests whose Origin or Referer does not
// match the Host. The API has exactly two such endpoints, login and
// logout, and both are worth protecting.
func csrfOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer == "" {
			// No browser sent either header, so this is not a cross site
			// form post. SameSite=Strict already covers the cookie case.
			return true
		}
		origin = referer
	}
	host := r.Host
	origin = strings.TrimSuffix(origin, "/")
	for _, scheme := range []string{"http://", "https://"} {
		if rest, ok := strings.CutPrefix(origin, scheme); ok {
			origin = rest
			break
		}
	}
	return origin == host || strings.HasPrefix(origin, host+"/")
}

// randomToken returns a hex token of n bytes, used for nothing secret but
// for values that must not repeat.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; if it does, a timestamp
		// still keeps values apart.
		return base64.RawURLEncoding.EncodeToString([]byte(time.Now().String()))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var _ = json.Marshal
