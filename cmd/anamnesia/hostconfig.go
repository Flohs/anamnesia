// hostconfig.go resolves the effective configuration for every host-side
// command, and renders the server's environment from it.
//
// Precedence, highest first, and this is the order the code actually
// applies (the previous implementation documented one order and used
// another, so a project file silently beat an environment variable and a
// global file silently beat a project file):
//
//  1. command-line flags
//  2. environment variables
//  3. ./.anamnesia.toml         (project, nearest git root)
//  4. ~/.anamnesia/config.toml  (global)
//  5. built-in defaults
//
// Reading uses a real TOML parser. Writing edits the file line by line so
// the comments a user reads to understand their own config survive a
// `config set`.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// origin records where a resolved value came from, for `config list`.
type origin string

const (
	fromDefault origin = "default"
	fromGlobal  origin = "global"
	fromProject origin = "project"
	fromEnv     origin = "env"
	fromFlag    origin = "flag"
)

// hostConfig is the resolved view of all settings.
type hostConfig struct {
	values  map[string]string
	origins map[string]origin

	// GlobalPath and ProjectPath record which files were consulted.
	GlobalPath  string
	ProjectPath string // empty when no project file applies

	// Unknown lists dotted keys found in a config file that Anamnesia does
	// not recognise. Reported rather than ignored, because a typo'd key is
	// otherwise indistinguishable from a setting that does not work.
	Unknown []string
}

// loadHostConfig resolves configuration from every source.
func loadHostConfig() (*hostConfig, error) {
	hc := &hostConfig{
		values:  make(map[string]string, len(settings)),
		origins: make(map[string]origin, len(settings)),
	}
	for _, s := range settings {
		hc.values[s.Key] = s.Def
		hc.origins[s.Key] = fromDefault
	}

	gp, err := globalConfigPath()
	if err != nil {
		return nil, err
	}
	hc.GlobalPath = gp
	if err := hc.overlayFile(gp, fromGlobal); err != nil {
		return nil, err
	}

	if pp := findProjectConfig(); pp != "" {
		hc.ProjectPath = pp
		if err := hc.overlayFile(pp, fromProject); err != nil {
			return nil, err
		}
	}

	// Environment beats both files: it is the one-off override.
	for _, s := range settings {
		if s.Env == "" {
			continue
		}
		if v, ok := os.LookupEnv(s.Env); ok && strings.TrimSpace(v) != "" {
			norm, err := s.validate(v)
			if err != nil {
				return nil, fmt.Errorf("environment %s: %w", s.Env, err)
			}
			hc.values[s.Key] = norm
			hc.origins[s.Key] = fromEnv
		}
	}
	// ANAMNESIA_URL is the historical name for the server location and
	// stays supported; it has no setting of its own because server.url is
	// derived rather than stored by default.
	if v := strings.TrimSpace(os.Getenv("ANAMNESIA_URL")); v != "" {
		hc.values["server.url"] = v
		hc.origins["server.url"] = fromEnv
	}
	if v := strings.TrimSpace(os.Getenv("ANAMNESIA_USER")); v != "" {
		hc.values["identity.user"] = v
		hc.origins["identity.user"] = fromEnv
	}

	// Flags win.
	if rf.serverURL != "" {
		hc.values["server.url"] = rf.serverURL
		hc.origins["server.url"] = fromFlag
	}
	if rf.user != "" {
		hc.values["identity.user"] = rf.user
		hc.origins["identity.user"] = fromFlag
	}
	return hc, nil
}

// overlayFile merges one TOML file over the current values.
func (hc *hostConfig) overlayFile(path string, src origin) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var tree map[string]any
	if err := toml.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("%s is not valid TOML: %w", path, err)
	}
	for section, body := range tree {
		table, ok := body.(map[string]any)
		if !ok {
			// A bare top-level key. The old format allowed
			// `project = "x"`, so keep understanding it.
			if section == "project" {
				hc.values["identity.project"] = fmt.Sprint(body)
				hc.origins["identity.project"] = src
				continue
			}
			hc.Unknown = append(hc.Unknown, fmt.Sprintf("%s (in %s)", section, path))
			continue
		}
		for name, v := range table {
			key := section + "." + name
			s, known := settingByKey[key]
			if !known {
				hc.Unknown = append(hc.Unknown, fmt.Sprintf("%s (in %s)", key, path))
				continue
			}
			norm, err := s.validate(scalarToString(v))
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if norm == "" && s.Def != "" {
				// An explicitly blank value means "use the default".
				continue
			}
			hc.values[key] = norm
			hc.origins[key] = src
		}
	}
	return nil
}

