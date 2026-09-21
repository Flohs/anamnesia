package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hashedAsset finds a built asset by walking the embedded bundle, because its
// name carries a content hash that changes on every meaningful edit.
func hashedAsset(t *testing.T, ext string) string {
	t.Helper()
	entries, err := fs.ReadDir(bundle, "dist/assets")
	if err != nil {
		t.Fatalf("reading the embedded assets: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ext) {
			return "/assets/" + e.Name()
		}
	}
	t.Fatalf("no %s asset in the embedded bundle", ext)
	return ""
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// An empty dist/ compiles perfectly well and ships a binary whose console is a
// blank page, so the embed is asserted directly rather than through a handler.
func TestEmbeddedBundleContainsTheApp(t *testing.T) {
	index, err := fs.ReadFile(bundle, "dist/index.html")
	if err != nil {
		t.Fatalf("no index.html in the embedded bundle: %v", err)
	}
	if !strings.Contains(string(index), `id="root"`) {
		t.Errorf("embedded index.html is not the console:\n%s", index)
	}
}

func TestServesTheAppAtRoot(t *testing.T) {
	rec := get(t, Handler(), "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Error("GET / did not return the console")
	}
}

func TestServesHashedAssets(t *testing.T) {
	path := hashedAsset(t, ".js")
	rec := get(t, Handler(), path)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a javascript type", ct)
	}
}

// The names are content hashes, so a stale copy can never be the wrong file.
func TestHashedAssetsAreCachedForever(t *testing.T) {
	rec := get(t, Handler(), hashedAsset(t, ".css"))

	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want it to mark the asset immutable", cc)
	}
}

// index.html names those hashed assets, so caching it is how a browser ends up
// asking for a bundle the binary no longer contains.
func TestTheAppItselfIsNotCached(t *testing.T) {
	rec := get(t, Handler(), "/")

	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want / to revalidate", cc)
	}
}

// The console routes client-side: a reload on /memory has to reach the app.
func TestDeepLinksServeTheApp(t *testing.T) {
	rec := get(t, Handler(), "/memory")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /memory = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Error("a deep link did not return the console")
	}
}

// Without this a typo'd endpoint answers 200 with a web page, which reads as
// a working call to anything that is not a browser.
func TestUnmatchedAPIPathsAre404(t *testing.T) {
	h := Handler("/v1/", "/api/", "/mcp")

	for _, path := range []string{"/v1/nope", "/api/v1/nope", "/mcp/nope"} {
		rec := get(t, h, path)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s returned the console rather than an error", path)
		}
	}
}
