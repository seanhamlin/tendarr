package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/seanhamlin/tendarr/internal/store"
)

// bcryptCost is a variable so tests can lower it.
var bcryptCost = 12

const (
	sessionCookie = "tendarr_session"
	// MinPasswordLength follows NIST 800-63B's guidance for user-chosen
	// passwords protecting something that matters.
	MinPasswordLength = 12
	maxPasswordLength = 72 // bcrypt ignores bytes beyond this
)

type ctxKey int

const sessionKey ctxKey = iota

// currentSession returns the signed-in session, if any.
func currentSession(r *http.Request) (store.Session, bool) {
	s, ok := r.Context().Value(sessionKey).(store.Session)
	return s, ok
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashPassword hashes a password with bcrypt.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

// dummyHash is compared against when the username does not exist, so a
// failed login takes the same time whether or not the account is real.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equaliser-not-a-password"), 12)

// ValidateUsername checks a proposed username.
func ValidateUsername(u string) string {
	u = strings.TrimSpace(u)
	switch {
	case u == "":
		return "Username is required"
	case utf8.RuneCountInString(u) < 3 || utf8.RuneCountInString(u) > 64:
		return "Username must be between 3 and 64 characters"
	case strings.ContainsAny(u, " \t\r\n"):
		return "Username cannot contain spaces"
	}
	return ""
}

// ValidatePassword checks a proposed password.
func ValidatePassword(pw, confirm, username string) string {
	switch {
	case utf8.RuneCountInString(pw) < MinPasswordLength:
		return "Password must be at least 12 characters"
	case len(pw) > maxPasswordLength:
		return "Password must be at most 72 bytes"
	case strings.EqualFold(pw, username):
		return "Password cannot be the same as the username"
	case pw != confirm:
		return "Passwords do not match"
	}
	return ""
}

func (s *Server) cookieSecure(r *http.Request) bool {
	switch s.cfg.CookieSecure {
	case "true":
		return true
	case "false":
		return false
	}
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token := randomToken()
	if err := s.store.CreateSession(r.Context(), hashToken(token), userID, randomToken(), s.cfg.SessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode,
	})
}

// withSession loads the session, if the cookie names a live one.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := s.store.SessionByHash(r.Context(), hashToken(c.Value))
		if errors.Is(err, store.ErrNotFound) {
			s.clearSessionCookie(w, r)
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		// Slide the expiry, but at most hourly to avoid a write per request.
		if time.Until(sess.ExpiresAt) < s.cfg.SessionTTL-time.Hour {
			if err := s.store.TouchSession(r.Context(), sess.TokenHash, s.cfg.SessionTTL); err != nil {
				s.logger.Warn("could not extend session", "error", err)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

// requireAuth redirects anonymous visitors to the login page and checks the
// CSRF token on every state-changing request. Cross-origin form posts are
// already refused by CrossOriginProtection; the token is defence in depth.
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := currentSession(r)
		if !ok {
			if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/job") {
				http.Redirect(w, r, "/login?next="+safeNext(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "Sign in required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Bound form bodies; the largest legitimate form is a few hundred
			// checkbox values.
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRFToken)) != 1 {
				s.logger.Warn("csrf token mismatch", "path", r.URL.Path, "user", sess.Username)
				http.Error(w, "Your session form token is invalid or expired. Go back, refresh the page and try again.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// safeNext keeps post-login redirects on this site. Browsers strip tabs and
// newlines from URLs and treat a backslash as a slash, so "/\t/evil.com" would
// become "//evil.com"; any of those characters anywhere rejects the target.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.IndexFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) >= 0 {
		return "/"
	}
	return next
}

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "setup", map[string]any{"Errors": map[string]string{}})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	username := strings.TrimSpace(r.PostFormValue("username"))
	pw := r.PostFormValue("password")

	errs := map[string]string{}
	if msg := ValidateUsername(username); msg != "" {
		errs["username"] = msg
	}
	if msg := ValidatePassword(pw, r.PostFormValue("confirm"), username); msg != "" {
		errs["password"] = msg
	}
	if len(errs) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "setup", map[string]any{"Errors": errs, "Username": username})
		return
	}

	hash, err := HashPassword(pw)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.setupMu.Lock()
	user, err := s.store.CreateFirstUser(r.Context(), username, hash)
	s.setupMu.Unlock()
	if errors.Is(err, store.ErrSetupComplete) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.logger.Info("initial account created", "user", user.Username)
	if err := s.startSession(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.flash(w, r, "success", "Account created. Next, connect your services.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", map[string]any{"Next": safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	username := strings.TrimSpace(r.PostFormValue("username"))
	pw := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	ip := clientIP(r)

	fail := func(status int, msg string) {
		s.render(w, r, status, "login", map[string]any{"Error": msg, "Username": username, "Next": next})
	}

	if wait := s.limiter.blocked(ip); wait > 0 {
		s.logger.Warn("login rate limited", "ip", ip, "user", username)
		fail(http.StatusTooManyRequests, "Too many failed sign-in attempts. Try again in "+wait.Round(time.Minute).String()+".")
		return
	}

	if username == "" || pw == "" || len(pw) > maxPasswordLength {
		fail(http.StatusUnprocessableEntity, "Enter your username and password.")
		return
	}

	user, err := s.store.UserByUsername(r.Context(), username)
	hash := dummyHash
	if err == nil {
		hash = []byte(user.PasswordHash)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}

	if bcrypt.CompareHashAndPassword(hash, []byte(pw)) != nil || err != nil {
		s.limiter.fail(ip)
		s.logger.Warn("login failed", "ip", ip, "user", username)
		fail(http.StatusUnauthorized, "Incorrect username or password.")
		return
	}

	s.limiter.reset(ip)
	if err := s.startSession(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.logger.Info("login", "user", user.Username, "ip", ip)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess, ok := currentSession(r); ok {
		if err := s.store.DeleteSession(r.Context(), sess.TokenHash); err != nil {
			s.logger.Warn("could not delete session", "error", err)
		}
		s.logger.Info("logout", "user", sess.Username)
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := currentSession(r)
	user, err := s.store.UserByID(r.Context(), sess.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	errs := map[string]string{}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(r.PostFormValue("current"))) != nil {
		errs["current"] = "Current password is incorrect"
	}
	pw := r.PostFormValue("password")
	if msg := ValidatePassword(pw, r.PostFormValue("confirm"), user.Username); msg != "" {
		errs["password"] = msg
	}
	if len(errs) > 0 {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, settingsPage{PasswordErrors: errs})
		return
	}

	hash, err := HashPassword(pw)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.UpdatePassword(r.Context(), user.ID, hash, sess.TokenHash); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.logger.Info("password changed", "user", user.Username)
	s.flash(w, r, "success", "Password changed. Other signed-in browsers have been signed out.")
	http.Redirect(w, r, "/settings#account", http.StatusSeeOther)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLimiter throttles failed logins per client address.
type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
	now    func() time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, fails: map[string][]time.Time{}, now: time.Now}
}

func (l *loginLimiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

// blocked returns how long the key must wait, or 0.
func (l *loginLimiter) blocked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	fails := l.prune(key)
	if len(fails) < l.max {
		return 0
	}
	return fails[0].Add(l.window).Sub(l.now())
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(key)
	l.fails[key] = append(l.fails[key], l.now())
	// Bound memory if something sprays addresses.
	if len(l.fails) > 10000 {
		for k := range l.fails {
			delete(l.fails, k)
			if len(l.fails) <= 5000 {
				break
			}
		}
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
