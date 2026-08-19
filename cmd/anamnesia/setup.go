// setup.go is the one command a new user runs.
//
//	anamnesia setup
//
// It creates ~/.anamnesia/config.toml with a generated database password,
// tells the user where that file is, wires Claude Code's hooks and MCP
// entry to this exact binary, starts the stack, and finishes by reporting
// health. Every step is idempotent, so running it again after editing the
// config is a normal thing to do rather than a repair operation.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var (
	setupNoHooks   bool
	setupNoStart   bool
	setupScope     string
	setupConfigDir string
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Create the config, wire Claude Code, and start the stack",
	Long: "Prepare Anamnesia for use:\n\n" +
		"  1. create ~/.anamnesia/config.toml (with a generated database password)\n" +
		"  2. wire Claude Code's hooks and MCP entry to this binary\n" +
		"  3. start the Postgres container and the server\n" +
		"  4. report health\n\n" +
		"Idempotent: re-running it repairs anything that has drifted and leaves\n" +
		"your existing settings alone.",
	RunE: runSetup,
}

func init() {
	setupCmd.Flags().BoolVar(&setupNoHooks, "no-hooks", false, "skip patching Claude Code's config")
	setupCmd.Flags().BoolVar(&setupNoStart, "no-start", false, "skip starting the stack")
	setupCmd.Flags().StringVar(&setupScope, "scope", "user", "hook scope: user (~/.claude) or project ($PWD/.claude)")
	setupCmd.Flags().StringVar(&setupConfigDir, "config-dir", "", "override the config directory (testing escape hatch)")
	setupCmd.Flags().BoolVar(&adoptContainer, "adopt", false,
		"claim a postgres container created before installs recorded ownership")
}

func runSetup(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Setting up Anamnesia")

	home, err := ensureHome()
	if err != nil {
		return err
	}
	created, configPath, err := ensureConfigFile()
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "  created %s\n", configPath)
	} else {
		fmt.Fprintf(out, "  using existing %s\n", configPath)
	}

	hc, err := loadHostConfig()
	if err != nil {
		return err
	}

	if !setupNoHooks {
		installF.scope = setupScope
		installF.configDir = setupConfigDir
		installF.dryRun = false
		if err := applyInstall(hc, out); err != nil {
			return err
		}
	}

	if !setupNoStart {
		if err := startStack(cmd.Context(), hc, out); err != nil {
			fmt.Fprintf(out, "\n  could not start the stack: %v\n", err)
			printNextSteps(out, hc, configPath, home)
			return err
		}
	}

	fmt.Fprintln(out)
	printNextSteps(out, hc, configPath, home)

	if setupNoStart {
		return nil
	}
	fmt.Fprintln(out)
	return reportHealth(cmd.Context(), hc, out)
}

// printNextSteps tells the user where their configuration lives and how to
// change it. This is deliberately verbose: it is the one moment where the
// user has to know that a file exists and that it is theirs to edit.
func printNextSteps(out io.Writer, hc *hostConfig, configPath, home string) {
	fmt.Fprintln(out, "Your configuration")
	fmt.Fprintf(out, "  file:  %s\n", configPath)
	fmt.Fprintf(out, "  state: %s\n", home)
	fmt.Fprintln(out, "  The file is commented. Edit it directly, or use the CLI:")
	fmt.Fprintln(out, "    anamnesia config set openrouter.api_key sk-or-v1-…")
	fmt.Fprintln(out, "    anamnesia config list")
	fmt.Fprintln(out, "    anamnesia config edit")

	if hc.Get("llm.provider") == "" && hc.Get("openrouter.api_key") == "" &&
		hc.Get("anthropic.api_key") == "" && hc.Get("openai.api_key") == "" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  No model configured yet, so nothing will be extracted from your")
		fmt.Fprintln(out, "  sessions. One key is enough to light up all three workloads:")
		fmt.Fprintln(out, "    anamnesia config set openrouter.api_key sk-or-v1-…")
		fmt.Fprintln(out, "    anamnesia restart")
	}
}

// ensureConfigFile creates the global config with a generated password if
// it does not exist. Returns whether it created the file.
func ensureConfigFile() (bool, string, error) {
	path, err := globalConfigPath()
	if err != nil {
		return false, "", err
	}
	if fileExists(path) {
		return false, path, ensurePasswordPresent(path)
	}
	if _, err := ensureHome(); err != nil {
		return false, "", err
	}
	password, err := generatePassword()
	if err != nil {
		return false, "", err
	}
	seed := map[string]string{
		"postgres.password": password,
		"identity.user":     defaultUserHandle(),
	}
	if err := writeFileAtomic(path, renderDefaultConfig(seed)); err != nil {
		return false, "", err
	}
	return true, path, nil
}

// ensurePasswordPresent fills in a generated password when an existing
// config has none, which is what happens after a hand-written config or an
// upgrade from a version that had no config file at all.
func ensurePasswordPresent(path string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	if !hc.ManagesPostgres() || hc.Get("postgres.password") != "" {
		return nil
	}
	password, err := generatePassword()
	if err != nil {
		return err
	}
	return setConfigValue(path, "postgres.password", password)
}

// defaultUserHandle is the OS username, normalised.
func defaultUserHandle() string {
	if u := os.Getenv("USER"); u != "" {
		return sanitizeSlug(u)
	}
	return "default"
}
