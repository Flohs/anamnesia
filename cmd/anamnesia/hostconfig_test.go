package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flohs/anamnesia/internal/config"
	"github.com/flohs/anamnesia/internal/decay"
	"github.com/flohs/anamnesia/pkg/anamnesia"
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

func TestActivitySettingsReachTheServer(t *testing.T) {
	isolatedHome(t)
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Get("activity.enabled"); got != "true" {
		t.Errorf("activity.enabled default = %q, want true", got)
	}
	if got := hc.Get("activity.traces"); got != "200" {
		t.Errorf("activity.traces default = %q, want 200", got)
	}
	env := strings.Join(hc.ServerEnv(), "\n")
	for _, want := range []string{"ANAMNESIA_ACTIVITY_ENABLED=true", "ANAMNESIA_ACTIVITY_TRACES=200"} {
		if !strings.Contains(env, want) {
			t.Errorf("server environment is missing %s", want)
		}
	}
}

func TestActivityTracesRejectsZero(t *testing.T) {
	// The recorder is switched off with activity.enabled, not with a
	// ring of size zero: every numeric setting rejects non-positive
	// values, and that check is worth more than the shorthand.
	if _, err := settingByKey["activity.traces"].validate("0"); err == nil {
		t.Error("activity.traces accepted 0")
	}
}

func TestConfigSnapshotMasksSecrets(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), `
[openrouter]
api_key = "sk-or-v1-supersecretvalue"
[llm]
provider = "openrouter"
`)
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}

	byKey := map[string]string{}
	source := map[string]string{}
	for _, item := range configSnapshot(hc) {
		byKey[item.Key] = item.Value
		source[item.Key] = item.Source
		if item.Key == "openrouter.api_key" && !item.Secret {
			t.Error("openrouter.api_key is not flagged as a secret")
		}
	}
	if got := byKey["openrouter.api_key"]; strings.Contains(got, "supersecret") {
		t.Fatalf("the API key reached the snapshot unmasked: %q", got)
	}
	if got := byKey["llm.provider"]; got != "openrouter" {
		t.Errorf("llm.provider = %q, want openrouter", got)
	}
	if got := source["llm.provider"]; got != "global" {
		t.Errorf("llm.provider source = %q, want global", got)
	}
	if got := source["embed.dims"]; got != "default" {
		t.Errorf("embed.dims source = %q, want default", got)
	}
}

func TestEnvMatchingTheFileKeepsTheFileOrigin(t *testing.T) {
	// `anamnesia start` hands the server its own config as environment
	// variables, so inside the server process every configured setting
	// would otherwise report itself as an environment override. That
	// makes `config list` and /v1/config lie about where a value came
	// from. An environment value equal to the file's is not an override.
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[llm]\nmodel = \"claude-sonnet-4-6\"\n")
	t.Setenv("ANAMNESIA_LLM_MODEL", "claude-sonnet-4-6")

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Origin("llm.model"); got != fromGlobal {
		t.Errorf("origin = %q, want %q when the environment merely echoes the file", got, fromGlobal)
	}
	if got := hc.Get("llm.model"); got != "claude-sonnet-4-6" {
		t.Errorf("value = %q", got)
	}
}

func TestEnvDifferingFromTheFileIsStillAnOverride(t *testing.T) {
	home := isolatedHome(t)
	writeConfig(t, filepath.Join(home, "config.toml"), "[llm]\nmodel = \"claude-sonnet-4-6\"\n")
	t.Setenv("ANAMNESIA_LLM_MODEL", "gpt-4o-mini")

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Origin("llm.model"); got != fromEnv {
		t.Errorf("origin = %q, want %q for a real override", got, fromEnv)
	}
	if got := hc.Get("llm.model"); got != "gpt-4o-mini" {
		t.Errorf("value = %q, want the environment to win", got)
	}
}

func TestDecayHalfLifeDefaultsMatchTheCode(t *testing.T) {
	// Promoting a default to a setting must not change what the server
	// does. This fails if the two ever drift, and if a new experience
	// kind arrives without a setting to go with it.
	for kind, want := range decay.DefaultHalfLives() {
		key := "decay.half_life_" + string(kind)
		s, ok := settingByKey[key]
		if !ok {
			t.Errorf("experience kind %q has a half-life in the code and no %s setting", kind, key)
			continue
		}
		got, err := time.ParseDuration(s.Def)
		if err != nil {
			t.Errorf("%s default %q is not a duration: %v", key, s.Def, err)
			continue
		}
		if got != want {
			t.Errorf("%s defaults to %s, but the decay worker uses %s", key, got, want)
		}
	}
}

func TestDecayConfigCarriesEverySettingToTheWorker(t *testing.T) {
	cfg := &config.Config{
		DecayHalfLifeCase:     48 * time.Hour,
		DecayHalfLifeStrategy: 100 * 24 * time.Hour,
		DecayHalfLifeHybrid:   72 * time.Hour,
	}
	got := decayConfig(cfg).HalfLives
	want := map[anamnesia.ExperienceKind]time.Duration{
		anamnesia.ExperienceCase:     48 * time.Hour,
		anamnesia.ExperienceStrategy: 100 * 24 * time.Hour,
		anamnesia.ExperienceHybrid:   72 * time.Hour,
	}
	if len(got) != len(want) {
		t.Fatalf("half-lives = %v, want %v", got, want)
	}
	for kind, d := range want {
		if got[kind] != d {
			t.Errorf("%s half-life = %s, want %s", kind, got[kind], d)
		}
	}
}

func TestDecaySettingsReachTheServer(t *testing.T) {
	isolatedHome(t)
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(hc.ServerEnv(), "\n")
	for _, want := range []string{
		"ANAMNESIA_DECAY_HALF_LIFE_CASE=336h",
		"ANAMNESIA_DECAY_HALF_LIFE_STRATEGY=8760h",
		"ANAMNESIA_DECAY_HALF_LIFE_HYBRID=1440h",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("server environment is missing %s", want)
		}
	}
}
