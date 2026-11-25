package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/store"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const cookieName = "tapepilot_session"

type requestIDKey struct{}
type Auth struct {
	db           *store.Store
	origin, host string
	secure       bool
	cookie       string
	dummy        string
	loginRate    limiter
	issueRate    limiter
	passwords    chan struct{}
}

func New(db *store.Store, origin string) (*Auth, error) {
	normalized, host, secure, err := parseOrigin(origin)
	if err != nil {
		return nil, err
	}
	dummy, err := PasswordHash("unusable-" + uuid.NewString())
	if err != nil {
		return nil, err
	}
	name := cookieName
	if secure {
		name = "__Host-" + cookieName
	}
	return &Auth{cookie: name, db: db, origin: normalized, host: host, secure: secure, dummy: dummy, loginRate: limiter{burst: 5, period: 30 * time.Second}, issueRate: limiter{burst: 30, period: time.Second}, passwords: make(chan struct{}, 2)}, nil
}
func parseOrigin(raw string) (string, string, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", false, errors.New("invalid public origin")
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", "", false, errors.New("invalid public origin host")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", "", false, errors.New("invalid public origin port")
		}
	}
	if strings.ContainsAny(hostname, " ,;\\\x00\r\n") || strings.HasSuffix(hostname, ".") {
		return "", "", false, errors.New("invalid public origin host")
	}
	ip := net.ParseIP(hostname)
	if u.Scheme == "http" && hostname != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", "", false, errors.New("HTTPS is required for non-loopback origins")
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = hostname
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return u.Scheme + "://" + host, host, u.Scheme == "https", nil
}
func RequestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "30")
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": RequestID(r)}})
}
func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/api/v1/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		requestHost := strings.ToLower(r.Host)
		defaultPort := ":80"
		if a.secure {
			defaultPort = ":443"
		}
		requestHost = strings.TrimSuffix(requestHost, defaultPort)

		if requestHost != a.host {
			WriteError(w, r, 403, "UNTRUSTED_HOST", "Request host does not match the configured origin")
			return
		}
		if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || origins[0] != a.origin) {
			WriteError(w, r, 403, "ORIGIN_REJECTED", "Request origin is not trusted")
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
			WriteError(w, r, 403, "ORIGIN_REJECTED", "Cross-site administrative requests are refused")
			return
		}
		if r.Method == http.MethodOptions {
			WriteError(w, r, 403, "ORIGIN_REJECTED", "Cross-origin access is unavailable")
			return
		}
		token, session, err := a.lookup(r.Context(), r, true)
		if err != nil {
			WriteError(w, r, 503, "AUTH_UNAVAILABLE", "Authentication is temporarily unavailable")
			return
		}
		if r.URL.Path == "/api/v1/auth/session" && r.Method == http.MethodGet {
			if token == "" {
				if !a.issueRate.allow(r.RemoteAddr, time.Now()) {
					WriteError(w, r, 429, "RATE_LIMITED", "Please wait before requesting another session")
					return
				}
				token, err = a.create(r.Context(), w, nil, "", RequestID(r))
				if err != nil {
					a.sessionError(w, r, err)
					return
				}
			}
			a.sessionJSON(w, token, session.AdminVersion != nil)
			return
		}
		mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
		if mutation {
			if r.Header.Get("Origin") != a.origin {
				WriteError(w, r, 403, "ORIGIN_REQUIRED", "A trusted Origin header is required")
				return
			}
			expected := csrf(token)
			if token == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
				WriteError(w, r, 403, "CSRF_REJECTED", "A current session CSRF token is required")
				return
			}
		}
		switch r.URL.Path {
		case "/api/v1/auth/login":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				WriteError(w, r, 405, "METHOD_NOT_ALLOWED", "Use POST for login")
				return
			}
			a.login(w, r, token)
			return
		case "/api/v1/auth/logout":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				WriteError(w, r, 405, "METHOD_NOT_ALLOWED", "Use POST for logout")
				return
			}
			a.logout(w, r, token)
			return
		case "/api/v1/auth/session":
			w.Header().Set("Allow", "GET")
			WriteError(w, r, 405, "METHOD_NOT_ALLOWED", "Use GET for session status")
			return
		}
		if session.AdminVersion == nil {
			WriteError(w, r, 401, "AUTH_REQUIRED", "Sign in to continue")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func csrf(token string) string {
	sum := sha256.Sum256([]byte("TapePilot CSRF v1\x00" + token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func (a *Auth) lookup(ctx context.Context, r *http.Request, touch bool) (string, store.Session, error) {
	var token string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == a.cookie {
			token = cookie.Value
			count++
		}
	}
	if count != 1 || len(token) != 43 {
		return "", store.Session{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return "", store.Session{}, nil
	}
	session, err := a.db.Session(ctx, tokenHash(token), time.Now(), touch)
	if errors.Is(err, store.ErrNotFound) {
		return "", store.Session{}, nil
	}
	if err != nil {
		return "", store.Session{}, err
	}
	return token, session, nil
}

// ValidSession does not extend idle expiry. SSE handlers call it periodically
// and close the stream when a password reset, logout or expiry revokes access.
func (a *Auth) ValidSession(ctx context.Context, r *http.Request) bool {
	_, s, err := a.lookup(ctx, r, false)
	return err == nil && s.AdminVersion != nil
}
func (a *Auth) create(ctx context.Context, w http.ResponseWriter, version *int64, oldToken, requestID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	absolute := now.Add(15 * time.Minute)
	idle := absolute
	if version != nil {
		absolute = now.Add(12 * time.Hour)
		idle = now.Add(30 * time.Minute)
	}
	oldHash := ""
	if oldToken != "" {
		oldHash = tokenHash(oldToken)
	}
	if err := a.db.CreateSession(ctx, store.Session{RequestID: requestID, TokenHash: tokenHash(token), AdminVersion: version, CreatedAt: now.Format(catalogLayout), LastSeenAt: now.Format(catalogLayout), IdleExpiresAt: idle.Format(catalogLayout), AbsoluteExpiresAt: absolute.Format(catalogLayout)}, oldHash); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookie, Value: token, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, Expires: absolute, MaxAge: int(time.Until(absolute).Seconds())})
	return token, nil
}

const catalogLayout = "2006-01-02T15:04:05.000000000Z"

func (a *Auth) sessionJSON(w http.ResponseWriter, token string, authenticated bool) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"authenticated": authenticated, "csrf_token": csrf(token)})
}
func (a *Auth) sessionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrSessionLimit) {
		WriteError(w, r, 429, "SESSION_LIMIT", "Too many active sessions. Wait for expiry or close another session")
		return
	}
	WriteError(w, r, 503, "AUTH_UNAVAILABLE", "Authentication is temporarily unavailable")
}
func (a *Auth) login(w http.ResponseWriter, r *http.Request, oldToken string) {
	if !a.loginRate.allow(r.RemoteAddr, time.Now()) {
		WriteError(w, r, 429, "RATE_LIMITED", "Please wait before trying to sign in again")
		return
	}
	select {
	case a.passwords <- struct{}{}:
		defer func() { <-a.passwords }()
	default:
		WriteError(w, r, 429, "RATE_LIMITED", "Please wait before trying to sign in again")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		WriteError(w, r, 415, "INVALID_CONTENT_TYPE", "Use application/json")
		return
	}
	password, err := loginPassword(w, r)
	if err != nil {
		WriteError(w, r, 400, "INVALID_REQUEST", "Provide one bounded password field")
		return
	}
	credential, err := a.db.AdminCredential(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, 503, "AUTH_UNAVAILABLE", "Authentication is temporarily unavailable")
		return
	}
	hash := credential.Hash
	if hash == "" {
		hash = a.dummy
	}
	matched := PasswordMatches(hash, password) && credential.Hash != ""
	if !matched {
		if err = a.db.AuthAudit(r.Context(), "LOGIN", "FAILED", RequestID(r)); err != nil {
			WriteError(w, r, 503, "AUTH_UNAVAILABLE", "Authentication is temporarily unavailable")
			return
		}
		WriteError(w, r, 401, "INVALID_CREDENTIALS", "Unable to sign in with those credentials")
		return
	}
	token, err := a.create(r.Context(), w, &credential.Version, oldToken, RequestID(r))
	if err != nil {
		a.sessionError(w, r, err)
		return
	}
	a.sessionJSON(w, token, true)
}
func loginPassword(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return "", store.ErrEvidence
	}
	name, err := decoder.Token()
	if err != nil || name != "password" {
		return "", store.ErrEvidence
	}
	var password string
	if err = decoder.Decode(&password); err != nil || len(password) > 1024 {
		return "", store.ErrEvidence
	}
	if decoder.More() {
		return "", store.ErrEvidence
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return "", store.ErrEvidence
	}
	if _, err = decoder.Token(); err != io.EOF {
		return "", store.ErrEvidence
	}
	return password, nil
}
func (a *Auth) logout(w http.ResponseWriter, r *http.Request, oldToken string) {
	if err := a.db.LogoutSession(r.Context(), tokenHash(oldToken), RequestID(r)); err != nil {
		WriteError(w, r, 503, "AUTH_UNAVAILABLE", "Authentication is temporarily unavailable")
		return
	}

	token, err := a.create(r.Context(), w, nil, "", RequestID(r))
	if err != nil {
		a.sessionError(w, r, err)
		return
	}
	a.sessionJSON(w, token, false)
}
