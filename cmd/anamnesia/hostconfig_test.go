package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedHome points the config system at a scratch directory and moves
// the working directory somewhere without a project config, so the
// developer's own repository cannot influence a test.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(homeEnv, home)
	t.Chdir(t.TempDir())
	rf.serverURL, rf.user, rf.project = "", "", ""
	return home
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDefaultsApply(t *testing.T) {
	isolatedHome(t)
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Get("embed.dims"); got != "1536" {
		t.Errorf("embed.dims default = %q, want 1536", got)
	}
	if got := hc.ServerURL(); got != "http://127.0.0.1:8181" {
		t.Errorf("ServerURL default = %q", got)
	}
	if !hc.ManagesPostgres() {
		t.Error("expected to manage postgres by default")
	}
}

// TestEnvBeatsFile locks in the documented precedence. The old code applied
// files after the environment, so a project file silently overrode an
// environment variable.
func TestEnvBeatsFile(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[llm]\nprovider = \"anthropic\"\n")
	t.Setenv("ANAMNESIA_LLM_PROVIDER", "openai")

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Get("llm.provider"); got != "openai" {
		t.Errorf("llm.provider = %q, want openai (env must beat the file)", got)
	}
	if got := hc.Origin("llm.provider"); got != fromEnv {
		t.Errorf("origin = %q, want env", got)
	}
}

// TestProjectBeatsGlobal locks in the other half of the inversion: the
// global file used to be merged last and therefore won.
func TestProjectBeatsGlobal(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[identity]\nuser = \"global-user\"\n")

	project := t.TempDir()
	writeConfig(t, filepath.Join(project, projectConfigName), "[identity]\nuser = \"project-user\"\n")
	t.Chdir(project)

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.User(); got != "project-user" {
		t.Errorf("User() = %q, want project-user (project must beat global)", got)
	}
}

func TestFlagBeatsEverything(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[server]\nurl = \"http://from-file:1\"\n")
	t.Setenv("ANAMNESIA_URL", "http://from-env:2")
	rf.serverURL = "http://from-flag:3"
	t.Cleanup(func() { rf.serverURL = "" })

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.ServerURL(); got != "http://from-flag:3" {
		t.Errorf("ServerURL() = %q, want the flag value", got)
	}
}

// TestBadValueIsReported is the fix for silent fallbacks: a typo used to be
// swallowed and replaced by the default.
func TestBadValueIsReported(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[embed]\ndims = \"not-a-number\"\n")

	_, err := loadHostConfig()
	if err == nil {
		t.Fatal("expected an error for a non-numeric embed.dims")
	}
	if !strings.Contains(err.Error(), "embed.dims") {
		t.Errorf("error does not name the setting: %v", err)
	}
}

func TestBadDurationIsReported(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[worker]\nextract_every = \"soon\"\n")
	if _, err := loadHostConfig(); err == nil {
		t.Fatal("expected an error for a non-duration worker.extract_every")
	}
}

func TestUnknownKeysAreReported(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[llm]\nprovidor = \"openai\"\n")

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(hc.Unknown) == 0 {
		t.Fatal("a misspelled key was silently ignored")
	}
	if !strings.Contains(hc.Unknown[0], "llm.providor") {
		t.Errorf("unknown key not named: %v", hc.Unknown)
	}
}

