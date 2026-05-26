package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose Anamnesia config + connectivity",
	RunE:  runDoctor,
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	cfg, err := resolveHostConfig()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Anamnesia doctor")
	fmt.Fprintf(out, "  version:    %s\n", version)
	fmt.Fprintf(out, "  server:     %s\n", cfg.ServerURL)
	fmt.Fprintf(out, "  user:       %s\n", cfg.User)
	fmt.Fprintf(out, "  project:    %s\n", cfg.Project)
	if cfg.ServerToken == "" {
		fmt.Fprintln(out, "  token:      <none> (server is in unauthenticated mode)")
	} else {
		fmt.Fprintln(out, "  token:      set via ANAMNESIA_SERVER_TOKEN/config")
	}

	// Health probe.
	fmt.Fprint(out, "  /v1/health: ")
	if ok, msg := probeHealth(cfg); ok {
		fmt.Fprintf(out, "OK (%s)\n", msg)
	} else {
		fmt.Fprintf(out, "FAIL (%s)\n", msg)
	}

	// Hook + MCP inspection.
	if home, err := os.UserHomeDir(); err == nil {
		settings := filepath.Join(home, ".claude", "settings.json")
		fmt.Fprintf(out, "  hooks:      %s — %s\n", settings, summariseHooks(settings))
		mcp := filepath.Join(home, ".claude.json")
		fmt.Fprintf(out, "  mcp:        %s — %s\n", mcp, summariseMCP(mcp))
	}
	return nil
}

func probeHealth(cfg *hostConfig) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", cfg.ServerURL+"/v1/health", nil)
	if err != nil {
		return false, err.Error()
	}
	res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return false, fmt.Sprintf("%s: %s", res.Status, string(b))
	}
	return true, fmt.Sprintf("status %d", res.StatusCode)
}

func summariseHooks(path string) string {
	obj, err := readJSONObject(path)
	if err != nil {
		return err.Error()
	}
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		return "no hooks block"
	}
	managed := 0
	for _, raw := range hooks {
		entries, _ := raw.([]any)
		for _, e := range entries {
			em, _ := e.(map[string]any)
			if v, _ := em[managedKey].(bool); v {
				managed++
			}
		}
	}
	if managed == 0 {
		return "no Anamnesia hooks (run `anamnesia install`)"
	}
	return fmt.Sprintf("%d managed entries", managed)
}

func summariseMCP(path string) string {
	obj, err := readJSONObject(path)
	if err != nil {
		return err.Error()
	}
	servers, _ := obj["mcpServers"].(map[string]any)
	if servers == nil {
		return "no mcpServers block"
	}
	ana, ok := servers["anamnesia"].(map[string]any)
	if !ok {
		return "no anamnesia entry (run `anamnesia install`)"
	}
	url, _ := ana["url"].(string)
	if url == "" {
		return "anamnesia entry present but no url"
	}
	b, _ := json.Marshal(ana)
	return string(b)
}
