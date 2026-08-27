// Package claude talks to Claude through one of two backends:
//
//   - "cli": shells out to the Claude Code CLI, which authenticates with a
//     Claude.ai subscription (Pro/Max) via CLAUDE_CODE_OAUTH_TOKEN.
//   - "api": the Anthropic API SDK, billed per token via ANTHROPIC_API_KEY.
//
// Whichever credential is present wins; the subscription is preferred because
// it is the one most self-hosters already pay for.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Request is one summarisation call.
type Request struct {
	System string
	User   string
	Model  string
	Effort string
	Schema map[string]any
}

// Backend produces a JSON document matching Request.Schema.
type Backend interface {
	Name() string
	Complete(ctx context.Context, req Request) (string, error)
}

// New picks a backend from the environment. Explicit selection via
// NEWSDIGEST_BACKEND ("cli" or "api") overrides auto-detection.
func New() (Backend, error) {
	want := strings.ToLower(strings.TrimSpace(os.Getenv("NEWSDIGEST_BACKEND")))

	hasToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != ""
	hasKey := os.Getenv("ANTHROPIC_API_KEY") != ""

	switch want {
	case "cli":
		return newCLI()
	case "api":
		if !hasKey {
			return nil, fmt.Errorf("NEWSDIGEST_BACKEND=api but ANTHROPIC_API_KEY is not set")
		}
		return &apiBackend{client: anthropic.NewClient()}, nil
	case "":
		// fall through to auto-detection
	default:
		return nil, fmt.Errorf("NEWSDIGEST_BACKEND must be \"cli\" or \"api\", got %q", want)
	}

	if hasToken {
		return newCLI()
	}
	if hasKey {
		return &apiBackend{client: anthropic.NewClient()}, nil
	}
	return nil, fmt.Errorf("no Claude credentials found: set CLAUDE_CODE_OAUTH_TOKEN " +
		"(Claude subscription, run `claude setup-token` on your laptop) or ANTHROPIC_API_KEY (pay-per-token API)")
}

// --- API backend ---

type apiBackend struct {
	client anthropic.Client
}

func (a *apiBackend) Name() string { return "api" }

func (a *apiBackend) Complete(ctx context.Context, req Request) (string, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: 16000,
		System: []anthropic.TextBlockParam{{
			Text: req.System,
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.User)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(req.Effort),
			Format: anthropic.JSONOutputFormatParam{Schema: req.Schema},
		},
	}

	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return "", err
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", fmt.Errorf("claude declined the request (%s): %s",
			resp.StopDetails.Category, resp.StopDetails.Explanation)
	}

	var out strings.Builder
	for _, block := range resp.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(text.Text)
		}
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("claude returned no text (stop_reason=%s)", resp.StopReason)
	}
	return out.String(), nil
}

// --- CLI backend (Claude subscription) ---

type cliBackend struct {
	bin string
}

func newCLI() (Backend, error) {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found on PATH (set CLAUDE_BIN or use ANTHROPIC_API_KEY): %w", err)
	}
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return nil, fmt.Errorf("CLAUDE_CODE_OAUTH_TOKEN is not set; run `claude setup-token` on a machine " +
			"where you are logged in and put the result in .env")
	}
	return &cliBackend{bin: path}, nil
}

func (c *cliBackend) Name() string { return "cli" }

// cliResult is the envelope printed by `claude -p --output-format json`.
type cliResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

func (c *cliBackend) Complete(ctx context.Context, req Request) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	// The CLI has no structured-output flag, so the schema is described in the
	// prompt and the response is extracted below.
	prompt := req.System + "\n\n" + req.User + "\n\n" +
		"Reply with a single JSON object matching this JSON Schema and nothing else - " +
		"no prose, no markdown fence:\n" + mustJSON(req.Schema)

	cmd := exec.CommandContext(ctx, c.bin,
		"-p",
		"--output-format", "json",
		"--model", req.Model,
	)
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Inherit the environment so CLAUDE_CODE_OAUTH_TOKEN and HOME reach the CLI.
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude CLI failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var env cliResult
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		// Not the expected envelope - fall back to the raw output.
		return stdout.String(), nil
	}
	if env.IsError {
		return "", fmt.Errorf("claude CLI returned an error: %s", env.Result)
	}
	return env.Result, nil
}

func mustJSON(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// ExtractJSON pulls the first complete JSON object out of a model response,
// tolerating markdown fences and stray prose around it.
func ExtractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)

	// Strip a leading ```json / ``` fence if present.
	if strings.HasPrefix(s, "```") {
		if _, rest, ok := strings.Cut(s, "\n"); ok {
			s = rest
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}

	start := strings.Index(s, "{")
	if start < 0 {
		return "", fmt.Errorf("no JSON object in response")
	}

	// Walk the string tracking brace depth, ignoring braces inside strings.
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// nothing to do
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unterminated JSON object in response")
}