// scalarToString renders a TOML scalar. Non-scalars are rejected by
// validate() further up, with the key named.
func scalarToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// ─── accessors ───────────────────────────────────────────────────────

func (hc *hostConfig) Get(key string) string    { return hc.values[key] }
func (hc *hostConfig) Origin(key string) origin { return hc.origins[key] }
func (hc *hostConfig) Bool(key string) bool     { b, _ := strconv.ParseBool(hc.values[key]); return b }
func (hc *hostConfig) Int(key string) int       { n, _ := strconv.Atoi(hc.values[key]); return n }
func (hc *hostConfig) Dur(key string) time.Duration {
	d, _ := time.ParseDuration(hc.values[key])
	return d
}

// ServerURL is where the CLI and the hooks reach the server. Derived from
// server.addr unless explicitly set, so moving the port cannot leave the
// hooks pointing at the old one.
func (hc *hostConfig) ServerURL() string {
	if u := strings.TrimSpace(hc.values["server.url"]); u != "" {
		return strings.TrimRight(u, "/")
	}
	addr := strings.TrimSpace(hc.values["server.addr"])
	if addr == "" {
		addr = "127.0.0.1:8181"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	// A server listening on all interfaces is still reached over loopback.
	addr = strings.Replace(addr, "0.0.0.0:", "127.0.0.1:", 1)
	return "http://" + addr
}

// ManagesPostgres reports whether Anamnesia owns a Postgres container.
// False when the user pointed postgres.url at their own database.
func (hc *hostConfig) ManagesPostgres() bool {
	return strings.TrimSpace(hc.values["postgres.url"]) == ""
}

// DatabaseURL is the DSN the server connects with.
func (hc *hostConfig) DatabaseURL() string {
	// An explicit environment variable wins, matching the documented
	// precedence and keeping one-off runs against another database simple.
	if u := strings.TrimSpace(os.Getenv("ANAMNESIA_DATABASE_URL")); u != "" {
		return u
	}
	if u := strings.TrimSpace(hc.values["postgres.url"]); u != "" {
		return u
	}
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable",
		hc.values["postgres.user"],
		hc.values["postgres.password"],
		hc.Int("postgres.port"),
		hc.values["postgres.database"],
	)
}

// User is the memory owner for host-side calls.
func (hc *hostConfig) User() string {
	if u := strings.TrimSpace(hc.values["identity.user"]); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return sanitizeSlug(u)
	}
	return "default"
}

// Project is the slug for the current working directory.
func (hc *hostConfig) Project() string {
	if rf.project != "" {
		return sanitizeSlug(rf.project)
	}
	if p := strings.TrimSpace(os.Getenv("ANAMNESIA_PROJECT")); p != "" {
		return sanitizeSlug(p)
	}
	if p := strings.TrimSpace(hc.values["identity.project"]); p != "" {
		return sanitizeSlug(p)
	}
	return deriveProjectFromCWD()
}

// Token is the optional shared secret.
func (hc *hostConfig) Token() string { return hc.values["server.token"] }

// ServerEnv renders the environment for the server process. Only settings
// with an Env mapping are passed, plus the derived database URL, so the
// server can never see a host-only setting and drift from it.
func (hc *hostConfig) ServerEnv() []string {
	env := os.Environ()
	set := func(k, v string) { env = append(env, k+"="+v) }

	set("ANAMNESIA_DATABASE_URL", hc.DatabaseURL())
	set("ANAMNESIA_DEFAULT_USER", hc.User())
	for _, s := range settings {
		if s.Env == "" || s.Env == "ANAMNESIA_DEFAULT_USER" {
			continue
		}
		// Empty means "let the server apply its own default", which is how
		// provider auto-selection stays in one place.
		if v := hc.values[s.Key]; v != "" {
			set(s.Env, v)
		}
	}
	return env
}

