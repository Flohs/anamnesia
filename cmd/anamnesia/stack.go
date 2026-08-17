// stack.go runs the local stack: the Postgres container plus the server
// process.
//
// The server is this same binary re-executed as `anamnesia serve`, detached
// and logging to ~/.anamnesia/server.log. That is what makes the CLI, the
// hooks and the server one artifact: there is no second thing to upgrade,
// so the host binary and the server can never disagree about the protocol
// between them.
//
// Hooks call ensureServerRunning, which is why the start path has to be
// safe under concurrency (several hooks can fire at once) and must never
// block a session for long.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/flohs/anamnesia/internal/httpapi"
)

var (
	stopAllFlag    bool
	logsFollowFlag bool
	logsLinesFlag  int
	statusJSONFlag bool
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the local stack (postgres container + server)",
	Long: "Bring up everything Anamnesia needs: the Postgres container it manages\n" +
		"and the background server process. Safe to run when already running.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		hc, err := loadHostConfig()
		if err != nil {
			return err
		}
		if err := requireConfigured(hc); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Starting Anamnesia")
		if err := startStack(cmd.Context(), hc, out); err != nil {
			return err
		}
		return reportHealth(cmd.Context(), hc, out)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the server (and the postgres container with --all)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		hc, err := loadHostConfig()
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Stopping Anamnesia")
		if err := stopServer(hc, out); err != nil {
			// Not being able to stop someone else's server is worth saying,
			// but it should not stop us from bringing postgres down.
			if !errors.Is(err, errUnmanagedServer) {
				return err
			}
			fmt.Fprintf(out, "  %v\n", err)
		}
		if stopAllFlag {
			return stopPostgres(cmd.Context(), hc, out)
		}
		fmt.Fprintln(out, "  postgres left running (use --all to stop it too)")
		return nil
	},
}

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the server",
	RunE: func(cmd *cobra.Command, _ []string) error {
		hc, err := loadHostConfig()
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if err := stopServer(hc, out); err != nil {
			return err
		}
		if err := startStack(cmd.Context(), hc, out); err != nil {
			return err
		}
		return reportHealth(cmd.Context(), hc, out)
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the stack is running",
	RunE:  runStatus,
}

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show the server log",
	RunE:  runLogs,
}

func init() {
	stopCmd.Flags().BoolVar(&stopAllFlag, "all", false, "also stop the postgres container")
	logsCmd.Flags().BoolVarP(&logsFollowFlag, "follow", "f", false, "keep printing new lines")
	logsCmd.Flags().IntVarP(&logsLinesFlag, "lines", "n", 50, "how many trailing lines to print")
	statusCmd.Flags().BoolVar(&statusJSONFlag, "json", false, "print machine-readable status")
}

// requireConfigured fails early when setup has never run, rather than
// producing a confusing database error later.
func requireConfigured(hc *hostConfig) error {
	if !fileExists(hc.GlobalPath) {
		return fmt.Errorf("no config at %s\n\nRun `anamnesia setup` first", hc.GlobalPath)
	}
	if hc.ManagesPostgres() && hc.Get("postgres.password") == "" {
		return fmt.Errorf("postgres.password is empty in %s\n\nRun `anamnesia setup` to generate one", hc.GlobalPath)
	}
	return nil
}

// startStack ensures Postgres is up, then the server.
func startStack(ctx context.Context, hc *hostConfig, out io.Writer) error {
	if err := ensurePostgres(ctx, hc, out); err != nil {
		return err
	}
	if serverResponding(ctx, hc, 1500*time.Millisecond) {
		// Distinguish our server from someone else's. A server we did not
		// start is running whatever configuration it was given, which is the
		// difference between "already done" and "your change did not apply".
		if pid, err := readPID(); err == nil && processAlive(pid) {
			fmt.Fprintln(out, "  server already running")
		} else {
			fmt.Fprintf(out, "  a server is already answering on %s, but this CLI did not start it\n", hc.ServerURL())
			fmt.Fprintln(out, "  its configuration may differ; `anamnesia stop` explains how to take over")
		}
		return nil
	}
	cmd, err := spawnServer(hc, out)
	if err != nil {
		return err
	}
	// Reaping the child in a goroutine is what makes a failed startup
	// visible immediately. A released process that exits becomes a zombie,
	// and a zombie still answers signal 0, so polling liveness cannot tell
	// the difference between "still booting" and "already dead".
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return waitServerResponding(ctx, hc, exited, 60*time.Second)
}

