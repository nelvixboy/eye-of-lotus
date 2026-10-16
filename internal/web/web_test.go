package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/config"
	"github.com/nelvixboy/eye-of-lotus/internal/model"
)

func testAuth(t *testing.T, password string, ttl time.Duration, trustProxy bool) *authState {
	t.Helper()
	hash, err := config.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return newAuth(config.Config{
		Password:      hash,
		SessionSecret: "a secret only the server knows",
		SessionTTL:    config.Duration(ttl),
		TrustProxy:    trustProxy,
	})
}

func TestSessionTokensAreSignedAndExpire(t *testing.T) {
	a := testAuth(t, "correct horse", time.Hour, false)

	if !a.validSession(a.newSession(time.Now().Add(time.Hour))) {
		t.Fatal("a session this server just signed was rejected")
	}

	// The expiry is inside the signed payload, so rewriting it must not
	// work: that is the difference between a session and a suggestion.
	good := a.newSession(time.Now().Add(time.Hour))
	_, sig, _ := strings.Cut(good, "|")
	forged := time.Now().Add(500*time.Hour).UTC().Format(time.RFC3339) + "|" + sig
	if a.validSession(forged) {
		t.Error("a session with a rewritten expiry was accepted")
	}

	if a.validSession(good[:len(good)-2] + "xy") {
		t.Error("a session with a broken signature was accepted")
	}
	if a.validSession(a.newSession(time.Now().Add(-time.Minute))) {
		t.Error("an expired session was accepted")
	}

	other := newAuth(config.Config{
		Password:      "x",
		SessionSecret: "a different secret",
		SessionTTL:    config.Duration(time.Hour),
	})
	if a.validSession(other.newSession(time.Now().Add(time.Hour))) {
		t.Error("a session signed with another secret was accepted")
	}

	// Shapes that are not sessions at all.
	for _, bad := range []string{"", "|", "garbage", "no-separator", "not-a-time|signature"} {
		if a.validSession(bad) {
			t.Errorf("validSession(%q) = true, want false", bad)
		}
	}
}

func TestLoginThrottleLocksOneAddressOnly(t *testing.T) {
	a := testAuth(t, "correct horse", time.Hour, false)

	for i := 0; i < loginLimit; i++ {
		ok, wait := a.checkLogin("10.0.0.1", "wrong")
		if ok {
			t.Fatal("a wrong password was accepted")
		}
		if wait != 0 {
			t.Fatalf("throttled after %d attempts, before the limit of %d", i+1, loginLimit)
		}
	}

	// Past the limit even the right password is refused, with a real wait.
	ok, wait := a.checkLogin("10.0.0.1", "correct horse")
	if ok {
		t.Error("the right password was accepted while the address was throttled")
	}
	if wait <= 0 || wait > loginWindow {
		t.Errorf("retryAfter = %v, want something within %v", wait, loginWindow)
	}

	// One client must not be able to lock out another.
	if ok, _ := a.checkLogin("10.0.0.2", "correct horse"); !ok {
		t.Error("a second address was throttled by the first one's failures")
	}
}

func TestThrottleLapsesAndIsClearedByASuccess(t *testing.T) {
	a := testAuth(t, "correct horse", time.Hour, false)

	a.recordFailure("10.0.0.3")
	a.mu.Lock()
	a.attempts["10.0.0.3"].until = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if wait := a.throttled("10.0.0.3"); wait != 0 {
		t.Errorf("a lapsed record still throttles for %v", wait)
	}
	a.mu.Lock()
	_, stillThere := a.attempts["10.0.0.3"]
	a.mu.Unlock()
	if stillThere {
		t.Error("a lapsed record was kept")
	}

	// A success clears the count, so the next failed attempts start over.
	for i := 0; i < loginLimit-2; i++ {
		a.checkLogin("10.0.0.4", "wrong")
	}
	if ok, _ := a.checkLogin("10.0.0.4", "correct horse"); !ok {
		t.Fatal("the right password was refused before the limit was reached")
	}
	a.mu.Lock()
	count := len(a.attempts)
	a.mu.Unlock()
	if count != 0 {
		t.Errorf("a successful login left %d address records behind", count)
	}
}

// TestClientIPIgnoresForwardedForUnlessTrusted is the anti-spoofing test. A
// client that could name its own address would walk past the login throttle
// by claiming a new one on every attempt.
func TestClientIPIgnoresForwardedForUnlessTrusted(t *testing.T) {
	plain := testAuth(t, "x", time.Hour, false)
	trusting := testAuth(t, "x", time.Hour, true)

	r := httptest.NewRequest("GET", "/api/health", nil)
	r.RemoteAddr = "192.0.2.10:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.5")

	if got := plain.clientIP(r); got != "192.0.2.10" {
		t.Errorf("without trust_proxy the address is %q, want the socket address", got)
	}
	if got := trusting.clientIP(r); got != "203.0.113.5" {
		t.Errorf("with trust_proxy the address is %q, want the forwarded one", got)
	}

	r.Header.Set("X-Forwarded-For", "203.0.113.5, 198.51.100.7")
	if got := trusting.clientIP(r); got != "203.0.113.5" {
		t.Errorf("a forwarded chain resolved to %q, want the first entry", got)
	}

	r.Header.Del("X-Forwarded-For")
	if got := trusting.clientIP(r); got != "192.0.2.10" {
		t.Errorf("without the header the address is %q, want the socket address", got)
	}
}

