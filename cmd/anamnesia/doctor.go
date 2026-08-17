// doctor.go verifies an installation end to end.
//
// The previous version reported "/v1/health: OK" and exited 0 against a
// server that could not store a single memory, and told users to run
// `anamnesia install` when their hooks were already installed — advice that
// duplicated every hook. Both failures had the same root cause: it checked
// what was easy to check rather than what had to be true.
//
// So every check here corresponds to something that has actually broken an
// install, doctor exits non-zero when any check fails, and --json makes it
// usable from a script or CI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/flohs/anamnesia/internal/httpapi"
)

type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
)

type check struct {
	Name    string      `json:"name"`
	Status  checkStatus `json:"status"`
	Message string      `json:"message"`
	Details []string    `json:"details,omitempty"`
	Fix     string      `json:"fix,omitempty"`
}

type doctorReport struct {
	Version string  `json:"version"`
	Checks  []check `json:"checks"`
	Failed  int     `json:"failed"`
	Warned  int     `json:"warned"`
}

var (
	doctorJSON      bool
	doctorDeep      bool
	doctorScope     string
	doctorConfigDir string
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Verify the installation and report what is wrong",
	Long: "Check every part of an Anamnesia install: configuration, Docker, the\n" +
		"database, the schema, the server, Claude Code's hooks and MCP entry,\n" +
		"and whether the hooks have actually been running.\n\n" +
		"Exits non-zero when any check fails, so it can gate a script.",
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "print the report as JSON")
	doctorCmd.Flags().BoolVar(&doctorDeep, "deep", false, "also write and read back a canary memory")
	doctorCmd.Flags().StringVar(&doctorScope, "scope", "user", "which hook scope to inspect: user or project")
	doctorCmd.Flags().StringVar(&doctorConfigDir, "config-dir", "", "override the config directory (testing escape hatch)")
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	report := doctorReport{Version: version}
	add := func(c check) { report.Checks = append(report.Checks, c) }

	hc, cfgErr := loadHostConfig()
	if cfgErr != nil {
		add(check{Name: "config", Status: statusFail, Message: cfgErr.Error(),
			Fix: "fix the file, or regenerate it by moving it aside and running `anamnesia setup`"})
		return finishDoctor(out, report)
	}
	add(checkConfig(hc))
	add(checkBinary())

	if hc.ManagesPostgres() {
		dockerCheck := checkDocker(ctx)
		add(dockerCheck)
		if dockerCheck.Status != statusFail {
			add(checkPostgresContainer(ctx, hc))
		}
	} else {
		add(check{Name: "postgres", Status: statusOK,
			Message: "external database (postgres.url is set); no container managed"})
	}

	serverCheck, health := checkServer(ctx, hc)
	add(serverCheck)
	if health != nil {
		add(checkSchema(health))
		add(checkQueue(ctx, hc))
	}
	add(checkHooks(hc))
	add(checkMCP(hc))
	add(checkHookActivity())
	if doctorDeep && health != nil {
		add(checkCanary(ctx, hc))
	}

	return finishDoctor(out, report)
}

func finishDoctor(out io.Writer, report doctorReport) error {
	for _, c := range report.Checks {
		switch c.Status {
		case statusFail:
			report.Failed++
		case statusWarn:
			report.Warned++
		}
	}
	if doctorJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "Anamnesia doctor (%s)\n\n", report.Version)
		width := 0
		for _, c := range report.Checks {
			if len(c.Name) > width {
				width = len(c.Name)
			}
		}
		for _, c := range report.Checks {
			fmt.Fprintf(out, "  %s %-*s  %s\n", lamp(c.Status), width, c.Name, c.Message)
			for _, d := range c.Details {
				fmt.Fprintf(out, "         %-*s  %s\n", width, "", d)
			}
			if c.Fix != "" && c.Status != statusOK {
				fmt.Fprintf(out, "         %-*s  → %s\n", width, "", c.Fix)
			}
		}
		fmt.Fprintln(out)
		switch {
		case report.Failed > 0:
			fmt.Fprintf(out, "%d failed, %d warnings.\n", report.Failed, report.Warned)
		case report.Warned > 0:
			fmt.Fprintf(out, "All essential checks passed, %d warnings.\n", report.Warned)
		default:
			fmt.Fprintln(out, "Everything checks out.")
		}
	}
	if report.Failed > 0 {
		// A non-zero exit is the point: this is what lets a setup script or
		// CI job treat a broken install as broken.
		return errDoctorFailed
	}
	return nil
}

