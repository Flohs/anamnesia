// Package artifacts reads what Claude Code published.
//
// Two jobs, both deliberately deterministic. Recognising a publish from
// the Artifact tool's own response, and turning the published file into
// the text that gets embedded. Neither involves a model: a URL is not
// something to be judged worth remembering, and an identifier that is
// only probably right is not an identifier.
package artifacts

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/net/html"
)

// publishRE matches the Artifact tool's success line. The tool also
// serves `action: "list"`, whose response is a listing and must not be
// recorded, so the response shape is the gate rather than the tool name.
var publishRE = regexp.MustCompile(
	`Published\s+(\S+)\s+at\s+(https://claude\.ai/code/artifact/([0-9a-fA-F-]{36}))`)

// Publish is one recognised publish.
type Publish struct {
	FilePath string
	URL      string
	UUID     uuid.UUID
}

// ParsePublish reads the tool response. ok=false covers anything that is
// not a publish, which is a normal outcome rather than an error.
func ParsePublish(response string) (Publish, bool) {
	m := publishRE.FindStringSubmatch(response)
	if m == nil {
		return Publish{}, false
	}
	id, err := uuid.Parse(m[3])
	if err != nil {
		return Publish{}, false
	}
	return Publish{FilePath: m[1], URL: m[2], UUID: id}, true
}

// urlRE matches an artifact URL anywhere in a line of text.
var urlRE = regexp.MustCompile(`https://claude\.ai/code/artifact/[0-9a-fA-F-]{36}`)

// URLsIn finds every artifact URL mentioned in a string, whether or not
// it came from a publish. The backfill uses this to notice the ones it
// cannot describe, so they can be reported rather than dropped quietly.
func URLsIn(s string) []string {
	return urlRE.FindAllString(s, -1)
}

// UUIDFromURL pulls the id out of an artifact URL.
func UUIDFromURL(u string) (uuid.UUID, bool) {
	i := strings.LastIndex(u, "/")
	if i < 0 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(u[i+1:])
	return id, err == nil
}

// URLFor renders the canonical URL for an artifact id.
func URLFor(id uuid.UUID) string {
	return "https://claude.ai/code/artifact/" + id.String()
}

// markup are elements whose text is not content. An artifact is a
// self-contained page carrying its CSS and JS inline, so keeping these
// would fill the embedding with stylesheet and script tokens: the page
// would match on everything and mean nothing.
var markup = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true,
}

// FromHTML extracts the document title and its readable text, capped at
// max bytes. The cap is not a nicety: an artifact runs to hundreds of KB
// while the embedding model reads only the first few thousand tokens, so
// the rest costs storage and buys no recall.
func FromHTML(r io.Reader, max int) (title, text string, err error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", "", fmt.Errorf("parse html: %w", err)
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if markup[n.Data] {
				return
			}
			// Taken as the title, and so not also as body text.
			if n.Data == "title" {
				if title == "" && n.FirstChild != nil {
					title = strings.TrimSpace(n.FirstChild.Data)
				}
				return
			}
		}
		if n.Type == html.TextNode && b.Len() < max {
			if s := strings.TrimSpace(n.Data); s != "" {
				b.WriteString(s)
				b.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, truncate(collapse(b.String()), max), nil
}

// FromMarkdown keeps a markdown artifact as it is, capped the same way.
// There is no markup to strip that is not also content.
func FromMarkdown(r io.Reader, max int) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(max)*4))
	if err != nil {
		return "", err
	}
	return truncate(collapse(string(raw)), max), nil
}

// collapse squeezes runs of whitespace to single spaces.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncate cuts to max bytes without splitting a rune.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut)
}