// spawnServer re-executes this binary as a detached `serve` process.
func spawnServer(hc *hostConfig, out io.Writer) (*exec.Cmd, error) {
	self, err := selfPath()
	if err != nil {
		return nil, err
	}
	if _, err := ensureHome(); err != nil {
		return nil, err
	}
	logPath, err := serverLogPath()
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(self, "serve", "-v")
	cmd.Env = hc.ServerEnv()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	// The child is in its own session (see detachAttr), so it outlives this
	// process without being released. Keeping the handle is what lets the
	// caller Wait on it and notice a startup failure straight away.
	pid := cmd.Process.Pid
	if err := writePID(pid); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "  server started (pid %d, log %s)\n", pid, logPath)
	return cmd, nil
}

// errUnmanagedServer means something is serving on our URL that we have no
// pid for, so we cannot stop it.
//
// `stop` treats that as a warning, but `restart` has to treat it as a
// failure: silently leaving the old process running would report a
// successful restart while the user's configuration change never took
// effect.
var errUnmanagedServer = errors.New("a server is responding but was not started by this CLI")

// stopServer signals the recorded server process and waits for it to go.
func stopServer(hc *hostConfig, out io.Writer) error {
	pid, readErr := readPID()
	if readErr != nil || pid <= 0 {
		if serverResponding(context.Background(), hc, time.Second) {
			return fmt.Errorf("%w on %s.\nStop it yourself, then try again (`pkill -f 'anamnesia serve'` if it is an orphan)",
				errUnmanagedServer, hc.ServerURL())
		}
		fmt.Fprintln(out, "  server not running")
		return clearPID()
	}
	if !processAlive(pid) {
		if err := clearPID(); err != nil {
			return err
		}
		if serverResponding(context.Background(), hc, time.Second) {
			return fmt.Errorf("%w on %s (recorded pid %d is gone).\nStop it yourself, then try again",
				errUnmanagedServer, hc.ServerURL(), pid)
		}
		fmt.Fprintln(out, "  server not running (stale pid file)")
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal server: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			fmt.Fprintf(out, "  server stopped (pid %d)\n", pid)
			return clearPID()
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("server (pid %d) did not exit within 15s", pid)
}

// ─── liveness ────────────────────────────────────────────────────────

// serverResponding reports whether anything answers /v1/health.
func serverResponding(ctx context.Context, hc *hostConfig, timeout time.Duration) bool {
	_, err := fetchHealth(ctx, hc, timeout)
	return err == nil
}

// fetchHealth reads /v1/health. A 503 is still a successful read: the
// server is up and telling us what is wrong, which is more useful than a
// dial error.
func fetchHealth(ctx context.Context, hc *hostConfig, timeout time.Duration) (*httpapi.HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hc.ServerURL()+"/v1/health", nil)
	if err != nil {
		return nil, err
	}
	res, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var health httpapi.HealthResponse
	if err := json.Unmarshal(body, &health); err != nil {
		return nil, fmt.Errorf("unexpected reply from %s: %s", hc.ServerURL(), strings.TrimSpace(string(body)))
	}
	return &health, nil
}

