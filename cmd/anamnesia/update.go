// update.go reconciles an installation with the binary that is running.
//
// Upgrading Anamnesia is: replace the binary, run `anamnesia update`.
//
// There is deliberately no self-download here. The one thing an update has
// to guarantee is that nothing is left half-upgraded, and that is entirely
// about the things around the binary: the hooks still name a path that
// exists, the recorded hook set matches this version, the schema matches
// the configured embedding width, the database image is current, and the
// running server is this binary rather than the previous one.
//
// The server cannot drift from the CLI, because they are the same
// executable. That removes the whole category of version skew that a
// separate server image would reintroduce.
package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	updateSkipPull  bool
	updateSkipHooks bool
	updateScope     string
	updateConfigDir string
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Reconcile hooks, schema, image and server with this binary",
	Long: "Bring an existing installation in line with the binary you are running.\n\n" +
		"Replace the binary first, then run this. It re-points Claude Code's\n" +
		"hooks at the current path, refreshes the hook set, pulls the database\n" +
		"image, applies migrations, reconciles the embedding schema, restarts\n" +
		"the server, and finishes with a full health check.\n\n" +
		"Safe to run at any time; every step is idempotent.",
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().BoolVar(&updateSkipPull, "no-pull", false, "skip pulling the postgres image")
	updateCmd.Flags().BoolVar(&updateSkipHooks, "no-hooks", false, "skip refreshing Claude Code's config")
	updateCmd.Flags().StringVar(&updateScope, "scope", "user", "hook scope to refresh: user or project")
	updateCmd.Flags().StringVar(&updateConfigDir, "config-dir", "", "override the config directory (testing escape hatch)")
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	self, err := selfPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Updating Anamnesia to %s\n", version)
	fmt.Fprintf(out, "  binary: %s\n", self)

	// A missing config means this is a first run, not an update.
	created, configPath, err := ensureConfigFile()
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "  created %s (no config existed yet)\n", configPath)
	}
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	if len(hc.Unknown) > 0 {
		fmt.Fprintln(out, "  unrecognised keys in your config (left untouched):")
		for _, u := range hc.Unknown {
			fmt.Fprintf(out, "    %s\n", u)
		}
	}

	if !updateSkipHooks {
		installF.scope = updateScope
		installF.configDir = updateConfigDir
		installF.dryRun = false
		if err := applyInstall(hc, out); err != nil {
			return err
		}
	}

	if !updateSkipPull {
		if err := pullPostgresImage(ctx, hc, out); err != nil {
			// A failed pull is not a reason to abandon the update; the
			// existing local image still works.
			fmt.Fprintf(out, "  could not pull the database image: %v\n", err)
		}
	}

	if err := ensurePostgres(ctx, hc, out); err != nil {
		return err
	}

	// Restart before migrating so the new binary is the one that applies
	// migrations and enforces the schema checks.
	if err := stopServer(hc, out); err != nil {
		return err
	}
	if err := startStack(ctx, hc, out); err != nil {
		// Report health anyway: a refusal to boot is usually the schema
		// guard, and its message is the actionable part.
		fmt.Fprintf(out, "  server did not come up: %v\n", err)
		fmt.Fprintln(out, "  run `anamnesia logs` for the reason, then `anamnesia migrate`")
		return err
	}

	fmt.Fprintln(out)
	if err := reportHealth(ctx, hc, out); err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Update complete. Run `anamnesia doctor` for a full check.")
	return nil
}
