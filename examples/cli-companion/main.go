// cli-companion is the reference dock-on agent for Anamnesia. It proves
// that an external agent can become a personal AI by consuming only
// Anamnesia's public MCP surface — it imports nothing from
// github.com/flohs/anamnesia-open-source/internal/*.
//
// Boot:   anamnesia_identity (persona) + anamnesia_capabilities (tools).
// Turn:   anamnesia_search (context) -> LLM -> anamnesia_commitments_record
//
//	if the reply contains a promise.
//
// Exit:   anamnesia_ingest (whole transcript) so the kernel can extract.
//
// The LLM step uses the Anthropic Messages API over raw HTTP when
// ANTHROPIC_API_KEY is set; otherwise it echoes a canned reply so the
// dock-on contract can be exercised offline.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

var (
	mcpURL    = flag.String("mcp", "http://localhost:8181/mcp", "Anamnesia MCP endpoint")
	user      = flag.String("user", "default", "user handle")
	project   = flag.String("project", "", "project slug (optional)")
	modelName = flag.String("model", "claude-sonnet-4-6", "Anthropic model")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mc, err := client.NewStreamableHttpClient(*mcpURL)
	if err != nil {
		return fmt.Errorf("mcp client: %w", err)
	}
	if err := mc.Start(ctx); err != nil {
		return fmt.Errorf("mcp start: %w", err)
	}
	if _, err := mc.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "cli-companion", Version: "0.1.0"},
		},
	}); err != nil {
		return fmt.Errorf("mcp init: %w", err)
	}

	persona := callIdentity(ctx, mc)
	caps := callCapabilities(ctx, mc)
	log.Printf("dock-on agent ready: %d capabilities available", caps)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	var transcript strings.Builder

	fmt.Println("# cli-companion — type a prompt; ^D to exit and ingest")
	for {
		fmt.Print("you> ")
		if !scanner.Scan() {
			break
		}
		prompt := scanner.Text()
		if strings.TrimSpace(prompt) == "" {
			continue
		}

		hits := callSearch(ctx, mc, prompt)

		var sysParts []string
		if persona != "" {
			sysParts = append(sysParts, persona)
		}
		if hits != "" {
			sysParts = append(sysParts, "Anamnesia context:\n"+hits)
		}
		reply := askLLM(ctx, strings.Join(sysParts, "\n\n"), prompt)
		fmt.Printf("ai> %s\n", reply)

		if hasCommitmentLanguage(reply) {
			callRecordCommitment(ctx, mc, firstSentence(reply))
			log.Println("recorded a commitment")
		}

		transcript.WriteString("user: " + prompt + "\nai: " + reply + "\n\n")
	}

	if transcript.Len() > 0 {
		callIngest(ctx, mc, transcript.String())
		log.Println("transcript ingested")
	}
	return nil
}

// ─── MCP call wrappers ───────────────────────────────────────────────

func callTool(ctx context.Context, mc *client.Client, name string, args map[string]any) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := mc.CallTool(rctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("tool %s returned an error", name)
	}
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			return tc.Text, nil
		}
	}
	return "", nil
}

// baseArgs builds the {user, project?} map every tool call shares.
func baseArgs() map[string]any {
	m := map[string]any{"user": *user}
	if *project != "" {
		m["project"] = *project
	}
	return m
}

func callIdentity(ctx context.Context, mc *client.Client) string {
	raw, err := callTool(ctx, mc, "anamnesia_identity", baseArgs())
	if err != nil {
		log.Printf("identity: %v", err)
		return ""
	}
	var id struct {
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return ""
	}
	return id.SystemPrompt
}

func callCapabilities(ctx context.Context, mc *client.Client) int {
	raw, err := callTool(ctx, mc, "anamnesia_capabilities", baseArgs())
	if err != nil {
		return 0
	}
	var arr []any
	_ = json.Unmarshal([]byte(raw), &arr)
	return len(arr)
}

func callSearch(ctx context.Context, mc *client.Client, text string) string {
	args := baseArgs()
	args["text"] = text
	args["k"] = 5
	raw, _ := callTool(ctx, mc, "anamnesia_search", args)
	return raw
}

func callRecordCommitment(ctx context.Context, mc *client.Client, body string) {
	args := baseArgs()
	args["body"] = body
	_, _ = callTool(ctx, mc, "anamnesia_commitments_record", args)
}

func callIngest(ctx context.Context, mc *client.Client, content string) {
	args := baseArgs()
	args["kind"] = "cli-companion-session"
	args["content"] = content
	_, _ = callTool(ctx, mc, "anamnesia_ingest", args)
}

// ─── LLM (raw Anthropic Messages API; stub when no key) ──────────────

func askLLM(ctx context.Context, system, prompt string) string {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "(stub reply — set ANTHROPIC_API_KEY for a real model) you said: " + prompt
	}
	body, _ := json.Marshal(map[string]any{
		"model":      *modelName,
		"max_tokens": 1024,
		"system":     system,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	})
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "(llm error: " + err.Error() + ")"
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "(llm parse error)"
	}
	var sb strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// ─── heuristics ─────────────────────────────────────────────────────

var commitmentMarkers = []string{
	"I'll ", "I will ", "I'll send", "I'll follow up", "Let me ", "I promise",
}

func hasCommitmentLanguage(s string) bool {
	for _, m := range commitmentMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func firstSentence(s string) string {
	for _, sep := range []string{". ", ".\n"} {
		if i := strings.Index(s, sep); i > 0 {
			return s[:i+1]
		}
	}
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
