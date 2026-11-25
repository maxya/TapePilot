package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/maxya/TapePilot/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testPassword = "synthetic-only-password-123"

func fixture(t *testing.T) (*Auth, *store.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = Bootstrap(context.Background(), db, testPassword); err != nil {
		t.Fatal(err)
	}
	a, err := New(db, "http://localhost:8787")
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Write([]byte(`{"status":"ok","mode":"simulator"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	return a, db, a.Wrap(next)
}

type sessionReply struct {
	Authenticated bool   `json:"authenticated"`
	CSRF          string `json:"csrf_token"`
}

func request(handler http.Handler, method, path, body string, cookie *http.Cookie, csrfValue, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:8787"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrfValue != "" {
		r.Header.Set("X-CSRF-Token", csrfValue)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func session(t *testing.T, h http.Handler) (*http.Cookie, sessionReply) {
	t.Helper()
	w := request(h, "GET", "/api/v1/auth/session", "", nil, "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result sessionReply
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal(cookies)
	}
	return cookies[0], result
}
func login(t *testing.T, h http.Handler) (*http.Cookie, sessionReply) {
	t.Helper()
	anonymous, s := session(t, h)
	body, _ := json.Marshal(map[string]string{"password": testPassword})
	w := request(h, "POST", "/api/v1/auth/login", string(body), anonymous, s.CSRF, "http://localhost:8787")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var signed sessionReply
	json.Unmarshal(w.Body.Bytes(), &signed)
	return w.Result().Cookies()[0], signed
}
func TestLoginRotationCSRFLogoutAndReset(t *testing.T) {
	a, db, h := fixture(t)
	anonymous, guest := session(t, h)
	if guest.Authenticated || len(guest.CSRF) != 43 || !anonymous.HttpOnly || anonymous.SameSite != http.SameSiteStrictMode || anonymous.Secure {
		t.Fatal(anonymous, guest)
	}
	passwordJSON, _ := json.Marshal(map[string]string{"password": testPassword})
	w := request(h, "POST", "/api/v1/auth/login", string(passwordJSON), anonymous, guest.CSRF, "http://localhost:8787")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	signed := w.Result().Cookies()[0]
	if signed.Value == anonymous.Value {
		t.Fatal("session did not rotate")
	}
	var authenticated sessionReply
	json.Unmarshal(w.Body.Bytes(), &authenticated)
	if !authenticated.Authenticated || authenticated.CSRF == guest.CSRF {
		t.Fatal(authenticated)
	}
	old := request(h, "GET", "/api/v1/jobs", "", anonymous, "", "")
	if old.Code != 401 {
		t.Fatal("old cookie still authenticated", old.Code)
	}
	good := request(h, "POST", "/api/v1/jobs", "{}", signed, authenticated.CSRF, "http://localhost:8787")
	if good.Code != 200 {
		t.Fatal(good.Code, good.Body.String())
	}
	r := httptest.NewRequest("GET", "http://localhost:8787/api/v1/jobs/stream", nil)
	r.AddCookie(signed)
	if !a.ValidSession(context.Background(), r) {
		t.Fatal("SSE session not valid")
	}
	if err := Reset(context.Background(), db, "new-synthetic-password-456"); err != nil {
		t.Fatal(err)
	}
	if a.ValidSession(context.Background(), r) {
		t.Fatal("password reset left SSE authorized")
	}
	revoked := request(h, "GET", "/api/v1/jobs", "", signed, "", "")
	if revoked.Code != 401 {
		t.Fatal(revoked.Code)
	}
	// Re-establish the original test password for the logout half of the flow.
	if err := Reset(context.Background(), db, testPassword); err != nil {
		t.Fatal(err)
	}
	cookie, current := login(t, h)
	logout := request(h, "POST", "/api/v1/auth/logout", "{}", cookie, current.CSRF, "http://localhost:8787")
	if logout.Code != 200 {
		t.Fatal(logout.Code, logout.Body.String())
	}
	if w := request(h, "GET", "/api/v1/jobs", "", cookie, "", ""); w.Code != 401 {
		t.Fatal("logout cookie still valid", w.Code)
	}
}
func TestSecurityGuardsAndPublicHealth(t *testing.T) {
	_, _, h := fixture(t)
	cookie, s := login(t, h)
	for _, test := range []struct {
		method, path, csrf, origin, host string
		want                             int
	}{
		{"POST", "/api/v1/jobs", "", "http://localhost:8787", "", 403},
		{"POST", "/api/v1/jobs", s.CSRF, "", "", 403},
		{"POST", "/api/v1/jobs", s.CSRF, "https://evil.example", "", 403},
		{"GET", "/api/v1/jobs", "", "", "evil.example", 403},
		{"OPTIONS", "/api/v1/jobs", "", "http://localhost:8787", "", 403},
	} {
		r := httptest.NewRequest(test.method, "http://localhost:8787"+test.path, strings.NewReader(`{}`))
		r.AddCookie(cookie)
		if test.host != "" {
			r.Host = test.host
		}
		if test.origin != "" {
			r.Header.Set("Origin", test.origin)
		}
		r.Header.Set("X-CSRF-Token", test.csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatal(test, w.Code, w.Body.String())
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS access allowed")
		}
		if w.Header().Get("X-Request-ID") == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Header())
		}
	}
	if w := request(h, "GET", "/api/v1/jobs", "", nil, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	// Liveness stays usable on the private backend host for its container check.
	r := httptest.NewRequest("GET", "http://127.0.0.1:8788/api/v1/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestLoginRequiresAnonymousCSRFAndStrictBoundedJSON(t *testing.T) {
	_, _, h := fixture(t)
	if w := request(h, "POST", "/api/v1/auth/login", `{"password":"`+testPassword+`"}`, nil, "", "http://localhost:8787"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	cookie, s := session(t, h)
	for _, body := range []string{`{"password":"a","password":"b"}`, `{"password":"a","extra":true}`, `{"password":"a"}{}`, `{"password":"` + strings.Repeat("x", 8193) + `"}`} {
		w := request(h, "POST", "/api/v1/auth/login", body, cookie, s.CSRF, "http://localhost:8787")
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestInvalidCredentialsRateLimitAndNoCredentialEcho(t *testing.T) {
	_, _, h := fixture(t)
	cookie, s := session(t, h)
	for i := 0; i < 6; i++ {
		w := request(h, "POST", "/api/v1/auth/login", `{"password":"do-not-echo-this-password"}`, cookie, s.CSRF, "http://localhost:8787")
		expected := 401
		if i == 5 {
			expected = 429
		}
		if w.Code != expected {
			t.Fatal(i, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "do-not-echo") {
			t.Fatal("credential echoed")
		}
	}
}
func TestSecureOriginAndCookie(t *testing.T) {
	_, db, _ := fixture(t)
	for _, origin := range []string{"http://192.0.2.1:8787", "https://user:password@example.com", "https://example.com/path", "https://example.com?x=1", "ftp://example.com"} {
		if _, err := New(db, origin); err == nil {
			t.Fatal("unsafe origin accepted", origin)
		}
	}
	a, err := New(db, "https://tapepilot.example")
	if err != nil {
		t.Fatal(err)
	}
	h := a.Wrap(http.NotFoundHandler())
	r := httptest.NewRequest("GET", "https://tapepilot.example/api/v1/auth/session", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal(w.Code, w.Header())
	}
}
func TestPasswordHashParametersAndRandomSalt(t *testing.T) {
	first, err := PasswordHash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PasswordHash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "$argon2id$v=19$m=65536,t=3,p=2$") || !PasswordMatches(first, testPassword) || PasswordMatches(first, "wrong") {
		t.Fatal("password derivation failed")
	}
	for _, bad := range []string{"short", strings.Repeat("a", 1025), string([]byte{0xff, 0xfe}) + strings.Repeat("a", 12)} {
		if _, err = PasswordHash(bad); err == nil {
			t.Fatal("password policy bypass")
		}
	}
	if PasswordMatches(strings.Replace(first, "m=65536", "m=4294967295", 1), testPassword) {
		t.Fatal("unbounded password hash accepted")
	}
}
func TestLimiterBoundsAddressState(t *testing.T) {
	l := limiter{burst: 1, period: time.Minute}
	now := time.Now()
	for i := 0; i < 2000; i++ {
		l.allow(fmt.Sprintf("192.0.2.%d:1234", i), now.Add(time.Duration(i)*time.Hour))
	}
	if len(l.entries) > 256 {
		t.Fatal(len(l.entries))
	}
}

func BenchmarkPasswordHash(b *testing.B) {
	for range b.N {
		if _, err := PasswordHash(testPassword); err != nil {
			b.Fatal(err)
		}
	}
}

func TestNoDefaultCredentialsOrHTTPBootstrap(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := New(db, "http://localhost:8787")
	if err != nil {
		t.Fatal(err)
	}
	h := a.Wrap(http.NotFoundHandler())
	cookie, s := session(t, h)
	loginAttempt := request(h, "POST", "/api/v1/auth/login", `{"password":"admin"}`, cookie, s.CSRF, "http://localhost:8787")
	if loginAttempt.Code != 401 {
		t.Fatal(loginAttempt.Code, loginAttempt.Body.String())
	}
	bootstrapAttempt := request(h, "POST", "/api/v1/auth/bootstrap", `{"password":"synthetic-invalid-bootstrap"}`, cookie, s.CSRF, "http://localhost:8787")
	if bootstrapAttempt.Code != 401 {
		t.Fatal(bootstrapAttempt.Code, bootstrapAttempt.Body.String())
	}
	if _, err = db.AdminCredential(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("HTTP request created administrator", err)
	}
}
