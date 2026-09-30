package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/prompt"
	"github.com/callumscott/incant/internal/spawn"
)

// Fallback drives the user's own claude CLI in print mode.
//
// This path needs no API key: it authenticates as whatever the user's CLI is
// already logged in as, so incant works the moment it is installed. The cost
// is latency -- a full agent harness boots per invocation, which is seconds,
// not the sub-second budget the keybind wants -- and no reconnaissance, since
// the probe tools live on incant's side of the wire. It is the on-ramp, not
// the destination.
type Fallback struct {
	// Timeout bounds one invocation. The CLI's startup dominates it.
	Timeout time.Duration
}

// NewFallback returns a fallback backend with a sensible timeout.
func NewFallback() *Fallback {
	return &Fallback{Timeout: 60 * time.Second}
}

// Name identifies the backend.
func (f *Fallback) Name() string { return "claude-cli" }

// Available reports whether the claude CLI is on PATH.
func (f *Fallback) Available() bool {
	_, err := spawn.Lookup("claude")
	return err == nil
}

// disallowedTools is every acting and reading tool the harness ships. The
// fallback is text in, text out.
var disallowedTools = []string{
	"Bash", "BashOutput", "KillShell",
	"Read", "Write", "Edit", "MultiEdit", "NotebookEdit",
	"Glob", "Grep", "LS",
	"WebFetch", "WebSearch",
	"Task", "TodoWrite", "SlashCommand", "Skill",
	"Artifact", "ArtifactComments", "ArtifactData",
}

// cliEnvelope is the shape of `claude -p --output-format json`.
type cliEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// Suggest asks the CLI for one command.
//
// N is ignored: generating distinct alternates needs either multiple round
// trips through a multi-second harness or a structured reply the CLI does not
// guarantee. Candidate cycling is an API-backend feature, and the shell
// integration degrades to a single suggestion here.
func (f *Fallback) Suggest(ctx context.Context, req Request) ([]Candidate, error) {
	if !f.Available() {
		return nil, fmt.Errorf("%w: claude not found on PATH", ErrUnavailable)
	}

	system := prompt.System
	if req.Fix {
		system += prompt.FixSuffix
	}

	// The request goes over stdin, never in argv: it is user text of arbitrary
	// length and shape, and argv is the one place it could be re-read as
	// something other than data.
	args := []string{
		"-p",
		"--output-format", "json",
		"--append-system-prompt", system,
		// Latency is the fallback's weak point and this path never needs
		// deep reasoning, so ask for the cheapest useful setting.
		"--effort", "low",
	}
	// Strip the harness of every tool. Left alone, claude -p will happily run
	// commands in the user's cwd and answer the question itself -- which would
	// drive a hole straight through incant's promise never to execute what it
	// suggests, and would do it via a harness whose allowlist incant does not
	// control.
	//
	// Read-only tools are refused too, tempting as they look: recon through
	// claude's own Read/Grep would bypass the sandbox in internal/probe
	// entirely -- no cwd containment, no secret-file gating, no budget. Owning
	// that tool surface is exactly why the API backend is the primary path.
	args = append(args, "--disallowed-tools", strings.Join(disallowedTools, " "))
	// Belt and braces: anything that would prompt for permission -- including
	// a tool added to the harness after this was written -- is denied rather
	// than left to hang in front of the user's prompt.
	args = append(args, "--permission-prompts", "none")

	timeout := f.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	res, err := spawn.Run(ctx, "claude", args, prompt.User(req.Context, req.Query), timeout)
	if err != nil {
		return nil, fmt.Errorf("claude CLI: %w", err)
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		if detail == "" {
			detail = "no output"
		}
		return nil, fmt.Errorf("claude CLI exited %d: %s", res.ExitCode, firstLine(detail))
	}

	reply, err := extractReply(res.Stdout)
	if err != nil {
		return nil, err
	}

	cmd, err := prompt.ParseCommand(reply)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unsupported:") {
			return nil, fmt.Errorf("%w: %s", ErrUnsupported, strings.TrimPrefix(err.Error(), "unsupported: "))
		}
		return nil, err
	}
	return []Candidate{{Command: cmd, Probes: 0}}, nil
}

// extractReply pulls the assistant text out of the CLI's JSON envelope,
// tolerating a plain-text reply in case the output format changes.
func extractReply(stdout string) (string, error) {
	s := strings.TrimSpace(stdout)
	if s == "" {
		return "", fmt.Errorf("claude CLI produced no output")
	}

	if strings.HasPrefix(s, "{") {
		var env cliEnvelope
		if err := json.Unmarshal([]byte(s), &env); err == nil {
			if env.IsError {
				return "", fmt.Errorf("claude CLI reported an error: %s", firstLine(env.Result))
			}
			if strings.TrimSpace(env.Result) != "" {
				return env.Result, nil
			}
			return "", fmt.Errorf("claude CLI returned an empty result")
		}
		// Fall through: a JSON-looking reply that does not match the envelope
		// is more likely a changed format than a command, so do not guess.
		return "", fmt.Errorf("could not parse claude CLI output as JSON")
	}

	return s, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
