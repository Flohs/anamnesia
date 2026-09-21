package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestBrowserCommandPerPlatform(t *testing.T) {
	cases := map[string][]string{
		"darwin":  {"open", "http://x"},
		"linux":   {"xdg-open", "http://x"},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", "http://x"},
	}
	for goos, want := range cases {
		t.Run(goos, func(t *testing.T) {
			if got := browserCommand(goos, "http://x"); !reflect.DeepEqual(got, want) {
				t.Errorf("browserCommand(%q) = %v, want %v", goos, got, want)
			}
		})
	}
}

// Nothing is protected without a token, so asking the server for a link
// would be a round trip to be let through a door that is already open.
func TestConsoleLinkWithoutATokenIsTheBareURL(t *testing.T) {
	got, err := consoleLink(context.Background(), "http://127.0.0.1:8181", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:8181"; got != want {
		t.Errorf("consoleLink = %q, want %q", got, want)
	}
}

func TestConsoleLinkMintsAOneTimeNonce(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"nonce": "one-time"})
	}))
	defer srv.Close()

	got, err := consoleLink(context.Background(), srv.URL, "s3cret")
	if err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/console/session" {
		t.Errorf("asked for %s %s, want POST /v1/console/session", gotMethod, gotPath)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the server token", gotAuth)
	}
	if want := srv.URL + "/?k=one-time"; got != want {
		t.Errorf("consoleLink = %q, want %q", got, want)
	}
}

func TestConsoleLinkReportsARefusedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorised", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := consoleLink(context.Background(), srv.URL, "wrong")
	if err == nil {
		t.Fatal("a refused token was reported as success")
	}
	if !strings.Contains(err.Error(), "server.token") {
		t.Errorf("error does not name the setting to check: %v", err)
	}
}

// An older server has no console at all, and "404" on its own does not say so.
func TestConsoleLinkReportsAServerWithoutAConsole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := consoleLink(context.Background(), srv.URL, "s3cret")
	if err == nil {
		t.Fatal("a server with no console endpoint was reported as success")
	}
	if !strings.Contains(err.Error(), "anamnesia update") {
		t.Errorf("error does not suggest upgrading the server: %v", err)
	}
}
