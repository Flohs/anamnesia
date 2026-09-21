package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const (
	consoleCookie = "anamnesia_console"
	// Long enough to survive a browser launch, or a copy and paste out of
	// `anamnesia ui --no-open` over SSH; short enough that a link left in
	// shell history is not a standing key.
	nonceTTL = 5 * time.Minute
	// A working day at the console without re-running the command.
	sessionTTL = 12 * time.Hour
)

// consoleAuth gives the console a way to authenticate that a browser can
// actually use.
//
// The activity feed is an EventSource, and EventSource cannot set an
// Authorization header, so a token held in page JavaScript would leave the
// live feed 401ing while everything else worked. A cookie rides every
// request including the stream, and HttpOnly keeps it out of the page's
// reach, which is the property the Node proxy provided by holding the
// server token itself.
//
// Both tables are in memory. A session that does not survive a restart is
// correct here: `anamnesia ui` mints another in the time a browser takes to
// open.
type consoleAuth struct {
	mu       sync.Mutex
	nonces   map[string]time.Time // one-time links; value is expiry
	sessions map[string]time.Time
}

func newSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("httpapi: no randomness available: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// mint issues a single-use link token.
func (c *consoleAuth) mint(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(now)
	if c.nonces == nil {
		c.nonces = map[string]time.Time{}
	}
	n := newSecret()
	c.nonces[n] = now.Add(nonceTTL)
	return n
}

// redeem spends a link token and returns the session it buys. The token is
// consumed whether or not it was still valid, because a replay is not a retry.
func (c *consoleAuth) redeem(nonce string, now time.Time) (string, bool) {
	c.mu.Lock()
	expiry, ok := c.nonces[nonce]
	delete(c.nonces, nonce)
	c.mu.Unlock()

	if !ok || now.After(expiry) {
		return "", false
	}
	return c.open(now), true
}

// open starts a session without a link, which is what happens when the
// caller has already proved it holds the server token.
func (c *consoleAuth) open(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(now)
	if c.sessions == nil {
		c.sessions = map[string]time.Time{}
	}
	s := newSecret()
	c.sessions[s] = now.Add(sessionTTL)
	return s
}

func (c *consoleAuth) valid(session string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry, ok := c.sessions[session]
	return ok && !now.After(expiry)
}

// sweep drops what has expired, so a server left running for months does not
// accumulate every link it ever issued. Callers hold the lock.
func (c *consoleAuth) sweep(now time.Time) {
	for k, expiry := range c.nonces {
		if now.After(expiry) {
			delete(c.nonces, k)
		}
	}
	for k, expiry := range c.sessions {
		if now.After(expiry) {
			delete(c.sessions, k)
		}
	}
}

// handleConsoleSession mints the one-time link `anamnesia ui` opens the
// browser with. It sits behind protect, so holding the server token is what
// buys a console session.
func (d Deps) handleConsoleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"nonce": d.console.mint(time.Now())})
}

// consoleGate fronts the embedded console with the session exchange.
//
// Three cases in order: a fresh link is spent for a cookie and redirected so
// the nonce leaves the address bar and the browser's history; a request
// already carrying a session is served; anything else is refused with the
// command that fixes it, rather than with a console whose every call 401s.
//
// With no server token there is nothing to authenticate to, so the gate is
// not a gate.
func (d Deps) consoleGate(next http.Handler) http.Handler {
	if d.ServerToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if nonce := r.URL.Query().Get("k"); nonce != "" {
			session, ok := d.console.redeem(nonce, time.Now())
			if !ok {
				http.Error(w, "This console link is expired or already used. Run `anamnesia ui` for a new one.", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     consoleCookie,
				Value:    session,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				Secure:   r.TLS != nil,
				MaxAge:   int(sessionTTL / time.Second),
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if !d.hasConsoleSession(r) {
			http.Error(w, "The console needs a session. Run `anamnesia ui` to open it.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d Deps) hasConsoleSession(r *http.Request) bool {
	if d.console == nil {
		return false
	}
	cookie, err := r.Cookie(consoleCookie)
	return err == nil && d.console.valid(cookie.Value, time.Now())
}
