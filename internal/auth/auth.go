// Package auth is Cotta's built-in sign-in: browser sessions tied to the
// vault being unlocked, login throttling, and same-origin enforcement.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync"
	"time"

	"cotta/internal/vault"
)

const cookieName = "cotta_session"

type Auth struct {
	vault *vault.Vault
	idle  time.Duration

	mu         sync.Mutex
	sessions   map[string]time.Time // token -> last seen
	failures   []time.Time
	emptyTicks int
}

// New starts the janitor that expires idle browser sessions and locks the
// vault once nobody is signed in any more.
func New(v *vault.Vault, idle time.Duration) *Auth {
	a := &Auth{vault: v, idle: idle, sessions: map[string]time.Time{}}
	go a.janitor()
	return a
}

func (a *Auth) janitor() {
	for range time.Tick(30 * time.Second) {
		a.mu.Lock()
		now := time.Now()
		for tok, seen := range a.sessions {
			if now.Sub(seen) > a.idle {
				delete(a.sessions, tok)
			}
		}
		if len(a.sessions) == 0 {
			a.emptyTicks++
		} else {
			a.emptyTicks = 0
		}
		lock := a.emptyTicks >= 2 && a.vault.Unlocked()
		a.mu.Unlock()
		if lock {
			a.vault.Lock()
		}
	}
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// Login issues a browser session cookie. Call only after the passphrase has
// been verified.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	a.mu.Lock()
	a.sessions[tok] = time.Now()
	a.emptyTicks = 0
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r),
		MaxAge: int(a.idle.Seconds()),
	})
	return nil
}

// Authenticated reports whether the request carries a live session and the
// vault is unlocked. It slides the idle timeout.
func (a *Auth) Authenticated(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || !a.vault.Unlocked() {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	seen, ok := a.sessions[c.Value]
	if !ok || time.Since(seen) > a.idle {
		delete(a.sessions, c.Value)
		return false
	}
	a.sessions[c.Value] = time.Now()
	return true
}

// LockAll signs every browser out and drops the vault key from memory.
// Running SSH sessions are left alone.
func (a *Auth) LockAll(w http.ResponseWriter) {
	a.mu.Lock()
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	a.vault.Lock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// Require rejects unauthenticated requests.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authenticated(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Locked. Sign in again.","code":"locked"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

const (
	maxFailures   = 5
	failureWindow = 5 * time.Minute
)

// AllowAttempt reports whether a passphrase attempt may proceed, and if not,
// how long to wait.
func (a *Auth) AllowAttempt() (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-failureWindow)
	kept := a.failures[:0]
	for _, t := range a.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	a.failures = kept
	if len(a.failures) >= maxFailures {
		return false, time.Until(a.failures[0].Add(failureWindow))
	}
	return true, 0
}

func (a *Auth) RecordFailure() {
	a.mu.Lock()
	a.failures = append(a.failures, time.Now())
	a.mu.Unlock()
}

func (a *Auth) ClearFailures() {
	a.mu.Lock()
	a.failures = nil
	a.mu.Unlock()
}

// SameOrigin blocks cross-site state changes: every mutating request must
// carry the X-Cotta header (which a cross-site form cannot set) and, when the
// browser sends an Origin, it must match the host being addressed.
func SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("X-Cotta") == "" || !originMatches(r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func originMatches(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}
