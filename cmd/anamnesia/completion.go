// completion.go installs shell tab completion, and completes argument
// values rather than only command and flag names.
//
// Cobra generates the script; what it does not do is get it onto disk or
// sourced, so the feature existed and no user ever met it. The generated
// script is a shim that calls `anamnesia __complete` at runtime, which is
// why installing it once is enough: new commands and new settings come
// from whatever binary is on PATH, so it cannot go stale and `update`
// does not have to rewrite it.
//
// The scripts live under ANAMNESIA_HOME rather than in a package
// manager's completion directory, which brew and friends prune from
// under you, and which does not exist at all on a machine without them.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/flohs/anamnesia/internal/store"
)

// completionMarker labels the block written into a shell rc file.
const completionMarker = "# anamnesia shell completion (managed by `anamnesia install`)"

// completionTimeout bounds a completion that has to ask the database.
// A tab press must never hang a prompt, so a stopped stack offers
// nothing rather than making the user wait for a connection to fail.
const completionTimeout = 300 * time.Millisecond

// shellKind is a shell Anamnesia knows how to write completion for.
type shellKind string

const (
	shellZsh  shellKind = "zsh"
	shellBash shellKind = "bash"
	shellFish shellKind = "fish"
)

// completionShells is every shell uninstall has to sweep. The one $SHELL
// names today is not necessarily the one the script was installed for.
var completionShells = []shellKind{shellZsh, shellBash, shellFish}

// detectShell reads $SHELL. An unrecognised shell is not an error: it
// returns the empty kind and the caller skips the step.
func detectShell() shellKind {
	base := filepath.Base(strings.TrimSpace(os.Getenv("SHELL")))
	for _, k := range completionShells {
		if base == string(k) {
			return k
		}
	}
	return ""
}

// scriptPath is where the generated script is written. Fish autoloads its
// own completions directory, so its script goes there directly.
func (k shellKind) scriptPath() (string, error) {
	if k == shellFish {
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locate home directory: %w", err)
			}
			dir = filepath.Join(home, ".config")
		}
		return filepath.Join(dir, "fish", "completions", "anamnesia.fish"), nil
	}
	dir, err := homeFile("completions")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "anamnesia."+string(k)), nil
}

// rcPath is the shell startup file that has to source the script. Fish
// returns empty: it needs no edit.
func (k shellKind) rcPath() (string, error) {
	var name string
	switch k {
	case shellZsh:
		name = ".zshrc"
	case shellBash:
		name = ".bashrc"
	default:
		return "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, name), nil
}

// generate writes the completion script for this shell.
func (k shellKind) generate(w io.Writer) error {
	switch k {
	case shellZsh:
		return root.GenZshCompletion(w)
	case shellBash:
		return root.GenBashCompletionV2(w, true)
	case shellFish:
		return root.GenFishCompletion(w, true)
	}
	return fmt.Errorf("no completion script for shell %q", k)
}

// installCompletion writes and wires completion for the current shell.
func installCompletion(out io.Writer) error {
	k := detectShell()
	if k == "" {
		fmt.Fprintf(out, "  completion skipped: no script for shell %q\n", os.Getenv("SHELL"))
		return nil
	}
	return installCompletionFor(k, out)
}

func installCompletionFor(k shellKind, out io.Writer) error {
	script, err := k.scriptPath()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := k.generate(&buf); err != nil {
		return err
	}
	// Under ~/.config, which is conventionally readable; ANAMNESIA_HOME
	// is the user's own state and stays 0700.
	perm := os.FileMode(0o700)
	if k == shellFish {
		perm = 0o755
	}
	if err := os.MkdirAll(filepath.Dir(script), perm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(script), err)
	}
	if err := writeFileAtomic(script, buf.Bytes()); err != nil {
		return err
	}

	rc, err := k.rcPath()
	if err != nil {
		return err
	}
	if rc == "" {
		fmt.Fprintf(out, "  wrote %s completion to %s\n", k, script)
		return nil
	}
	added, err := ensureRCSources(rc, script)
	if err != nil {
		return err
	}
	if added {
		fmt.Fprintf(out, "  wrote %s completion to %s (%s updated)\n", k, script, rc)
		fmt.Fprintf(out, "    open a new shell, or `source %s`, to use it\n", rc)
	} else {
		fmt.Fprintf(out, "  refreshed %s completion in %s\n", k, script)
	}
	return nil
}

// removeCompletion undoes the install for every shell, since the script
// may have been installed under one and uninstalled under another.
func removeCompletion(out io.Writer) error {
	for _, k := range completionShells {
		script, err := k.scriptPath()
		if err != nil {
			return err
		}
		removed := false
		if fileExists(script) {
			if err := os.Remove(script); err != nil {
				return fmt.Errorf("remove %s: %w", script, err)
			}
			removed = true
		}
		rc, err := k.rcPath()
		if err != nil {
			return err
		}
		if rc != "" {
			stripped, err := stripRCSource(rc, script)
			if err != nil {
				return err
			}
			removed = removed || stripped
		}
		if removed {
			fmt.Fprintf(out, "  removed %s completion\n", k)
		}
	}
	return nil
}

