// update.go upgrades Anamnesia and reconciles the installation with it.
//
// Upgrading is one command: `anamnesia update`.
//
// Two halves. First the binary itself, against the project's GitHub releases
// (see release.go, which does the verifying). Then everything around it,
// because that is where an upgrade actually goes wrong: the hooks must name a
// path that exists, the recorded hook set must match this version, the schema
// must match the configured embedding width, the database image should be
// current, and the running server has to be this binary rather than the
// previous one.
//
// The order matters. The binary is replaced first and the rest is handed to
// the new one, so the version stamped into Claude Code's hooks and the code
// enforcing the schema are the version that will actually serve.
//
// The server cannot drift from the CLI, because they are the same executable.
// That removes the whole category of version skew a separate server image
// would reintroduce.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

var (
	updateSkipPull    bool
	updateSkipHooks   bool
	updateScope       string
	updateConfigDir   string
	updateCheckOnly   bool
	updateNoSelf      bool
	updateForceSelf   bool
	updatePre         bool
	updateHandedOffBy string
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update Anamnesia and reconcile the installation with it",
	Long: "Update Anamnesia and bring the installation in line with it.\n\n" +
		"Compares this build against the latest GitHub release and, when a newer\n" +
		"one exists, downloads it, verifies its checksum, replaces this binary and\n" +
		"hands the rest of the update to it. Then re-points Claude Code's hooks,\n" +
		"refreshes the hook set, pulls the database image, applies migrations,\n" +
		"restarts the server, and finishes with a health check.\n\n" +
		"  --check           report whether an update exists, change nothing\n" +
		"  --pre             consider prereleases too, not just stable releases\n" +
		"  --no-self-update  reconcile the installed binary, download nothing\n" +
		"  --force           replace a locally built binary too\n\n" +
		"Safe to run at any time; every step is idempotent.",
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().BoolVar(&updateSkipPull, "no-pull", false, "skip pulling the postgres image")
	updateCmd.Flags().BoolVar(&updateSkipHooks, "no-hooks", false, "skip refreshing Claude Code's config")
	updateCmd.Flags().StringVar(&updateScope, "scope", "user", "hook scope to refresh: user or project")
	updateCmd.Flags().StringVar(&updateConfigDir, "config-dir", "", "override the config directory (testing escape hatch)")
	updateCmd.Flags().BoolVar(&updateCheckOnly, "check", false, "only report whether a newer release exists")
	updateCmd.Flags().BoolVar(&updateNoSelf, "no-self-update", false, "do not download a new binary, only reconcile this one")
	updateCmd.Flags().BoolVar(&updateForceSelf, "force", false, "download the latest release even when this is not a released build")
	updateCmd.Flags().BoolVar(&updatePre, "pre", false, "consider prereleases as well as stable releases")
	// Set on the hand-off run so the new binary knows not to look again.
	updateCmd.Flags().StringVar(&updateHandedOffBy, "handed-off-by", "", "internal: version that performed the self-update")
	_ = updateCmd.Flags().MarkHidden("handed-off-by")
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	if updateCheckOnly {
		return checkOnly(ctx, out, updatePre)
	}

	self, err := selfPath()
	if err != nil {
		return err
	}
	if updateHandedOffBy != "" {
		fmt.Fprintf(out, "Continuing the update as %s (was %s)\n", version, updateHandedOffBy)
	} else {
		fmt.Fprintf(out, "Updating Anamnesia (running %s)\n", version)
	}
	fmt.Fprintf(out, "  binary: %s\n", self)

	// Replace the binary first, then hand the rest of the update to it. The
	// steps that follow write the version into Claude Code's hooks and enforce
	// the schema, so they have to run as the version that will actually serve.
	selfUpdateFailed := false
	if !updateNoSelf && updateHandedOffBy == "" {
		result, err := selfUpdate(ctx, out, updateForceSelf, updatePre)
		if err != nil {
			selfUpdateFailed = true
			// Failing to upgrade the binary is not a reason to abandon the
			// reconcile: the local install may still need repairing, and often
			// that is why someone ran this. Say plainly that the binary did not
			// change, so nobody reads the success line below as an upgrade.
			fmt.Fprintf(out, "  self-update did not happen: %v\n", err)
			fmt.Fprintf(out, "  continuing to reconcile the installed version (%s)\n", version)
		} else if result.Replaced {
			return handOffToNewBinary(cmd, result.SelfPath)
		}
	}

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
		installF.noCompletion = false
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
	if selfUpdateFailed {
		// The installation was reconciled, but the binary is the one we
		// started with. Saying "update complete" here reads as a successful
		// upgrade and is how someone ends up believing they are on a version
		// they are not.
		fmt.Fprintf(out, "Reconciled, but the binary was NOT updated: still %s.\n", version)
		fmt.Fprintf(out, "To finish upgrading, run this on a terminal: %s\n", retryCommand(updateForceSelf, updatePre))
		return nil
	}
	fmt.Fprintln(out, "Update complete. Run `anamnesia doctor` for a full check.")
	return nil
}

// handOffToNewBinary re-runs `update` using the binary that was just
// installed, passing the flags the user gave plus a marker so it does not look
// for another release. Its exit status becomes ours.
//
// Only flags that still mean something after the download are forwarded.
// --pre and --force choose *what* to fetch, and the handed-off run fetches
// nothing, so passing them on is at best noise and at worst fatal: the binary
// being handed to is a different version, and it may not know a flag this one
// does.
//
// That version gap is also why a rejected argument falls back to a minimal
// invocation rather than stranding the user with a freshly installed binary
// and a half-finished update.
func handOffToNewBinary(cmd *cobra.Command, self string) error {
	args := []string{"update", "--handed-off-by", version, "--scope", updateScope}
	if updateSkipPull {
		args = append(args, "--no-pull")
	}
	if updateSkipHooks {
		args = append(args, "--no-hooks")
	}
	if updateConfigDir != "" {
		args = append(args, "--config-dir", updateConfigDir)
	}
	if rf.verbose > 0 {
		args = append(args, "-"+strings.Repeat("v", rf.verbose))
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out)

	run := func(argv []string) ([]byte, error) {
		next := exec.Command(self, argv...)
		next.Stdin = os.Stdin
		// Captured rather than streamed so a failed first attempt does not
		// print a confusing error the fallback then contradicts.
		return next.CombinedOutput()
	}

	combined, err := run(args)
	if err != nil && strings.Contains(string(combined), "unknown flag") {
		fmt.Fprintf(out, "  %s does not accept every option this version passes; retrying with the essentials\n", version)
		combined, err = run([]string{"update", "--handed-off-by", version})
	}
	_, _ = out.Write(combined)
	if err != nil {
		return fmt.Errorf("the new binary is installed, but finishing the update failed: %w\nRun `anamnesia update` again", err)
	}
	return nil
}
