// project.go: `anamnesia project move`, which refiles one repository's
// memories under a different project slug.
//
// A slug defaults to the repository directory name, so one product built
// in several repositories becomes several projects that cannot see each
// other: the read path scopes to `project_id = $n OR project_id IS NULL`,
// and consolidation clusters strictly inside a scope. Pointing them at a
// shared slug is the fix, and it has two halves that only work together.
// Moving the rows without writing .anamnesia.toml leaves the next session
// filing under the old name again; writing the file first makes the old
// slug unresolvable, so there is nothing left to move. This command does
// both, in that order.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/flohs/anamnesia/internal/store"
)

var (
	projectMoveFrom   string
	projectMoveApply  bool
	projectPruneApply bool
)

var projectCmd = func() *cobra.Command {
	c := &cobra.Command{
		Use:   "project",
		Short: "Inspect and reorganise project scopes",
	}
	c.AddCommand(projectMoveCmd())
	c.AddCommand(projectPruneCmd())
	return c
}()

func projectMoveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "move <to>",
		Short: "Refile this repository's memories under another project",
		Long: "Move every memory filed under this repository's project into <to>,\n" +
			"and write .anamnesia.toml so future sessions file there too.\n\n" +
			"The project moved FROM is the one this directory resolves to, the same\n" +
			"way the hooks resolve it. Reports what would move and exits; pass\n" +
			"--apply to carry it out.",
		Args:              cobra.ExactArgs(1),
		RunE:              runProjectMove,
		ValidArgsFunction: completeProjectSlugArg,
	}
	c.Flags().StringVar(&projectMoveFrom, "from", "", "move this project instead of the one this directory resolves to")
	c.Flags().BoolVar(&projectMoveApply, "apply", false, "carry the move out instead of only reporting it")
	return c
}

func runProjectMove(cmd *cobra.Command, args []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	to := sanitizeSlug(args[0])
	if to == "" {
		return fmt.Errorf("%q is not a usable project slug", args[0])
	}
	from := sanitizeSlug(projectMoveFrom)
	if from == "" {
		from = hc.Project()
	}
	out := cmd.OutOrStdout()
	if from == to {
		fmt.Fprintf(out, "already filed under %q; nothing to move\n", to)
		return nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := gitToplevelOrCWD(wd)

	ctx := context.Background()
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return err
	}
	defer st.Close()

	userID, found, err := st.LookupUser(ctx, hc.User())
	if err != nil {
		return err
	}
	if !found {
		// No memories at all yet. Refiling the repository is still
		// meaningful: it decides where the first ones will land.
		return finishProjectMove(cmd, dir, from, to, nil)
	}

	plan, err := st.PlanProjectMove(ctx, userID, from, to)
	if err != nil {
		return err
	}
	printMovePlan(out, plan)

	if blockers := plan.Blockers(); len(blockers) > 0 {
		for _, b := range blockers {
			fmt.Fprintf(out, "  ✗ %s\n", b)
		}
		return fmt.Errorf("cannot move %q into %q while the conflicts above stand", from, to)
	}
	if !projectMoveApply {
		fmt.Fprintf(out, "\nNothing was changed. Run again with --apply to carry it out.\n")
		return nil
	}
	if err := st.ApplyProjectMove(ctx, userID, plan); err != nil {
		return err
	}
	return finishProjectMove(cmd, dir, from, to, plan)
}

// finishProjectMove writes the project file, which is what makes the move
// stick. Done after the rows have moved: the slug this directory resolves
// to is what the move reads as its source.
func finishProjectMove(cmd *cobra.Command, dir, from, to string, plan *store.ProjectMovePlan) error {
	out := cmd.OutOrStdout()
	if !projectMoveApply {
		fmt.Fprintf(out, "\nNothing was changed. Run again with --apply to carry it out.\n")
		return nil
	}
	path, err := refileProjectConfig(dir, to)
	if err != nil {
		return err
	}
	moved := 0
	if plan != nil {
		moved = plan.Total()
	}
	fmt.Fprintf(out, "\n✦ moved %d row(s) from %q to %q\n", moved, from, to)
	fmt.Fprintf(out, "  wrote %s, so sessions in this repository now file under %q\n", path, to)
	return nil
}

func printMovePlan(out interface{ Write([]byte) (int, error) }, plan *store.ProjectMovePlan) {
	if !plan.Found {
		fmt.Fprintf(out, "%q has no memories yet; only the project file needs writing\n", plan.From)
		return
	}
	fmt.Fprintf(out, "moving %q into %q\n", plan.From, plan.To)
	if plan.Total() == 0 {
		fmt.Fprintf(out, "  (no rows)\n")
		return
	}
	tables := make([]string, 0, len(plan.Counts))
	for t := range plan.Counts {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		fmt.Fprintf(out, "  %-16s %d\n", t, plan.Counts[t])
	}
}

// refileProjectConfig points .anamnesia.toml at `to`, creating the file
// if the repository has none. Updating in place rather than rewriting:
// the file is committed with the repository and may carry other
// overrides, and a comment-preserving write is what `config --project
// set` already does.
func refileProjectConfig(dir, to string) (string, error) {
	path := filepath.Join(dir, projectConfigName)
	if fileExists(path) {
		return path, setConfigValue(path, "identity.project", to)
	}
	return path, writeFileAtomic(path, renderProjectConfig(to))
}

func projectPruneCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "prune",
		Short: "Remove project entries that hold no memories",
		Long: "List the projects holding nothing at all — no sources, facts,\n" +
			"experiences, entities, skills, working memory or commitments — and,\n" +
			"with --apply, delete those entries.\n\n" +
			"Deleting one removes no memory, because there is none to remove. A\n" +
			"project reappears if a session there stores something later.",
		Args: cobra.NoArgs,
		RunE: runProjectPrune,
	}
	c.Flags().BoolVar(&projectPruneApply, "apply", false, "delete the entries instead of only listing them")
	return c
}

func runProjectPrune(cmd *cobra.Command, _ []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return err
	}
	defer st.Close()

	out := cmd.OutOrStdout()
	userID, found, err := st.LookupUser(ctx, hc.User())
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(out, "%q has no memories yet; nothing to prune\n", hc.User())
		return nil
	}
	empty, err := st.PrunableProjects(ctx, userID)
	if err != nil {
		return err
	}
	if len(empty) == 0 {
		fmt.Fprintf(out, "every project holds something; nothing to prune\n")
		return nil
	}
	// The project this directory resolves to is listed like any other, but
	// flagged: pruning it is harmless, and it will come back the moment
	// this repository stores anything.
	here := hc.Project()
	fmt.Fprintf(out, "%d project(s) hold nothing:\n", len(empty))
	slugs := make([]string, 0, len(empty))
	for _, p := range empty {
		note := ""
		if p.Slug == here {
			note = "   (this directory)"
		}
		fmt.Fprintf(out, "  %s%s\n", p.Slug, note)
		slugs = append(slugs, p.Slug)
	}
	if !projectPruneApply {
		fmt.Fprintf(out, "\nNothing was changed. Run again with --apply to delete these entries.\n")
		return nil
	}
	n, err := st.PruneProjects(ctx, userID, slugs)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✦ removed %d empty project entr%s\n", n, map[bool]string{true: "y", false: "ies"}[n == 1])
	return nil
}