// completionBlock is what gets appended to a shell rc.
func completionBlock(script string) string {
	p := rcPathForm(script)
	return completionMarker + "\n" +
		fmt.Sprintf("[ -f %q ] && source %q\n", p, p)
}

// rcPathForm renders a path under the home directory as $HOME/…, so the
// line survives the account being moved. A path outside home (a test's
// ANAMNESIA_HOME) is written as it stands.
func rcPathForm(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	rel, err := filepath.Rel(home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return "$HOME/" + filepath.ToSlash(rel)
}

// rcSourceForms are the ways the script path can appear in an rc file: as
// written, as $HOME/…, and as ~/… for a line someone added by hand.
func rcSourceForms(script string) []string {
	forms := []string{script}
	if p := rcPathForm(script); p != script {
		forms = append(forms, p, "~"+strings.TrimPrefix(p, "$HOME"))
	}
	return forms
}

// rcSourcesScript reports whether the rc already sources the script,
// however it was written. Idempotency cannot key off the marker alone:
// an entry added by hand or by an older version carries no marker, and
// trusting one is what appended a second copy of every hook once.
func rcSourcesScript(body, script string) bool {
	forms := rcSourceForms(script)
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		for _, f := range forms {
			if strings.Contains(t, f) {
				return true
			}
		}
	}
	return false
}

// ensureRCSources appends the block unless the rc already sources the
// script. Reports whether it wrote anything.
func ensureRCSources(rc, script string) (bool, error) {
	raw, err := os.ReadFile(rc)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", rc, err)
	}
	body := string(raw)
	if rcSourcesScript(body, script) {
		return false, nil
	}
	if err := backupOnce(rc); err != nil {
		return false, err
	}
	if body != "" {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "\n"
	}
	return true, writeFileAtomic(rc, []byte(body+completionBlock(script)))
}

// stripRCSource removes the block again, including the blank line the
// install added ahead of it, so install followed by uninstall leaves the
// file byte for byte as it was.
func stripRCSource(rc, script string) (bool, error) {
	raw, err := os.ReadFile(rc)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", rc, err)
	}
	forms := rcSourceForms(script)
	lines := strings.Split(string(raw), "\n")
	kept := make([]string, 0, len(lines))
	dropped := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		drop := t == completionMarker
		if !drop {
			for _, f := range forms {
				if strings.Contains(t, f) {
					drop = true
					break
				}
			}
		}
		if !drop {
			kept = append(kept, line)
			continue
		}
		dropped = true
		// The install put a blank line ahead of the block; take it back
		// so a round trip changes nothing.
		if t == completionMarker && len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
			kept = kept[:len(kept)-1]
		}
	}
	if !dropped {
		return false, nil
	}
	body := strings.Join(kept, "\n")
	// An rc that holds nothing but our block was created by us.
	if strings.TrimSpace(body) == "" {
		return true, os.Remove(rc)
	}
	return true, writeFileAtomic(rc, []byte(body))
}

// ─── completing argument values ──────────────────────────────────────

// completeSettingKey completes `config get|set` and the bare `config`
// form: the key first, then the values that key accepts.
func completeSettingKey(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		out := make([]string, 0, len(settings))
		for _, s := range settings {
			if strings.HasPrefix(s.Key, toComplete) {
				out = append(out, s.Key+"\t"+completionDoc(s.Doc))
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	case 1:
		s, ok := settingByKey[args[0]]
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, v := range s.valueCompletions() {
			if strings.HasPrefix(v, toComplete) {
				out = append(out, v)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// completionDoc trims a setting's documentation to something a shell can
// show on one line beside the key.
func completionDoc(doc string) string {
	if i := strings.IndexAny(doc, ".\n"); i > 0 {
		doc = doc[:i]
	}
	const max = 60
	if len(doc) > max {
		doc = strings.TrimSpace(doc[:max]) + "…"
	}
	return doc
}

// completeSettingKeyOnly is `config get`, which takes a key and nothing
// after it.
func completeSettingKeyOnly(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeSettingKey(cmd, nil, toComplete)
}

// completeProjectSlugArg is `project move <to>`, which takes one slug.
func completeProjectSlugArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeProjectSlug(cmd, args, toComplete)
}

// completeProjectSlug offers the projects this user actually has. It
// answers nothing rather than an error when the database is unreachable:
// a stopped stack must not put a failure in front of a tab press.
func completeProjectSlug(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()

	hc, err := loadHostConfig()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer st.Close()

	userID, found, err := st.LookupUser(ctx, hc.User())
	if err != nil || !found {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	projects, err := st.ListProjects(ctx, &userID)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, p := range projects {
		if strings.HasPrefix(p.Slug, toComplete) {
			out = append(out, p.Slug)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
