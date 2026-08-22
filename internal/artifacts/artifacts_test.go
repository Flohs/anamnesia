package artifacts

import (
	"strings"
	"testing"
)

func TestParsePublishAcceptsOnlyAPublish(t *testing.T) {
	const url = "https://claude.ai/code/artifact/fdf66da0-8467-45a2-ab83-412ce9c32da0"
	tests := []struct {
		name     string
		response string
		want     bool
	}{
		{
			name:     "a publish",
			response: "Published /tmp/scratch/audit.html at " + url + "\n\nLive subscription: skipped",
			want:     true,
		},
		{
			// `action: "list"` returns a listing that names artifacts and
			// their URLs. Recording those would file every artifact the
			// user owns as if it had just been published.
			name:     "a listing",
			response: "Your artifacts:\n  Install Audit  " + url + "  (updated 2026-08-19)",
			want:     false,
		},
		{
			name:     "a refusal",
			response: "Publishing is not available for this page.",
			want:     false,
		},
		{
			name:     "a malformed id",
			response: "Published /tmp/x.html at https://claude.ai/code/artifact/not-a-uuid-at-all-really-no",
			want:     false,
		},
		{name: "empty", response: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParsePublish(tc.response)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			if got.URL != url {
				t.Errorf("url = %q, want %q", got.URL, url)
			}
			if got.FilePath != "/tmp/scratch/audit.html" {
				t.Errorf("file path = %q", got.FilePath)
			}
			if URLFor(got.UUID) != url {
				t.Errorf("uuid did not round-trip: %s", got.UUID)
			}
		})
	}
}

// The whole reason for a real HTML parser rather than a tag strip: an
// artifact carries its CSS and JS inline, and embedding those makes the
// page match everything and mean nothing.
func TestScriptAndStyleAreNotContent(t *testing.T) {
	const page = `<!doctype html><html><head><title>Install Audit</title>
	<style>:root{--ground:#08080b} .card{padding:2rem}</style>
	<script>const telemetry = {beacon: "flush", retries: 3};</script>
	</head><body><h1>Hook failures</h1>
	<p>Six of fifty-four session-end hooks failed.</p>
	<script>console.log("noise")</script></body></html>`

	title, text, err := FromHTML(strings.NewReader(page), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Install Audit" {
		t.Errorf("title = %q, want %q", title, "Install Audit")
	}
	for _, want := range []string{"Hook failures", "Six of fifty-four session-end hooks failed."} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"--ground", "padding", "telemetry", "console.log", "beacon"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("text carries markup %q:\n%s", unwanted, text)
		}
	}
	// The title is taken as the title, so it must not also be body text.
	if strings.Contains(text, "Install Audit") {
		t.Errorf("the title was repeated in the body text:\n%s", text)
	}
}

func TestExtractIsCappedOnARuneBoundary(t *testing.T) {
	page := "<html><body><p>" + strings.Repeat("wörter ", 500) + "</p></body></html>"
	_, text, err := FromHTML(strings.NewReader(page), 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > 64 {
		t.Errorf("text is %d bytes, want at most 64", len(text))
	}
	if !utf8Valid(text) {
		t.Errorf("the cap split a rune: %q", text)
	}
}

func TestMarkdownIsKeptAsItIs(t *testing.T) {
	got, err := FromMarkdown(strings.NewReader("# Title\n\nSome **bold** prose."), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Some **bold** prose.") {
		t.Errorf("markdown was mangled: %q", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
