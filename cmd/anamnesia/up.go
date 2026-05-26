// up.go / down.go are convenience wrappers around `docker compose`.
// They look for docker-compose.yml in:
//
//   1. ./docker-compose.yml
//   2. ${XDG_DATA_HOME}/anamnesia/docker-compose.yml or ~/.local/share/anamnesia/docker-compose.yml
//   3. The directory specified by --compose-dir
//
// `anamnesia up` is just sugar so a user who installed the binary via
// homebrew / a release tarball never has to remember the path.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	composeDir string
	upDetach   bool
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the local docker-compose stack",
	RunE:  runUp,
}

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the local docker-compose stack",
	RunE:  runDown,
}

func init() {
	upCmd.Flags().StringVar(&composeDir, "compose-dir", "", "directory containing docker-compose.yml")
	upCmd.Flags().BoolVar(&upDetach, "detach", true, "run docker compose up in the background")
	downCmd.Flags().StringVar(&composeDir, "compose-dir", "", "directory containing docker-compose.yml")
}

func runUp(cmd *cobra.Command, _ []string) error {
	dir, err := findComposeDir()
	if err != nil {
		return err
	}
	args := []string{"compose", "up", "--build"}
	if upDetach {
		args = append(args, "-d")
	}
	return runDocker(cmd, dir, args)
}

func runDown(cmd *cobra.Command, _ []string) error {
	dir, err := findComposeDir()
	if err != nil {
		return err
	}
	return runDocker(cmd, dir, []string{"compose", "down"})
}

func runDocker(cmd *cobra.Command, dir string, args []string) error {
	c := exec.Command("docker", args...)
	c.Dir = dir
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		return fmt.Errorf("docker %v: %w", args, err)
	}
	return nil
}

func findComposeDir() (string, error) {
	if composeDir != "" {
		if _, err := os.Stat(filepath.Join(composeDir, "docker-compose.yml")); err == nil {
			return composeDir, nil
		}
		return "", fmt.Errorf("no docker-compose.yml in %s", composeDir)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(wd, "docker-compose.yml")); err == nil {
		return wd, nil
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dataHome = filepath.Join(home, ".local", "share")
		}
	}
	if dataHome != "" {
		p := filepath.Join(dataHome, "anamnesia")
		if _, err := os.Stat(filepath.Join(p, "docker-compose.yml")); err == nil {
			return p, nil
		}
	}
	return "", errors.New("could not find docker-compose.yml; pass --compose-dir or cd into the anamnesia repo")
}