// TestSetConfigValuePreservesComments matters because the file is meant to
// be read and hand-edited: rewriting it from a struct would strip the
// documentation that makes it usable.
func TestSetConfigValuePreservesComments(t *testing.T) {
	home := isolatedHome(t)
	path := filepath.Join(home, "config.toml")
	body := "# top comment\n\n[llm]\n# which model runs extraction\nprovider = \"stub\"\nmodel = \"\"\n\n[embed]\ndims = \"1536\"\n"
	writeConfig(t, path, body)

	if err := setConfigValue(path, "llm.provider", "openrouter"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{"# top comment", "# which model runs extraction", `provider = "openrouter"`, `dims = "1536"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q after set:\n%s", want, got)
		}
	}
	if strings.Contains(got, `provider = "stub"`) {
		t.Errorf("old value survived:\n%s", got)
	}
}

func TestSetConfigValueAddsMissingSectionAndKey(t *testing.T) {
	home := isolatedHome(t)
	path := filepath.Join(home, "config.toml")
	writeConfig(t, path, "[llm]\nprovider = \"stub\"\n")

	if err := setConfigValue(path, "openrouter.api_key", "sk-or-test"); err != nil {
		t.Fatal(err)
	}
	if err := setConfigValue(path, "llm.model", "anthropic/claude-sonnet-4.6"); err != nil {
		t.Fatal(err)
	}
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Get("openrouter.api_key"); got != "sk-or-test" {
		t.Errorf("api_key = %q", got)
	}
	if got := hc.Get("llm.model"); got != "anthropic/claude-sonnet-4.6" {
		t.Errorf("llm.model = %q", got)
	}
	if got := hc.Get("llm.provider"); got != "stub" {
		t.Errorf("llm.provider changed to %q", got)
	}
}

func TestSetConfigValueRejectsUnknownAndInvalid(t *testing.T) {
	home := isolatedHome(t)
	path := filepath.Join(home, "config.toml")

	if err := setConfigValue(path, "llm.providor", "openai"); err == nil {
		t.Error("expected an error for an unknown key")
	}
	if err := setConfigValue(path, "embed.dims", "many"); err == nil {
		t.Error("expected an error for a non-numeric dims")
	}
	if err := setConfigValue(path, "llm.provider", "gemini"); err == nil {
		t.Error("expected an error for a value outside the enum")
	}
}

// TestGeneratedConfigIsValid closes the loop: the file setup writes must be
// readable by the loader, with no unknown keys.
func TestGeneratedConfigIsValid(t *testing.T) {
	home := isolatedHome(t)
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, renderDefaultConfig(map[string]string{"postgres.password": "secret"}), 0o600); err != nil {
		t.Fatal(err)
	}
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if len(hc.Unknown) > 0 {
		t.Errorf("generated config contains keys the loader rejects: %v", hc.Unknown)
	}
	if got := hc.Get("postgres.password"); got != "secret" {
		t.Errorf("seeded password not carried through: %q", got)
	}
	for _, s := range settings {
		if s.Def == "" {
			continue
		}
		if got := hc.Get(s.Key); got != s.Def && s.Key != "postgres.password" {
			t.Errorf("%s = %q, want the default %q", s.Key, got, s.Def)
		}
	}
}

// TestServerEnvCarriesDerivedValues checks the bridge into the server: only
// mapped settings travel, and the DSN is composed from postgres.*.
func TestServerEnvCarriesDerivedValues(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"),
		"[postgres]\nuser = \"pg\"\npassword = \"pw\"\nport = \"6000\"\ndatabase = \"db\"\n[llm]\nprovider = \"openai\"\n")

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range hc.ServerEnv() {
		if name, value, ok := strings.Cut(kv, "="); ok {
			env[name] = value
		}
	}
	if want := "postgres://pg:pw@127.0.0.1:6000/db?sslmode=disable"; env["ANAMNESIA_DATABASE_URL"] != want {
		t.Errorf("DSN = %q, want %q", env["ANAMNESIA_DATABASE_URL"], want)
	}
	if env["ANAMNESIA_LLM_PROVIDER"] != "openai" {
		t.Errorf("ANAMNESIA_LLM_PROVIDER = %q", env["ANAMNESIA_LLM_PROVIDER"])
	}
	// Host-only settings must not leak into the server's environment.
	for _, leaked := range []string{"ANAMNESIA_POSTGRES_PORT", "ANAMNESIA_POSTGRES_PASSWORD", "ANAMNESIA_SERVER_AUTOSTART"} {
		if _, ok := env[leaked]; ok {
			t.Errorf("host-only setting leaked to the server: %s", leaked)
		}
	}
}

// TestServerURLDerivedFromAddr is what stops a changed port from leaving
// the hooks pointing at the old one.
func TestServerURLDerivedFromAddr(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8181": "http://127.0.0.1:8181",
		":9000":          "http://127.0.0.1:9000",
		"0.0.0.0:8282":   "http://127.0.0.1:8282",
	}
	for addr, want := range cases {
		home := t.TempDir()
		t.Setenv(homeEnv, home)
		writeConfig(t, filepath.Join(home, "config.toml"), "[server]\naddr = \""+addr+"\"\n")
		hc, err := loadHostConfig()
		if err != nil {
			t.Fatal(err)
		}
		if got := hc.ServerURL(); got != want {
			t.Errorf("addr %q → %q, want %q", addr, got, want)
		}
	}
}

func TestExternalPostgresSkipsContainer(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"),
		"[postgres]\nurl = \"postgres://u:p@db.internal:5432/mem\"\n")
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if hc.ManagesPostgres() {
		t.Error("postgres.url is set, so no container should be managed")
	}
	if got := hc.DatabaseURL(); got != "postgres://u:p@db.internal:5432/mem" {
		t.Errorf("DatabaseURL = %q", got)
	}
}

// TestLegacyProjectFileStillReadable keeps existing .anamnesia.toml files
// working; they used a bare `project = "x"` key.
func TestLegacyProjectFileStillReadable(t *testing.T) {
	isolatedHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, projectConfigName), "project = \"legacy-slug\"\n")
	t.Chdir(project)

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Project(); got != "legacy-slug" {
		t.Errorf("Project() = %q, want legacy-slug", got)
	}
	if len(hc.Unknown) > 0 {
		t.Errorf("legacy key reported as unknown: %v", hc.Unknown)
	}
}

func TestSettingValidation(t *testing.T) {
	tests := []struct {
		key     string
		value   string
		wantErr bool
	}{
		{"embed.dims", "3072", false},
		{"embed.dims", "0", true},
		{"embed.dims", "-1", true},
		{"worker.extract_every", "30s", false},
		{"worker.extract_every", "0s", true},
		{"server.autostart", "false", false},
		{"server.autostart", "maybe", true},
		{"pii.mode", "redact", false},
		{"pii.mode", "shred", true},
		{"llm.provider", "", false},
		{"openrouter.api_key", "sk-or-anything", false},
	}
	for _, tc := range tests {
		s, ok := settingByKey[tc.key]
		if !ok {
			t.Fatalf("unknown test key %q", tc.key)
		}
		_, err := s.validate(tc.value)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s=%q: error = %v, wantErr = %v", tc.key, tc.value, err, tc.wantErr)
		}
	}
}

func TestSecretsAreMasked(t *testing.T) {
	s := settingByKey["openrouter.api_key"]
	if got := s.mask("sk-or-v1-abcdef123456"); strings.Contains(got, "abcdef") {
		t.Errorf("mask leaked the key: %q", got)
	}
	if got := s.mask(""); got != "" {
		t.Errorf("mask of empty = %q, want empty", got)
	}
}

// TestEverySettingHasUniqueEnvMapping catches a copy-paste in the settings
// table that would make two settings fight over one variable.
func TestEverySettingHasUniqueEnvMapping(t *testing.T) {
	seen := map[string]string{}
	for _, s := range settings {
		if s.Env == "" {
			continue
		}
		if prev, dup := seen[s.Env]; dup {
			t.Errorf("%s and %s both map to %s", prev, s.Key, s.Env)
		}
		seen[s.Env] = s.Key
	}
}

func TestSanitizeSlug(t *testing.T) {
	cases := map[string]string{
		"Anamnesia":            "anamnesia",
		"my project":           "my-project",
		"  Trailing  ":         "trailing",
		"weird!!chars??":       "weird-chars",
		"already-fine_1.0":     "already-fine_1.0",
		"---leading-trailing-": "leading-trailing",
	}
	for in, want := range cases {
		if got := sanitizeSlug(in); got != want {
			t.Errorf("sanitizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