// ─── file generation and editing ─────────────────────────────────────

// renderDefaultConfig produces a fully commented config file. Values that
// differ from the built-in default are carried over, so regenerating never
// loses what the user already set.
func renderDefaultConfig(existing map[string]string) []byte {
	var b strings.Builder
	b.WriteString("# Anamnesia configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# Edit this file directly, or use the CLI:\n")
	b.WriteString("#   anamnesia config set openrouter.api_key sk-or-v1-...\n")
	b.WriteString("#   anamnesia config list\n")
	b.WriteString("#\n")
	b.WriteString("# Anything left empty falls back to the documented default.\n")
	b.WriteString("# Environment variables override this file; flags override those.\n")

	bySection := map[string][]setting{}
	for _, s := range settings {
		bySection[s.section()] = append(bySection[s.section()], s)
	}
	for _, sec := range sectionOrder() {
		b.WriteString("\n[" + sec + "]\n")
		for _, s := range bySection[sec] {
			if s.Doc != "" {
				for _, line := range wrapComment(s.Doc, 72) {
					b.WriteString("# " + line + "\n")
				}
			}
			v := s.Def
			if existing != nil {
				if got, ok := existing[s.Key]; ok {
					v = got
				}
			}
			b.WriteString(fmt.Sprintf("%s = %q\n", s.name(), v))
		}
	}
	return []byte(b.String())
}

// wrapComment breaks doc text into comment-width lines.
func wrapComment(text string, width int) []string {
	words := strings.Fields(text)
	var lines []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// generatePassword returns a URL-safe random secret for the database.
func generatePassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// keyLineRE matches `key = value`, capturing leading space and the key.
var keyLineRE = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+)(\s*)=`)

// sectionLineRE matches a `[section]` header.
var sectionLineRE = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)

// setConfigValue writes one key into a TOML file, preserving comments,
// ordering and unrelated content. The key is replaced in place when it
// exists, appended to its section when the section exists, and a new
// section is added at the end otherwise.
func setConfigValue(path, key, value string) error {
	s, ok := settingByKey[key]
	if !ok {
		return fmt.Errorf("unknown setting %q\n\nknown settings:\n  %s",
			key, strings.Join(knownKeys(), "\n  "))
	}
	norm, err := s.validate(value)
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := []string{}
	if len(raw) > 0 {
		lines = strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}

	newLine := fmt.Sprintf("%s = %q", s.name(), norm)
	wantSection := s.section()

	cur := ""
	inSection := false
	lastKeyInSection := -1
	sectionSeen := false
	for i, line := range lines {
		if m := sectionLineRE.FindStringSubmatch(line); m != nil {
			if inSection {
				// Left the target section without finding the key.
				break
			}
			cur = m[1]
			inSection = cur == wantSection
			if inSection {
				sectionSeen = true
				lastKeyInSection = i
			}
			continue
		}
		if !inSection {
			continue
		}
		if m := keyLineRE.FindStringSubmatch(line); m != nil {
			lastKeyInSection = i
			if m[2] == s.name() {
				lines[i] = newLine
				return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"))
			}
		}
	}

	switch {
	case sectionSeen && lastKeyInSection >= 0:
		out := append([]string{}, lines[:lastKeyInSection+1]...)
		out = append(out, newLine)
		out = append(out, lines[lastKeyInSection+1:]...)
		lines = out
	default:
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "["+wantSection+"]", newLine)
	}
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// ─── project helpers ─────────────────────────────────────────────────

// findProjectConfig returns the nearest .anamnesia.toml, preferring the
// git root so every directory in a repository resolves the same project.
func findProjectConfig() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if root := gitToplevelOrCWD(wd); root != "" {
		if p := filepath.Join(root, projectConfigName); fileExists(p) {
			return p
		}
	}
	if p := filepath.Join(wd, projectConfigName); fileExists(p) {
		return p
	}
	return ""
}

func gitToplevelOrCWD(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return cwd
	}
	return strings.TrimSpace(string(out))
}

func deriveProjectFromCWD() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	top := gitToplevelOrCWD(wd)
	if top == "" {
		top = wd
	}
	return sanitizeSlug(filepath.Base(top))
}

var slugRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizeSlug normalises a string into a stable project slug.
func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRE.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
