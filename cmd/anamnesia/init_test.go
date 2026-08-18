package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initInto runs `anamnesia init` with dir as the working directory,
// returning what the command printed.
func initInto(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(dir)
	initForce = false
	initProject = ""
	cmd := newInitCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestInitWritesTheDetectedProject(t *testing.T) {
	dir := t.TempDir()
	// The slug comes from the directory name, which is what the hooks
	// were already deriving. init pins it rather than changing it.
	repo := filepath.Join(dir, "My Service")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := initInto(t, repo)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out, "my-service") {
		t.Errorf("output %q does not name the project it filed under", out)
	}

	body, err := os.ReadFile(filepath.Join(repo, projectConfigName))
	if err != nil {
		t.Fatalf("init wrote no %s: %v", projectConfigName, err)
	}
	got := string(body)
	if !strings.Contains(got, `project = "my-service"`) {
		t.Errorf("file does not carry the detected slug:\n%s", got)
	}
	// The template exists to be read, so the docs have to come with it.
	if !strings.Contains(got, "# ") {
		t.Errorf("file carries no comments, which is the reason for generating it:\n%s", got)
	}
	for _, key := range []string{"[identity]", "user =", "[server]", "url ="} {
		if !strings.Contains(got, key) {
			t.Errorf("template is missing %q:\n%s", key, got)
		}
	}
}

func TestInitFileResolvesBackToTheSameProject(t *testing.T) {
	// A generated file the loader cannot read is worse than no file.
	dir := t.TempDir()
	repo := filepath.Join(dir, "round-trip")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := initInto(t, repo); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Setenv(homeEnv, t.TempDir())
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatalf("loadHostConfig: %v", err)
	}
	if got := hc.Get("identity.project"); got != "round-trip" {
		t.Errorf("identity.project = %q, want the slug init wrote", got)
	}
}

func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, projectConfigName)
	hand := []byte("[identity]\nproject = \"hand-written\"\n")
	if err := os.WriteFile(path, hand, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := initInto(t, repo)
	if err == nil {
		t.Fatal("init overwrote an existing config without --force")
	}
	msg := err.Error() + out
	if !strings.Contains(msg, "--force") {
		t.Errorf("error %q does not say how to overwrite", msg)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(hand) {
		t.Errorf("the existing file was modified by a refused init:\n%s", after)
	}
}

func TestInitForceRewrites(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, projectConfigName)
	if err := os.WriteFile(path, []byte("[identity]\nproject = \"old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := initInto(t, repo, "--force", "--project", "new-name"); err != nil {
		t.Fatalf("init --force: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `project = "new-name"`) {
		t.Errorf("--force did not rewrite the file:\n%s", body)
	}
}

func TestInitProjectFlagOverridesDetection(t *testing.T) {
	repo := t.TempDir()
	if _, err := initInto(t, repo, "--project", "Chosen Name"); err != nil {
		t.Fatalf("init: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(repo, projectConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `project = "chosen-name"`) {
		t.Errorf("--project was not used, or not sanitised:\n%s", body)
	}
}

func TestProjectTemplateNeverCarriesASecret(t *testing.T) {
	// .anamnesia.toml is committed with the repository. A key or a
	// password reaching this template is a secret published by a command
	// whose whole job is to be run without thinking about it.
	for _, s := range projectSettings() {
		if s.Kind == kSecret {
			t.Errorf("%s is a secret and must never be in the project template", s.Key)
		}
	}
	rendered := string(renderProjectConfig("proj"))
	for _, s := range settings {
		if s.Kind != kSecret {
			continue
		}
		if strings.Contains(rendered, s.name()+" =") {
			t.Errorf("the project template renders the secret %s:\n%s", s.Key, rendered)
		}
	}
}

func TestInitDoesNotOverrideGlobalSettingsItHasNoValueFor(t *testing.T) {
	// The template documents every project-settable key, but a key it has
	// no value for must not override the global config with emptiness.
	// identity.user going blank falls back to $USER, which would file
	// this repository's memories under a different person than the rest.
	home := t.TempDir()
	t.Setenv(homeEnv, home)
	rf.serverURL, rf.user, rf.project = "", "", ""
	writeConfig(t, filepath.Join(home, "config.toml"),
		"[identity]\nuser = \"deliberate-handle\"\n\n[server]\nurl = \"http://elsewhere:9000\"\n")

	repo := t.TempDir()
	if _, err := initInto(t, repo); err != nil {
		t.Fatalf("init: %v", err)
	}

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatalf("loadHostConfig: %v", err)
	}
	if got := hc.User(); got != "deliberate-handle" {
		t.Errorf("user = %q after init, want the global handle: the project file blanked it", got)
	}
	if got := hc.ServerURL(); got != "http://elsewhere:9000" {
		t.Errorf("server url = %q after init, want the global one: the project file blanked it", got)
	}
}
