package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testDeps(token string) Deps {
	return Deps{ServerToken: token, console: &consoleAuth{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func serverFor(t *testing.T, token string) http.Handler {
	t.Helper()
	return NewServer("127.0.0.1:0", testDeps(token)).Handler
}

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ── the console is served by the API server itself ──────────────────

func TestConsoleIsServedAtRoot(t *testing.T) {
	rec := do(t, serverFor(t, ""), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Error("GET / did not return the console")
	}
}

// The console calls /api/v1/..., which the Node proxy used to rewrite. The
// prefix is kept so `make dev` and the binary present the same shape.
func TestAPIIsAlsoReachableUnderTheAPIPrefix(t *testing.T) {
	h := serverFor(t, "")

	direct := do(t, h, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	prefixed := do(t, h, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if direct.Code != prefixed.Code {
		t.Fatalf("/v1/health = %d but /api/v1/health = %d", direct.Code, prefixed.Code)
	}
	if direct.Body.String() != prefixed.Body.String() {
		t.Errorf("bodies differ:\n direct: %s\nprefixed: %s", direct.Body, prefixed.Body)
	}
}

func TestUnknownAPIPathIsNotAnswredWithTheConsole(t *testing.T) {
	h := serverFor(t, "")

	for _, path := range []string{"/v1/nope", "/api/v1/nope"} {
		rec := do(t, h, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s returned the console", path)
		}
	}
}

// ── the one-time link ───────────────────────────────────────────────

func mintNonce(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/console/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := do(t, h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/console/session = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the session response: %v", err)
	}
	if got.Nonce == "" {
		t.Fatal("no nonce in the session response")
	}
	return got.Nonce
}

func TestMintingALinkNeedsTheServerToken(t *testing.T) {
	rec := do(t, serverFor(t, "s3cret"), httptest.NewRequest(http.MethodPost, "/v1/console/session", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /v1/console/session without a token = %d, want 401", rec.Code)
	}
}

func TestOpeningTheLinkSetsASessionCookie(t *testing.T) {
	h := serverFor(t, "s3cret")
	nonce := mintNonce(t, h, "s3cret")

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/?k="+nonce, nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /?k=... = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / so the nonce leaves the address bar", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	if !cookies[0].HttpOnly {
		t.Error("the session cookie is readable by browser JavaScript")
	}
	if cookies[0].SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict")
	}
}

// A link in shell history or a terminal scrollback must not still work.
func TestALinkWorksOnlyOnce(t *testing.T) {
	h := serverFor(t, "s3cret")
	nonce := mintNonce(t, h, "s3cret")

	first := do(t, h, httptest.NewRequest(http.MethodGet, "/?k="+nonce, nil))
	second := do(t, h, httptest.NewRequest(http.MethodGet, "/?k="+nonce, nil))

	if first.Code != http.StatusSeeOther {
		t.Fatalf("first use = %d, want 303", first.Code)
	}
	if second.Code != http.StatusUnauthorized {
		t.Errorf("second use = %d, want 401", second.Code)
	}
}

func TestAnUnknownLinkNamesTheFix(t *testing.T) {
	rec := do(t, serverFor(t, "s3cret"), httptest.NewRequest(http.MethodGet, "/?k=made-up", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /?k=made-up = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "anamnesia ui") {
		t.Errorf("the error does not say how to get a working link: %s", rec.Body)
	}
}

// ── what the cookie buys ────────────────────────────────────────────

func TestProtectAcceptsABearerOrASessionCookie(t *testing.T) {
	d := testDeps("s3cret")
	session := d.console.open(time.Now())
	guarded := d.protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name    string
		prepare func(*http.Request)
		want    int
	}{
		{"nothing", func(*http.Request) {}, http.StatusUnauthorized},
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret") }, http.StatusOK},
		{"cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: consoleCookie, Value: session})
		}, http.StatusOK},
		{"forged cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: consoleCookie, Value: "made-up"})
		}, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
			c.prepare(req)
			if rec := do(t, guarded, req); rec.Code != c.want {
				t.Errorf("got %d, want %d", rec.Code, c.want)
			}
		})
	}
}

// Without a token nothing is protected, so the console needs no session and
// asking for one would be a wall in front of an open door.
func TestTheConsoleNeedsNoSessionWithoutAToken(t *testing.T) {
	rec := do(t, serverFor(t, ""), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("GET / = %d, want 200", rec.Code)
	}
}

// Serving the app to someone with no session would render a console whose
// every call 401s, and whose error would not say what to do about it.
func TestTheConsoleRefusesWithoutASessionWhenATokenIsSet(t *testing.T) {
	rec := do(t, serverFor(t, "s3cret"), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET / = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "anamnesia ui") {
		t.Errorf("the error does not name the fix: %s", rec.Body)
	}
}

func TestTheConsoleOpensWithASessionCookie(t *testing.T) {
	d := testDeps("s3cret")
	session := d.console.open(time.Now())
	h := NewServer("127.0.0.1:0", d).Handler

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: consoleCookie, Value: session})

	if rec := do(t, h, req); rec.Code != http.StatusOK {
		t.Errorf("GET / with a session = %d, want 200", rec.Code)
	}
}

// ── expiry ──────────────────────────────────────────────────────────

func TestLinksAndSessionsExpire(t *testing.T) {
	var c consoleAuth
	start := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	t.Run("a link expires", func(t *testing.T) {
		nonce := c.mint(start)
		if _, ok := c.redeem(nonce, start.Add(nonceTTL+time.Second)); ok {
			t.Error("an expired link was accepted")
		}
	})

	t.Run("a session expires", func(t *testing.T) {
		session := c.open(start)
		if !c.valid(session, start.Add(sessionTTL-time.Minute)) {
			t.Error("a live session was rejected")
		}
		if c.valid(session, start.Add(sessionTTL+time.Second)) {
			t.Error("an expired session was accepted")
		}
	})
}

// The /api/ route re-enters the mux, so each nested prefix in a crafted URL
// would be another stack frame. A request line can carry ~200k of them, and
// routing happens before protect, so an unauthenticated caller must not be
// able to choose how deep the server recurses.
func TestNestedAPIPrefixesDoNotNest(t *testing.T) {
	h := serverFor(t, "")

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/api/api/v1/health", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/api/v1/health = %d, want 404: the prefix is being stripped more than once", rec.Code)
	}
}