// errDoctorFailed exits non-zero without printing another error line, since
// the report above already said everything.
var errDoctorFailed = &silentError{}

type silentError struct{}

func (*silentError) Error() string { return "" }

func lamp(s checkStatus) string {
	switch s {
	case statusOK:
		return "[ ok ]"
	case statusWarn:
		return "[warn]"
	default:
		return "[fail]"
	}
}

// ─── individual checks ───────────────────────────────────────────────

func checkConfig(hc *hostConfig) check {
	c := check{Name: "config", Status: statusOK, Message: hc.GlobalPath}
	if !fileExists(hc.GlobalPath) {
		return check{Name: "config", Status: statusFail,
			Message: "missing: " + hc.GlobalPath,
			Fix:     "run `anamnesia setup`"}
	}
	if hc.ProjectPath != "" {
		c.Details = append(c.Details, "project overrides: "+hc.ProjectPath)
	}
	if len(hc.Unknown) > 0 {
		sort.Strings(hc.Unknown)
		c.Status = statusWarn
		c.Message = hc.GlobalPath + " has unrecognised keys"
		c.Details = append(c.Details, hc.Unknown...)
		c.Fix = "remove or correct them; `anamnesia config list` shows every valid key"
	}
	if hc.ManagesPostgres() && hc.Get("postgres.password") == "" {
		c.Status = statusFail
		c.Message = "postgres.password is empty"
		c.Fix = "run `anamnesia setup` to generate one"
	}
	// A non-loopback listener with no token publishes the user's memory.
	addr := hc.Get("server.addr")
	if !strings.HasPrefix(addr, "127.0.0.1") && !strings.HasPrefix(addr, "localhost") && hc.Token() == "" {
		c.Status = statusWarn
		c.Details = append(c.Details, "server.addr "+addr+" is not loopback and server.token is empty")
		c.Fix = "set server.token, or bind server.addr to 127.0.0.1"
	}
	return c
}

func checkBinary() check {
	self, err := selfPath()
	if err != nil {
		return check{Name: "binary", Status: statusWarn, Message: "cannot resolve own path: " + err.Error()}
	}
	return check{Name: "binary", Status: statusOK, Message: fmt.Sprintf("%s (%s)", self, version)}
}

func checkDocker(ctx context.Context) check {
	if err := requireDocker(ctx); err != nil {
		return check{Name: "docker", Status: statusFail, Message: firstLine(err.Error()),
			Fix: "start Docker (Docker Desktop, OrbStack or colima), then `anamnesia start`"}
	}
	v, err := dockerVersion(ctx)
	if err != nil {
		return check{Name: "docker", Status: statusOK, Message: "available"}
	}
	return check{Name: "docker", Status: statusOK, Message: "engine " + v}
}

func checkPostgresContainer(ctx context.Context, hc *hostConfig) check {
	name := hc.Get("postgres.container")
	state, err := inspectContainer(ctx, name)
	if err != nil {
		return check{Name: "postgres", Status: statusFail, Message: err.Error()}
	}
	switch state {
	case stateRunning:
		return check{Name: "postgres", Status: statusOK,
			Message: fmt.Sprintf("container %s running on 127.0.0.1:%d", name, hc.Int("postgres.port"))}
	case stateStopped:
		return check{Name: "postgres", Status: statusFail,
			Message: "container " + name + " exists but is stopped", Fix: "run `anamnesia start`"}
	default:
		return check{Name: "postgres", Status: statusFail,
			Message: "container " + name + " does not exist", Fix: "run `anamnesia start`"}
	}
}

