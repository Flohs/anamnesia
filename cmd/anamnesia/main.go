// Anamnesia is a single binary: the CLI, the Claude Code hooks, and the
// memory server are all the same executable.
//
// Getting started:
//
//	anamnesia setup      create ~/.anamnesia/config.toml, wire Claude Code, start
//	anamnesia config …   read and write settings
//	anamnesia doctor     verify the installation
//
// Running the stack:
//
//	anamnesia start      start the postgres container + the server
//	anamnesia stop       stop the server (--all also stops postgres)
//	anamnesia restart    restart the server
//	anamnesia status     what is running
//	anamnesia logs       the server log
//
// Maintenance:
//
//	anamnesia update     reconcile everything with this binary after upgrading
//	anamnesia migrate    apply migrations (--dims rebuilds the vector columns)
//	anamnesia install    (re)wire Claude Code only
//	anamnesia uninstall  remove the wiring (--purge also deletes stored memory)
//
// The only thing Anamnesia puts in a container is Postgres. There is no
// compose file and no Anamnesia image: the binary you downloaded is the
// server, so `anamnesia start` needs nothing but Docker.
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type rootFlags struct {
	serverURL string
	project   string
	user      string
	verbose   int
	allowRoot bool

	log *slog.Logger
}

var rf rootFlags

var root = &cobra.Command{
	Use:   "anamnesia",
	Short: "Anamnesia — local-first memory for Claude Code",
	Long: "Anamnesia gives Claude Code a long-term memory: it reads what you work\n" +
		"on, keeps what matters, and hands it back at the start of your next\n" +
		"session.\n\n" +
		"One binary does everything. It manages its own Postgres container, so\n" +
		"there is no compose file to write and no image to build.\n\n" +
		"Start with `anamnesia setup`.",
	SilenceUsage:  true,
	SilenceErrors: true, // main() is the only place that prints errors
}

func init() {
	root.PersistentFlags().StringVar(&rf.serverURL, "server", "", "server URL (default: from your config)")
	root.PersistentFlags().StringVar(&rf.project, "project", "", "project slug (default: this git repository's name)")
	root.PersistentFlags().StringVar(&rf.user, "user", "", "user handle (default: from your config)")
	root.PersistentFlags().CountVarP(&rf.verbose, "verbose", "v", "increase logging verbosity (-v / -vv)")
	root.PersistentFlags().BoolVar(&rf.allowRoot, "allow-root", false, "permit running under sudo (see the warning it prints)")

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		rf.log = newLogger(rf.verbose, cmd.ErrOrStderr())
		return refuseSudo(cmd)
	}

	// Onboarding and configuration.
	root.AddCommand(setupCmd)
	root.AddCommand(initCmd)
	root.AddCommand(configCmd)
	root.AddCommand(doctorCmd)

	// Running the stack.
	root.AddCommand(startCmd)
	root.AddCommand(stopCmd)
	root.AddCommand(restartCmd)
	root.AddCommand(statusCmd)
	root.AddCommand(logsCmd)

	// Maintenance.
	root.AddCommand(updateCmd)
	root.AddCommand(migrateCmd)
	root.AddCommand(evalCmd)
	root.AddCommand(installCmd)
	root.AddCommand(uninstallCmd)

	// Internal: invoked by Claude Code, and by `anamnesia start`.
	root.AddCommand(hookCmd)
	root.AddCommand(serveCmd)

	root.AddCommand(versionCmd)
}

// rootSafeCommands may legitimately run as root: they neither write into the
// invoking user's home nor start anything long-lived.
var rootSafeCommands = map[string]bool{
	"version": true, "help": true, "completion": true, "doctor": true, "status": true,
}

// refuseSudo stops a command that was escalated from a normal account.
//
// `sudo anamnesia update` looks harmless and is not. Anamnesia writes the
// user's config, patches Claude Code's settings.json and .claude.json, and
// starts the memory server. Under sudo every one of those becomes root-owned:
// the user can no longer write their own Claude Code config, and the server
// runs as root. Self-update asks for a password for the one step that needs
// it, so there is no reason to escalate the rest.
//
// Genuine root usage (a root account, not `sudo` from a user) is left alone,
// as is --allow-root for anyone who means it.
func refuseSudo(cmd *cobra.Command) error {
	return sudoRefusal(os.Geteuid(), os.Getenv("SUDO_USER"), cmd.Name(), rf.allowRoot)
}

// sudoRefusal is the decision, separated from the environment so it can be
// tested without actually being root.
func sudoRefusal(euid int, invoker, command string, allowRoot bool) error {
	if allowRoot || euid != 0 {
		return nil
	}
	if invoker == "" || invoker == "root" {
		return nil // actually running as root, not escalated from a user
	}
	if rootSafeCommands[command] {
		return nil
	}
	return fmt.Errorf(`refusing to run as root.

You ran this with sudo from the account %q. Anamnesia writes your config,
patches Claude Code's settings.json and .claude.json, and starts the memory
server: under sudo all of those end up owned by root, and Claude Code can no
longer write its own files.

Run it as yourself:

  anamnesia %s

If the binary itself needs replacing, anamnesia update will ask for your
password for that one step. Pass --allow-root to override this.`,
		invoker, command)
}

func newLogger(v int, w io.Writer) *slog.Logger {
	lvl := slog.LevelWarn
	switch {
	case v >= 2:
		lvl = slog.LevelDebug
	case v >= 1:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
}

func main() {
	if err := root.Execute(); err != nil {
		// doctor prints its own report and signals failure with an empty
		// error, so there is nothing left to say here.
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		os.Exit(1)
	}
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintf(cmd.OutOrStdout(), "anamnesia %s (commit %s, built %s)\n", version, commit, date)
		return nil
	},
}
