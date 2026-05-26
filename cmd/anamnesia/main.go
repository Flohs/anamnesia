// Anamnesia is a single binary that does double duty.
//
// On a user's host machine, it:
//
//	anamnesia init       write .anamnesia.toml for the current project
//	anamnesia install    patch ~/.claude/settings.json + ~/.claude.json
//	anamnesia uninstall  remove the patches
//	anamnesia hook ...   invoked by Claude Code's hooks at runtime
//	anamnesia doctor     diagnose config + connectivity
//	anamnesia up         docker compose up (start the local stack)
//	anamnesia down       docker compose down
//
// Inside the docker-compose container, it runs:
//
//	anamnesia serve              HTTP API + MCP + (in-process) worker
//	anamnesia serve --worker     only the background worker
//	anamnesia migrate            apply DB migrations and exit
//
// One binary, one Go module, one container image.
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
	configPath string
	serverURL  string
	project    string
	user       string
	verbose    int

	log *slog.Logger
}

var rf rootFlags

var root = &cobra.Command{
	Use:   "anamnesia",
	Short: "Anamnesia — local-first memory for Claude Code",
	Long: "Anamnesia is a single binary that runs the memory server inside docker\n" +
		"and runs the Claude Code hooks on your host. One install, one config,\n" +
		"one place to store everything you've taught Claude.",
	SilenceUsage: true,
}

func init() {
	root.PersistentFlags().StringVar(&rf.configPath, "config", "", "config file (default ~/.anamnesia/config.toml)")
	root.PersistentFlags().StringVar(&rf.serverURL, "server", "", "Anamnesia server URL (default http://localhost:8181)")
	root.PersistentFlags().StringVar(&rf.project, "project", "", "project slug (default basename of CWD)")
	root.PersistentFlags().StringVar(&rf.user, "user", "", "user handle (default $USER or 'default')")
	root.PersistentFlags().CountVarP(&rf.verbose, "verbose", "v", "increase logging verbosity (-v / -vv)")

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		rf.log = newLogger(rf.verbose, cmd.ErrOrStderr())
		return nil
	}

	root.AddCommand(versionCmd)
	root.AddCommand(serveCmd)
	root.AddCommand(migrateCmd)
	root.AddCommand(initCmd)
	root.AddCommand(installCmd)
	root.AddCommand(uninstallCmd)
	root.AddCommand(hookCmd)
	root.AddCommand(doctorCmd)
	root.AddCommand(upCmd)
	root.AddCommand(downCmd)
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
