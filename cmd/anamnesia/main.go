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

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		rf.log = newLogger(rf.verbose, cmd.ErrOrStderr())
		return nil
	}

	// Onboarding and configuration.
	root.AddCommand(setupCmd)
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
	root.AddCommand(installCmd)
	root.AddCommand(uninstallCmd)

	// Internal: invoked by Claude Code, and by `anamnesia start`.
	root.AddCommand(hookCmd)
	root.AddCommand(serveCmd)

	root.AddCommand(versionCmd)
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
