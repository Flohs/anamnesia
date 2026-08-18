// init.go writes .anamnesia.toml, the per-repository configuration.
//
// The file is optional: without it a project slug is derived from the
// repository's directory name, and everything works. What it buys is
// pinning that slug, so renaming the directory or working from a clone
// under another name does not file memories somewhere new.
//
// Its contents come from the settings table rather than a literal here,
// so the template cannot drift from what is actually settable, and a
// secret cannot reach a file that is committed with the repository.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var (
	initForce   bool
	initProject string
)

var initCmd = newInitCmd()

func newInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Write .anamnesia.toml for this repository",
		Long: "Write .anamnesia.toml at the repository root, so memories from this\n" +
			"project are always filed under the same slug.\n\n" +
			"The slug is detected from the repository directory, which is what is\n" +
			"used already when there is no file; init pins it rather than changing\n" +
			"it. The generated file is meant to be committed, so it carries no\n" +
			"API keys or passwords: those belong in ~/.anamnesia/config.toml.",
		Args: cobra.NoArgs,
		RunE: runInit,
	}
	c.Flags().BoolVar(&initForce, "force", false, "rewrite an existing .anamnesia.toml")
	c.Flags().StringVar(&initProject, "project", "", "project slug to write (default: this directory's name)")
	return c
}

func runInit(cmd *cobra.Command, _ []string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := gitToplevelOrCWD(wd)
	path := filepath.Join(dir, projectConfigName)
	if fileExists(path) && !initForce {
		return fmt.Errorf("%s already exists.\n"+
			"Change one key with `anamnesia config --project set <key> <value>`, "+
			"or pass --force to rewrite the file", path)
	}

	project := sanitizeSlug(initProject)
	if project == "" {
		project = sanitizeSlug(filepath.Base(dir))
	}
	if project == "" {
		return errors.New("could not derive a project slug from " + dir + "; pass --project")
	}

	if err := writeFileAtomic(path, renderProjectConfig(project)); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✦ wrote %s (project: %s)\n", path, project)
	fmt.Fprintf(out, "  memories from this repository are filed under %q\n", project)
	return nil
}

// projectSettings are the settings worth overriding for one repository.
// Secrets are excluded structurally rather than by remembering to: the
// file is committed, and a published key is not a mistake a generated
// file gets to make.
func projectSettings() []setting {
	var out []setting
	for _, s := range settings {
		if s.Project && s.Kind != kSecret {
			out = append(out, s)
		}
	}
	return out
}

// renderProjectConfig builds .anamnesia.toml, seeded with the project
// slug and carrying each setting's own documentation.
func renderProjectConfig(project string) []byte {
	var b strings.Builder
	b.WriteString("# Anamnesia per-project configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# Overrides ~/.anamnesia/config.toml for work inside this repository.\n")
	b.WriteString("# Written by `anamnesia init`, or with:\n")
	b.WriteString("#   anamnesia config --project set <key> <value>\n")
	b.WriteString("#\n")
	b.WriteString("# This file is committed with the repository, so it holds no API keys\n")
	b.WriteString("# and no passwords. Those live in ~/.anamnesia/config.toml.\n")

	bySection := map[string][]setting{}
	for _, s := range projectSettings() {
		bySection[s.section()] = append(bySection[s.section()], s)
	}
	for _, sec := range sectionOrder() {
		group, ok := bySection[sec]
		if !ok {
			continue
		}
		b.WriteString("\n[" + sec + "]\n")
		for _, s := range group {
			for _, line := range wrapComment(s.Doc, 72) {
				b.WriteString("# " + line + "\n")
			}
			v := s.Def
			if s.Key == "identity.project" {
				v = project
			}
			line := fmt.Sprintf("%s = %q\n", s.name(), v)
			if v == "" {
				// Written commented out. A blank value here is an
				// override like any other, and identity.user going
				// blank files this repository under whoever $USER is
				// rather than the handle the global config chose.
				line = "# " + line
			}
			b.WriteString(line)
		}
	}
	return []byte(b.String())
}