// waitServerResponding polls until the server answers, the process dies, or
// time runs out.
//
// Watching the process matters: the server refuses to start on a schema it
// cannot write to, and waiting out a 60s timeout for a process that already
// exited hides the one thing the user needs to read.
func waitServerResponding(ctx context.Context, hc *hostConfig, exited <-chan error, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if serverResponding(ctx, hc, time.Second) {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("the server exited during startup:\n\n%s", serverLogTail(12))
		default:
		}
		if time.Now().After(deadline) {
			logPath, _ := serverLogPath()
			return fmt.Errorf("server did not answer on %s within %s; see %s",
				hc.ServerURL(), limit, logPath)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// serverLogTail returns the last n lines of the server log, indented, for
// embedding in an error message.
func serverLogTail(n int) string {
	path, err := serverLogPath()
	if err != nil {
		return "(no log available)"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(no log at " + path + ")"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// reportHealth prints the health payload, and fails when it is not ok so
// `anamnesia start` cannot report success over a broken stack.
func reportHealth(ctx context.Context, hc *hostConfig, out io.Writer) error {
	health, err := fetchHealth(ctx, hc, 5*time.Second)
	if err != nil {
		return err
	}
	if health.OK {
		fmt.Fprintf(out, "  ready on %s (schema v%d, embed %s/%d)\n",
			hc.ServerURL(), health.MigrationVersion, health.EmbedProvider, health.SchemaEmbedDims)
		return nil
	}
	fmt.Fprintln(out, "  server is up but reports problems:")
	for _, p := range health.Problems {
		fmt.Fprintf(out, "    - %s\n", p)
	}
	return errors.New("stack started but is not healthy")
}

// ─── auto-start for hooks ────────────────────────────────────────────

// ensureServerRunning is what the hooks call. It never returns an error
// that should reach the user's session: a memory system that cannot start
// must degrade to doing nothing, not to blocking Claude Code.
//
// wait is how long the caller is willing to spend. SessionStart can afford
// a few seconds to get memory into the first prompt; a per-prompt hook
// cannot, and passes zero to mean "kick it off and carry on".
func ensureServerRunning(ctx context.Context, hc *hostConfig, wait time.Duration) bool {
	if serverResponding(ctx, hc, 800*time.Millisecond) {
		return true
	}
	if !hc.Bool("server.autostart") {
		return false
	}
	// One starter at a time: several hooks can fire within the same second
	// and `docker run` twice with the same container name fails noisily.
	locked, release := acquireStartLock()
	if locked {
		defer release()
		go func() {
			// Detached from the caller's deadline on purpose: the start
			// should finish even though this hook is about to exit.
			bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			_ = startStack(bg, hc, io.Discard)
		}()
	}
	if wait <= 0 {
		return false
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if serverResponding(ctx, hc, 500*time.Millisecond) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// startLockStale is how long a lock file is honoured before being taken
// over, so a killed starter cannot wedge auto-start permanently.
const startLockStale = 3 * time.Minute

// acquireStartLock takes the exclusive start lock. The release function is
// only meaningful when locked is true.
func acquireStartLock() (locked bool, release func()) {
	path, err := startLockPath()
	if err != nil {
		return false, func() {}
	}
	if _, err := ensureHome(); err != nil {
		return false, func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > startLockStale {
			_ = os.Remove(path)
			return acquireStartLock()
		}
		return false, func() {}
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close()
	return true, func() { _ = os.Remove(path) }
}

// ─── pid file ────────────────────────────────────────────────────────

func writePID(pid int) error {
	path, err := serverPIDPath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

func readPID() (int, error) {
	path, err := serverPIDPath()
	if err != nil {
		return 0, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

func clearPID() error {
	path, err := serverPIDPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// processAlive reports whether a pid is a live process. Signal 0 performs
// the permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// selfPath returns the absolute, symlink-resolved path of this binary. The
// hooks are written with this rather than the bare name `anamnesia`,
// because a hook runs in whatever shell Claude Code spawns and that shell's
// PATH is frequently not the one from an interactive terminal.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate own binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// ─── status and logs ─────────────────────────────────────────────────

func runStatus(cmd *cobra.Command, _ []string) error {
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	pgState := containerState("unmanaged")
	if hc.ManagesPostgres() {
		if s, err := inspectContainer(ctx, hc.Get("postgres.container")); err == nil {
			pgState = s
		} else {
			pgState = stateMissing
		}
	}
	health, healthErr := fetchHealth(ctx, hc, 3*time.Second)
	pid, _ := readPID()

	if statusJSONFlag {
		payload := map[string]any{
			"config":   hc.GlobalPath,
			"url":      hc.ServerURL(),
			"postgres": string(pgState),
			"pid":      pid,
			"server":   "down",
		}
		if healthErr == nil {
			payload["server"] = "up"
			payload["health"] = health
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	fmt.Fprintln(out, "Anamnesia status")
	fmt.Fprintf(out, "  config:   %s\n", hc.GlobalPath)
	fmt.Fprintf(out, "  url:      %s\n", hc.ServerURL())
	fmt.Fprintf(out, "  postgres: %s\n", pgState)
	switch {
	case healthErr != nil:
		fmt.Fprintf(out, "  server:   down (%s)\n", healthErr)
	case health.OK:
		fmt.Fprintf(out, "  server:   up, healthy (pid %d, schema v%d)\n", pid, health.MigrationVersion)
	default:
		fmt.Fprintf(out, "  server:   up, unhealthy (pid %d)\n", pid)
		for _, p := range health.Problems {
			fmt.Fprintf(out, "            - %s\n", p)
		}
	}
	return nil
}

func runLogs(cmd *cobra.Command, _ []string) error {
	path, err := serverLogPath()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "no log yet at %s (run `anamnesia start`)\n", path)
			return nil
		}
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if logsLinesFlag > 0 && len(lines) > logsLinesFlag {
		lines = lines[len(lines)-logsLinesFlag:]
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	if !logsFollowFlag {
		return nil
	}
	offset := int64(len(raw))
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return err
		}
		grown, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if len(grown) > 0 {
			offset += int64(len(grown))
			fmt.Fprint(out, string(grown))
		}
	}
}
