// config_cmd.go is the CLI over ~/.anamnesia/config.toml.
//
//	anamnesia config                          show the resolved config
//	anamnesia config path                     print the file location
//	anamnesia config get llm.provider
//	anamnesia config set llm.provider openrouter
//	anamnesia config openrouter.api_key sk-…  shorthand for set
//	anamnesia config edit                     open it in $EDITOR
//
// --global (the default) writes ~/.anamnesia/config.toml; --project writes
// ./.anamnesia.toml next to the repository root, for per-project overrides
// such as the project slug.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	configGlobalFlag  bool
	configProjectFlag bool
	configSecretsFlag bool
)

var configCmd = &cobra.Command{
	Use:   "config [key] [value]",
	Short: "Read and write Anamnesia settings",
	Long: "Show the resolved configuration, or set a value.\n\n" +
		"Every setting is a dotted key: `anamnesia config set embed.dims 3072`.\n" +
		"Values can equally be edited by hand in the config file, which is\n" +
		"commented; `anamnesia config path` prints where it lives.",
	Args: cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch len(args) {
		case 0:
			return runConfigList(cmd)
		case 1:
			return runConfigGet(cmd, args[0])
		default:
			return runConfigSet(cmd, args[0], args[1])
		}
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Print one resolved value",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runConfigGet(cmd, args[0]) },
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Write one value to the config file",
	Args:  cobra.ExactArgs(2),
	RunE:  func(cmd *cobra.Command, args []string) error { return runConfigSet(cmd, args[0], args[1]) },
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show every setting, its value and where it came from",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return runConfigList(cmd) },
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the config file location",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := targetConfigPath()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), path)
		return nil
	},
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open the config file in $EDITOR",
	Args:  cobra.NoArgs,
	RunE:  runConfigEdit,
}

func init() {
	for _, c := range []*cobra.Command{configCmd, configGetCmd, configSetCmd, configListCmd, configPathCmd, configEditCmd} {
		c.Flags().BoolVar(&configGlobalFlag, "global", false, "use ~/.anamnesia/config.toml (default)")
		c.Flags().BoolVar(&configProjectFlag, "project", false, "use ./.anamnesia.toml for this repository")
	}
	configCmd.Flags().BoolVar(&configSecretsFlag, "show-secrets", false, "print API keys and passwords in full")
	configListCmd.Flags().BoolVar(&configSecretsFlag, "show-secrets", false, "print API keys and passwords in full")

	configCmd.AddCommand(configGetCmd, configSetCmd, configListCmd, configPathCmd, configEditCmd)
}

// targetConfigPath is the file a write applies to.
func targetConfigPath() (string, error) {
	if configProjectFlag {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(gitToplevelOrCWD(wd), projectConfigName), nil
	}
	return globalConfigPath()
}

func runConfigGet(cmd *cobra.Command, key string) error {
	s, ok := settingByKey[key]
	if !ok {
		return unknownKeyError(key)
	}
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	v := hc.Get(key)
	if s.Kind == kSecret && !configSecretsFlag {
		v = s.mask(v)
	}
	fmt.Fprintln(cmd.OutOrStdout(), v)
	return nil
}

func runConfigSet(cmd *cobra.Command, key, value string) error {
	if _, ok := settingByKey[key]; !ok {
		return unknownKeyError(key)
	}
	path, err := targetConfigPath()
	if err != nil {
		return err
	}
	if _, err := ensureHome(); err != nil {
		return err
	}
	if err := setConfigValue(path, key, value); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✦ %s set in %s\n", key, path)

	// Changes that only take effect on restart should say so, rather than
	// leaving the user wondering why nothing happened.
	if s := settingByKey[key]; s.Env != "" || strings.HasPrefix(key, "postgres.") || strings.HasPrefix(key, "server.") {
		hc, err := loadHostConfig()
		if err == nil && serverResponding(cmd.Context(), hc, time.Second) {
			fmt.Fprintln(out, "  run `anamnesia restart` to apply it to the running server")
		}
	}
	if key == "embed.dims" {
		fmt.Fprintln(out, "  run `anamnesia migrate --dims "+value+"` to rebuild the schema for the new width")
	}
	return nil
}

func runConfigList(cmd *cobra.Command) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "global:  %s\n", hc.GlobalPath)
	if hc.ProjectPath != "" {
		fmt.Fprintf(out, "project: %s\n", hc.ProjectPath)
	}
	fmt.Fprintf(out, "url:     %s\n", hc.ServerURL())
	fmt.Fprintln(out)

	width := 0
	for _, s := range settings {
		if len(s.Key) > width {
			width = len(s.Key)
		}
	}
	lastSection := ""
	for _, s := range settings {
		if sec := s.section(); sec != lastSection {
			if lastSection != "" {
				fmt.Fprintln(out)
			}
			lastSection = sec
		}
		v := hc.Get(s.Key)
		if s.Kind == kSecret && !configSecretsFlag {
			v = s.mask(v)
		}
		shown := v
		if shown == "" {
			shown = "(unset)"
		}
		src := ""
		if o := hc.Origin(s.Key); o != fromDefault {
			src = "  [" + string(o) + "]"
		}
		fmt.Fprintf(out, "  %-*s  %s%s\n", width, s.Key, shown, src)
	}

	if len(hc.Unknown) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "unrecognised keys (ignored, check for typos):")
		sort.Strings(hc.Unknown)
		for _, u := range hc.Unknown {
			fmt.Fprintf(out, "  %s\n", u)
		}
	}
	return nil
}

func runConfigEdit(cmd *cobra.Command, _ []string) error {
	path, err := targetConfigPath()
	if err != nil {
		return err
	}
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		return fmt.Errorf("no $EDITOR set. The file is at %s", path)
	}
	parts := strings.Fields(editor)
	c := exec.Command(parts[0], append(parts[1:], path)...) //nolint:gosec // user's own editor
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
	return c.Run()
}

func unknownKeyError(key string) error {
	var near []string
	for _, k := range knownKeys() {
		if strings.Contains(k, key) || strings.Contains(key, strings.SplitN(k, ".", 2)[0]) {
			near = append(near, k)
		}
	}
	msg := fmt.Sprintf("unknown setting %q", key)
	if len(near) > 0 {
		return fmt.Errorf("%s\n\ndid you mean:\n  %s", msg, strings.Join(near, "\n  "))
	}
	return fmt.Errorf("%s\n\nknown settings:\n  %s", msg, strings.Join(knownKeys(), "\n  "))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