func checkServer(ctx context.Context, hc *hostConfig) (check, *httpapi.HealthResponse) {
	health, err := fetchHealth(ctx, hc, 5*time.Second)
	if err != nil {
		return check{Name: "server", Status: statusFail,
			Message: "not responding on " + hc.ServerURL(),
			Details: []string{err.Error()},
			Fix:     "run `anamnesia start`, then `anamnesia logs` if it still fails"}, nil
	}
	c := check{Name: "server", Status: statusOK,
		Message: fmt.Sprintf("responding on %s", hc.ServerURL())}
	if pid, err := readPID(); err == nil && processAlive(pid) {
		c.Details = append(c.Details, fmt.Sprintf("pid %d", pid))
	}
	// A server built from a different binary than the one answering means
	// a restart was skipped after an upgrade.
	if health.Version != "" && health.Version != version {
		c.Status = statusWarn
		c.Details = append(c.Details,
			fmt.Sprintf("server is running %s but this binary is %s", health.Version, version))
		c.Fix = "run `anamnesia restart`"
	}
	c.Details = append(c.Details,
		fmt.Sprintf("llm %s/%s, embed %s/%s", orNone(health.LLMProvider), orNone(health.LLMModel),
			orNone(health.EmbedProvider), orNone(health.EmbedModel)))
	if health.LLMProvider == "stub" {
		c.Details = append(c.Details, "llm.provider is stub, so nothing is extracted from your sessions")
	}
	return c, health
}

func checkSchema(h *httpapi.HealthResponse) check {
	c := check{Name: "schema", Status: statusOK,
		Message: fmt.Sprintf("v%d, vector(%d), database %s", h.MigrationVersion, h.SchemaEmbedDims, h.Database)}
	if h.OK {
		return c
	}
	c.Status = statusFail
	c.Message = "the server reports problems"
	c.Details = append(c.Details, h.Problems...)
	switch {
	case h.SchemaEmbedDims != h.ConfiguredEmbedDims && h.SchemaEmbedDims > 0:
		c.Fix = fmt.Sprintf("run `anamnesia migrate --dims %d`", h.ConfiguredEmbedDims)
	case len(h.MissingANNIndexes) > 0:
		c.Fix = fmt.Sprintf("run `anamnesia migrate --dims %d` to rebuild the indexes", h.ConfiguredEmbedDims)
	default:
		c.Fix = "see `anamnesia logs`"
	}
	return c
}

// checkQueue reads the pending counters. A backlog that never drains is the
// signature of embeddings failing on every write, which is precisely the
// fault the old health check could not see.
func checkQueue(ctx context.Context, hc *hostConfig) check {
	type pending struct {
		Extract int `json:"extract_pending"`
		Embed   int `json:"embed_pending"`
	}
	url := fmt.Sprintf("%s/v1/queue/pending?user=%s&project=%s",
		hc.ServerURL(), hc.User(), hc.Project())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return check{Name: "queue", Status: statusWarn, Message: err.Error()}
	}
	if tok := hc.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return check{Name: "queue", Status: statusWarn, Message: err.Error()}
	}
	defer res.Body.Close()
	var p pending
	if err := json.NewDecoder(res.Body).Decode(&p); err != nil {
		return check{Name: "queue", Status: statusWarn, Message: "unreadable reply: " + err.Error()}
	}
	c := check{Name: "queue", Status: statusOK,
		Message: fmt.Sprintf("%d sources awaiting extraction, %d rows awaiting embedding", p.Extract, p.Embed)}
	if p.Embed > 0 {
		c.Status = statusWarn
		c.Details = append(c.Details,
			"a backlog that never drains usually means embedding writes are failing")
		c.Fix = "re-run this in a minute; if the number has not moved, check `anamnesia logs`"
	}
	return c
}