func TestCSRFOriginCheck(t *testing.T) {
	cases := []struct {
		name, host, origin, referer string
		want                        bool
	}{
		{name: "no browser headers at all", host: "lotus.home.lab", want: true},
		{name: "same origin", host: "lotus.home.lab", origin: "https://lotus.home.lab", want: true},
		{name: "same origin with a path", host: "lotus.home.lab", origin: "https://lotus.home.lab/index.html", want: true},
		{name: "same origin with a trailing slash", host: "lotus.home.lab", origin: "https://lotus.home.lab/", want: true},
		{name: "another site", host: "lotus.home.lab", origin: "https://evil.example", want: false},
		// A bare prefix comparison would accept this one, which is why the
		// case is here: the host must end, not merely begin.
		{name: "a host that begins the same", host: "lotus.home.lab", origin: "https://lotus.home.lab.evil.example", want: false},
		{name: "the same host on the other scheme", host: "lotus.home.lab", origin: "http://lotus.home.lab", want: true},
		{name: "a cross site referer", host: "lotus.home.lab", referer: "https://evil.example/form", want: false},
		{name: "a matching referer", host: "lotus.home.lab", referer: "https://lotus.home.lab/", want: true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "http://"+c.host+"/api/auth/login", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.referer != "" {
			r.Header.Set("Referer", c.referer)
		}
		if got := csrfOK(r); got != c.want {
			t.Errorf("%s: csrfOK = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCookiePath(t *testing.T) {
	if got := cookiePath(""); got != "/" {
		t.Errorf("cookiePath(\"\") = %q, want /", got)
	}
	if got := cookiePath("/lotus"); got != "/lotus" {
		t.Errorf("cookiePath(\"/lotus\") = %q", got)
	}
}

func TestStripBase(t *testing.T) {
	at := func(base string) *Server {
		return New(Options{Config: config.Config{Password: "x", BasePath: base}})
	}

	// Without a prefix every path passes through untouched.
	s := at("")
	r := httptest.NewRequest("GET", "/anything", nil)
	if !s.stripBase(httptest.NewRecorder(), r) {
		t.Error("a server without a base path refused a request")
	}

	s = at("/lotus")

	// The bare prefix redirects once, which is what a person typing the
	// hostname expects.
	rec := httptest.NewRecorder()
	if s.stripBase(rec, httptest.NewRequest("GET", "/lotus", nil)) {
		t.Error("the bare base path was served instead of redirected")
	}
	if rec.Code != http.StatusFound {
		t.Errorf("the bare base path returned %d, want a redirect", rec.Code)
	}

	// Inside the prefix, it is removed before routing.
	r = httptest.NewRequest("GET", "/lotus/api/health", nil)
	if !s.stripBase(httptest.NewRecorder(), r) {
		t.Fatal("a request inside the base path was refused")
	}
	if r.URL.Path != "/api/health" {
		t.Errorf("the path after stripping is %q, want /api/health", r.URL.Path)
	}

	// Outside it there is nothing to serve.
	rec = httptest.NewRecorder()
	if s.stripBase(rec, httptest.NewRequest("GET", "/other", nil)) {
		t.Error("a request outside the base path was served")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("a request outside the base path returned %d, want 404", rec.Code)
	}
}

type fakeCollector struct{ snap model.Snapshot }

func (f fakeCollector) Snapshot(context.Context) model.Snapshot { return f.snap }

func TestSecurityHeadersAndProtectedRoutes(t *testing.T) {
	s := New(Options{Config: config.Config{
		Password:      "correct horse",
		SessionSecret: "a secret only the server knows",
		SessionTTL:    config.Duration(time.Hour),
	}, Collector: fakeCollector{}, Version: "test"})
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/robots.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("robots.txt returned %d", rec.Code)
	}
	for _, header := range []string{
		"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy",
	} {
		if rec.Header().Get(header) == "" {
			t.Errorf("the %s header is missing", header)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("the policy does not forbid framing: %q", csp)
	}

	// The data endpoints are closed without a session.
	for _, path := range []string{"/api/snapshot", "/api/meta", "/api/history", "/api/events"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session returned %d, want 401", path, rec.Code)
		}
	}

	// A wrong password does not open one either.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://localhost/api/auth/login",
		strings.NewReader(`{"password":"wrong"}`))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("a wrong password was accepted")
	}

	// The right one returns a cookie that then opens the data.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "http://localhost/api/auth/login",
		strings.NewReader(`{"password":"correct horse"}`))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the right password returned %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("a successful login set no cookie")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("the session cookie is not HttpOnly and SameSite=Strict: %+v", cookies[0])
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "http://localhost/api/snapshot", nil)
	req.Host = "localhost"
	req.AddCookie(cookies[0])
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a request with a session returned %d: %s", rec.Code, rec.Body.String())
	}
	var snap model.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("the snapshot is not valid JSON: %v", err)
	}

	// A cross site login is refused even with the right password.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "http://localhost/api/auth/login",
		strings.NewReader(`{"password":"correct horse"}`))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a cross site login returned %d, want 403", rec.Code)
	}
}
