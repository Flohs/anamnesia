package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigListsSettingsWithSecretsMasked(t *testing.T) {
	srv := testServer(t, Deps{Config: []ConfigItem{
		{Key: "llm.provider", Value: "openrouter", Source: "global"},
		{Key: "openrouter.api_key", Value: "••••1234", Source: "global", Secret: true},
	}})

	var got struct {
		Items []ConfigItem `json:"items"`
	}
	if code := getJSON(t, srv.URL+"/v1/config", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %v, want two", got.Items)
	}
	if got.Items[0].Key != "llm.provider" || got.Items[0].Value != "openrouter" {
		t.Errorf("item = %+v", got.Items[0])
	}
	secret := got.Items[1]
	if !secret.Secret || secret.Value != "••••1234" {
		t.Errorf("secret item = %+v, want it flagged and already masked", secret)
	}
}

func TestHooksReadsTheLogNewestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.log")
	lines := `{"at":"2026-08-18T09:00:00Z","verb":"session-start","ok":true,"ms":120,"note":"12 facts"}
{"at":"2026-08-18T09:05:00Z","verb":"retrieve","ok":false,"ms":796,"error":"server unreachable"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, Deps{HookLogPath: path})

	var got struct {
		Path  string         `json:"path"`
		Items []HookLogEntry `json:"items"`
	}
	if code := getJSON(t, srv.URL+"/v1/hooks", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Path != path {
		t.Errorf("path = %q, want %q", got.Path, path)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %v, want two", got.Items)
	}
	if got.Items[0].Verb != "retrieve" || got.Items[0].OK {
		t.Errorf("first item = %+v, want the newest run, which failed", got.Items[0])
	}
	if got.Items[0].Error != "server unreachable" {
		t.Errorf("error = %q, want the reason the hook failed", got.Items[0].Error)
	}
}

func TestHooksWithNoLogYetIsAnEmptyList(t *testing.T) {
	// No hook has run since installation. That is a state the UI has to
	// be able to describe, so it is an empty list rather than an error.
	srv := testServer(t, Deps{HookLogPath: filepath.Join(t.TempDir(), "absent.log")})
	var got struct {
		Items []HookLogEntry `json:"items"`
	}
	if code := getJSON(t, srv.URL+"/v1/hooks", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Items) != 0 {
		t.Errorf("items = %v, want none", got.Items)
	}
}

func writeHookLog(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.log")
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"at":"2026-08-18T09:00:00Z","verb":"retrieve","ok":true,"ms":%d}`+"\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hookCount(t *testing.T, url string) int {
	t.Helper()
	var got struct {
		Items []HookLogEntry `json:"items"`
	}
	if code := getJSON(t, url, &got); code != http.StatusOK {
		t.Fatalf("status = %d for %s, want 200", code, url)
	}
	return len(got.Items)
}

func TestHooksHonoursLimit(t *testing.T) {
	// Every other list endpoint takes a limit. A panel that wants the
	// last handful should not be sent two hundred entries to throw away.
	srv := testServer(t, Deps{HookLogPath: writeHookLog(t, 40)})
	if got := hookCount(t, srv.URL+"/v1/hooks?limit=5"); got != 5 {
		t.Errorf("limit=5 returned %d entries", got)
	}
	if got := hookCount(t, srv.URL+"/v1/hooks?limit=100"); got != 40 {
		t.Errorf("limit=100 returned %d entries, want the whole 40-entry log", got)
	}
	if got := hookCount(t, srv.URL+"/v1/hooks"); got != 40 {
		t.Errorf("no limit returned %d entries, want the whole log up to the cap", got)
	}
}

func TestHooksLimitIsCappedAtTheTail(t *testing.T) {
	// The tail is what bounds the response. A caller asking for more than
	// the server is willing to read gets the tail, not an error.
	srv := testServer(t, Deps{HookLogPath: writeHookLog(t, hookLogTail+50)})
	if got := hookCount(t, srv.URL+"/v1/hooks?limit=100000"); got != hookLogTail {
		t.Errorf("limit=100000 returned %d entries, want the %d-entry tail", got, hookLogTail)
	}
}

func TestHooksRejectsAnUnusableLimit(t *testing.T) {
	// browse.go rejects these rather than quietly picking a number, and
	// this route should not be the one that disagrees.
	srv := testServer(t, Deps{HookLogPath: writeHookLog(t, 5)})
	for _, v := range []string{"0", "-3", "many"} {
		if code := getJSON(t, srv.URL+"/v1/hooks?limit="+v, nil); code != http.StatusBadRequest {
			t.Errorf("limit=%s gave %d, want 400", v, code)
		}
	}
}