// checkHooks inspects Claude Code's settings.
//
// Detection is by command, not by marker key: hooks written by an older
// version carry no marker, and reporting those as "not installed" is what
// previously led users to install a second copy of every hook.
func checkHooks(hc *hostConfig) check {
	paths, err := resolvePaths(doctorScope, doctorConfigDir)
	if err != nil {
		return check{Name: "hooks", Status: statusFail, Message: err.Error()}
	}
	obj, err := readJSONObject(paths.settings)
	if err != nil {
		return check{Name: "hooks", Status: statusFail,
			Message: fmt.Sprintf("cannot read %s: %v", paths.settings, err)}
	}
	hooks, _ := obj["hooks"].(map[string]any)

	type found struct {
		event    string
		command  string
		managed  bool
		stampVer string
	}
	var all []found
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			managed, _ := em[managedKey].(bool)
			if !managed && !entryHasAnamnesiaCommand(em) {
				continue
			}
			stamp, _ := em[managedVersionKey].(string)
			cmdStr := ""
			if hs, ok := em["hooks"].([]any); ok {
				for _, h := range hs {
					if hm, ok := h.(map[string]any); ok {
						if s, _ := hm["command"].(string); s != "" {
							cmdStr = s
						}
					}
				}
			}
			all = append(all, found{event: event, command: cmdStr, managed: managed, stampVer: stamp})
		}
	}

	c := check{Name: "hooks", Status: statusOK}
	if len(all) == 0 {
		return check{Name: "hooks", Status: statusFail,
			Message: "no Anamnesia hooks in " + paths.settings,
			Fix:     "run `anamnesia install`"}
	}

	// Duplicates mean every hook runs more than once per event.
	perEvent := map[string]int{}
	for _, f := range all {
		perEvent[f.event]++
	}
	var dupes []string
	for event, n := range perEvent {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%s has %d entries", event, n))
		}
	}
	sort.Strings(dupes)

	wantEvents := map[string]bool{}
	for _, h := range anamnesiaHooks {
		wantEvents[h.event] = true
	}
	var missing, stale []string
	for event := range wantEvents {
		if perEvent[event] == 0 {
			missing = append(missing, event)
		}
	}
	for _, f := range all {
		if !wantEvents[f.event] {
			stale = append(stale, f.event+" is no longer used by this version")
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	c.Message = fmt.Sprintf("%d entries in %s", len(all), paths.settings)

	self, selfErr := selfPath()
	var pathProblems []string
	for _, f := range all {
		tokens := shellFields(f.command)
		if len(tokens) == 0 {
			continue
		}
		bin := tokens[0]
		if !filepath.IsAbs(bin) {
			pathProblems = append(pathProblems,
				fmt.Sprintf("%s runs %q, which only works when PATH happens to include the binary", f.event, bin))
			continue
		}
		if !fileExists(bin) {
			pathProblems = append(pathProblems,
				fmt.Sprintf("%s runs %s, which does not exist", f.event, bin))
			continue
		}
		if selfErr == nil && bin != self {
			pathProblems = append(pathProblems,
				fmt.Sprintf("%s runs %s, but this binary is %s", f.event, bin, self))
		}
	}
	sort.Strings(pathProblems)

	var stamps []string
	for _, f := range all {
		if f.stampVer != "" && f.stampVer != version {
			stamps = append(stamps, fmt.Sprintf("%s was written by %s", f.event, f.stampVer))
		}
	}
	sort.Strings(stamps)

	switch {
	case len(dupes) > 0:
		c.Status = statusFail
		c.Details = append(c.Details, dupes...)
		c.Details = append(c.Details, "duplicate entries make every hook run more than once per event")
		c.Fix = "run `anamnesia install` to replace them with exactly one each"
	case len(missing) > 0:
		c.Status = statusFail
		c.Details = append(c.Details, "missing: "+strings.Join(missing, ", "))
		c.Fix = "run `anamnesia install`"
	case len(pathProblems) > 0:
		c.Status = statusFail
		c.Details = append(c.Details, pathProblems...)
		c.Fix = "run `anamnesia install` to rewrite them with this binary's absolute path"
	case len(stale) > 0 || len(stamps) > 0:
		c.Status = statusWarn
		c.Details = append(c.Details, stale...)
		c.Details = append(c.Details, stamps...)
		c.Fix = "run `anamnesia install` to refresh them"
	}
	return c
}

func checkMCP(hc *hostConfig) check {
	paths, err := resolvePaths(doctorScope, doctorConfigDir)
	if err != nil {
		return check{Name: "mcp", Status: statusFail, Message: err.Error()}
	}
	obj, err := readJSONObject(paths.mcp)
	if err != nil {
		return check{Name: "mcp", Status: statusFail,
			Message: fmt.Sprintf("cannot read %s: %v", paths.mcp, err)}
	}
	servers, _ := obj["mcpServers"].(map[string]any)
	entry, ok := servers["anamnesia"].(map[string]any)
	if !ok {
		return check{Name: "mcp", Status: statusFail,
			Message: "no anamnesia entry in " + paths.mcp,
			Fix:     "run `anamnesia install`"}
	}
	url, _ := entry["url"].(string)
	want := strings.TrimRight(hc.ServerURL(), "/") + "/mcp"
	if url != want {
		return check{Name: "mcp", Status: statusFail,
			Message: "configured URL does not match the server",
			Details: []string{"configured: " + url, "expected:   " + want},
			Fix:     "run `anamnesia install`"}
	}
	return check{Name: "mcp", Status: statusOK, Message: url}
}

// checkHookActivity reads the hook log. Without this, a hook failing on
// every single turn is indistinguishable from a healthy install, because
// hooks swallow their errors by design.
func checkHookActivity() check {
	entries, err := readHookLogTail(200)
	if err != nil {
		return check{Name: "hook runs", Status: statusWarn, Message: "cannot read hook log: " + err.Error()}
	}
	if len(entries) == 0 {
		return check{Name: "hook runs", Status: statusWarn,
			Message: "no hook has run yet",
			Fix:     "start a Claude Code session, then re-run this"}
	}
	lastOK := map[string]time.Time{}
	lastErr := map[string]hookLogEntry{}
	for _, e := range entries {
		if e.OK {
			if e.At.After(lastOK[e.Verb]) {
				lastOK[e.Verb] = e.At
			}
			continue
		}
		if prev, ok := lastErr[e.Verb]; !ok || e.At.After(prev.At) {
			lastErr[e.Verb] = e
		}
	}
	newest := entries[len(entries)-1]
	c := check{Name: "hook runs", Status: statusOK,
		Message: fmt.Sprintf("%d recent runs, last %s ago", len(entries),
			time.Since(newest.At).Round(time.Second))}

	verbs := make([]string, 0, len(lastOK))
	for v := range lastOK {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	for _, v := range verbs {
		c.Details = append(c.Details, fmt.Sprintf("%s last succeeded %s ago", v,
			time.Since(lastOK[v]).Round(time.Second)))
	}

	var failing []string
	for verb, e := range lastErr {
		// Only complain when the most recent run of that verb failed.
		if ok, seen := lastOK[verb]; !seen || e.At.After(ok) {
			failing = append(failing, fmt.Sprintf("%s is failing: %s", verb, firstLine(e.Error)))
		}
	}
	sort.Strings(failing)
	if len(failing) > 0 {
		c.Status = statusFail
		c.Details = append(c.Details, failing...)
		c.Fix = "check `anamnesia status` and `anamnesia logs`"
	}
	return c
}

// checkCanary writes a memory and reads it back. Filed under a reserved
// user so a diagnostic never lands in the user's real memory.
func checkCanary(ctx context.Context, hc *hostConfig) check {
	const canaryUser = "anamnesia-doctor"
	body := fmt.Sprintf("Doctor canary written at %s.", time.Now().UTC().Format(time.RFC3339))

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var created struct {
		ExperienceID string `json:"experience_id"`
	}
	if err := httpPost(ctx, hc, "/v1/experience", map[string]any{
		"user":    canaryUser,
		"project": "doctor",
		"title":   "doctor canary",
		"body":    body,
	}, &created); err != nil {
		return check{Name: "round trip", Status: statusFail,
			Message: "could not write a memory",
			Details: []string{firstLine(err.Error())},
			Fix:     "this is the write path your sessions use; see `anamnesia logs`"}
	}
	var got struct {
		Hits []struct {
			Domain string `json:"domain"`
		} `json:"hits"`
	}
	if err := httpPost(ctx, hc, "/v1/retrieve", map[string]any{
		"user":    canaryUser,
		"project": "doctor",
		"prompt":  "doctor canary",
	}, &got); err != nil {
		return check{Name: "round trip", Status: statusWarn,
			Message: "wrote a memory but could not search",
			Details: []string{firstLine(err.Error())}}
	}
	if len(got.Hits) == 0 {
		return check{Name: "round trip", Status: statusWarn,
			Message: "wrote a memory but search returned nothing",
			Fix:     "embeddings may still be backfilling; re-run in a minute"}
	}
	return check{Name: "round trip", Status: statusOK,
		Message: fmt.Sprintf("wrote and retrieved a memory (%d hits)", len(got.Hits))}
}

// ─── small helpers ───────────────────────────────────────────────────

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
