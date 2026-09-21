package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var uiNoOpenFlag bool

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Open the console in a browser",
	Long: `Open the console in a browser.

The console ships inside this binary and is served by the running server, so
there is nothing else to install and nothing else to start. This command
makes sure the stack is up and points a browser at it.`,
	RunE: runUI,
}

func init() {
	uiCmd.Flags().BoolVar(&uiNoOpenFlag, "no-open", false,
		"print the URL instead of opening a browser")
}

func runUI(cmd *cobra.Command, _ []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	if err := requireConfigured(hc); err != nil {
		return err
	}
	ctx, out := cmd.Context(), cmd.OutOrStdout()

	if !ensureServerRunning(ctx, hc, 20*time.Second) {
		return fmt.Errorf("the server at %s is not responding: start it with `anamnesia start`, then `anamnesia logs` if it does not come up",
			hc.ServerURL())
	}

	link, err := consoleLink(ctx, hc.ServerURL(), hc.Get("server.token"))
	if err != nil {
		return err
	}

	if uiNoOpenFlag {
		fmt.Fprintln(out, link)
		return nil
	}
	if err := openBrowser(link); err != nil {
		// Not a failure: over SSH, or in a container, there is no browser
		// to open and the URL is the whole answer.
		fmt.Fprintf(out, "Could not open a browser (%v). Open this yourself:\n\n  %s\n", err, link)
		return nil
	}
	fmt.Fprintf(out, "Opened the console at %s\n", hc.ServerURL())
	return nil
}

// consoleLink is the URL to open.
//
// A browser cannot send a bearer token, so where one is configured the CLI
// spends the token it already holds on a single-use link, which the server
// exchanges for a session cookie. Without a token nothing is protected and
// there is nothing to exchange.
func consoleLink(ctx context.Context, base, token string) (string, error) {
	base = strings.TrimRight(base, "/")
	if strings.TrimSpace(token) == "" {
		return base, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/console/session", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking %s for a console link: %w", base, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", fmt.Errorf("%s refused the configured server.token; check it against the server that is actually running", base)
	case http.StatusNotFound:
		return "", fmt.Errorf("%s has no console, so it predates the one in this binary: `anamnesia update` it, then restart", base)
	default:
		return "", fmt.Errorf("asking %s for a console link: %s", base, resp.Status)
	}

	var body struct {
		Nonce string `json:"nonce"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("reading the console link from %s: %w", base, err)
	}
	if body.Nonce == "" {
		return "", fmt.Errorf("%s returned an empty console link", base)
	}
	return base + "/?k=" + url.QueryEscape(body.Nonce), nil
}

// browserCommand is the argv that opens a link in the platform's default
// browser. Separated from openBrowser so the mapping is testable on
// whichever platform happens to be running the tests.
func browserCommand(goos, link string) []string {
	switch goos {
	case "darwin":
		return []string{"open", link}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", link}
	default:
		return []string{"xdg-open", link}
	}
}

func openBrowser(link string) error {
	argv := browserCommand(runtime.GOOS, link)
	return exec.Command(argv[0], argv[1:]...).Start()
}
