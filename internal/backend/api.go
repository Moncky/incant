package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/callumscott/incant/internal/probe"
	"github.com/callumscott/incant/internal/prompt"
)

// API is the primary backend: a direct Messages API call with incant's own
// probe tools, so the model can look at the user's files -- through the
// sandbox, and only through it -- before answering.
//
// The loop is written out rather than using the SDK's tool runner because
// incant needs two things the runner does not give it: a hard cap on turns,
// with a last turn that must answer rather than probe again, and a probe
// budget shared across parallel calls.
type API struct {
	client  anthropic.Client
	model   string
	effort  string
	sandbox *probe.Sandbox
	logf    func(format string, args ...any)
}

// APIOptions configures the API backend.
type APIOptions struct {
	// APIKey overrides the SDK's own credential lookup (environment, then
	// profile). Empty leaves that lookup in charge.
	APIKey string
	// BaseURL points at a different endpoint; tests use it.
	BaseURL string
	Model   string
	Effort  string
	// Sandbox is where probes run. Nil sends no tools: the model answers
	// from the session block alone, as for context = minimal.
	Sandbox *probe.Sandbox
	// Logf receives diagnostics in verbose mode.
	Logf func(format string, args ...any)
}

// maxTurns bounds the probe loop. The last turn is forced to answer.
const maxTurns = 5

// NewAPI builds the API backend.
func NewAPI(o APIOptions) *API {
	opts := []option.RequestOption{
		// One retry: a keybind that stalls through three attempts is worse
		// than an error the user can retry by pressing it again.
		option.WithMaxRetries(1),
	}
	if o.APIKey != "" {
		opts = append(opts, option.WithAPIKey(o.APIKey))
	}
	if o.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(o.BaseURL))
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &API{
		client:  anthropic.NewClient(opts...),
		model:   o.Model,
		effort:  o.Effort,
		sandbox: o.Sandbox,
		logf:    logf,
	}
}

// Name identifies the backend.
func (a *API) Name() string { return "api (" + a.model + ")" }

// Credentials reports where API credentials would come from, or "" if
// nowhere. It mirrors the SDK's lookup order closely enough to choose a
// backend; the SDK remains the authority when the request is made.
func Credentials(configKey string) string {
	switch {
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		return "ANTHROPIC_API_KEY"
	case os.Getenv("ANTHROPIC_AUTH_TOKEN") != "":
		return "ANTHROPIC_AUTH_TOKEN"
	case configKey != "":
		return "config api_key"
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".config")
		}
	}
	if dir != "" {
		if entries, err := os.ReadDir(filepath.Join(dir, "anthropic")); err == nil && len(entries) > 0 {
			return "ant auth profile"
		}
	}
	return ""
}

// IsAuthError reports whether err is the API rejecting the credentials, which
// is the one API failure where falling back to the claude CLI makes sense.
func IsAuthError(err error) bool {
	var apierr *anthropic.Error
	return errors.As(err, &apierr) && (apierr.StatusCode == 401 || apierr.StatusCode == 403)
}

// supportsEffort: Haiku 4.5 and older reject output_config.effort.
func supportsEffort(model string) bool {
	return !strings.Contains(model, "haiku") && !strings.Contains(model, "-4-5") && !strings.Contains(model, "-3-")
}

// supportsDefaultFallback lists models that accept server-side refusal
// fallbacks in the "default" form, which routes a declined request to a
// suitable model inside the same call.
func supportsDefaultFallback(model string) bool {
	switch model {
	case "claude-opus-5-5", "claude-opus-5", "claude-fable-5-1", "claude-sonnet-5-5":
		return true
	}
	return false
}

// Suggest runs the probe loop and returns candidates.
func (a *API) Suggest(ctx context.Context, req Request) ([]Candidate, error) {
	n := max(req.N, 1)

	system := prompt.System
	if a.sandbox != nil {
		system += prompt.ToolGuidance
	}
	user := prompt.User(req.Context, req.Query)
	if req.Fix {
		// In the user turn, not the system prompt, so the cached system
		// prefix is shared by fix and ordinary requests.
		user += strings.TrimRight(prompt.FixSuffix, "\n")
	}
	if n > 1 {
		user += prompt.Alternatives(n)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 16000,
		System: []anthropic.TextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	}
	if a.effort != "" && supportsEffort(a.model) {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(a.effort)}
	}
	if a.sandbox != nil {
		params.Tools = probeTools()
	}
	var reqOpts []option.RequestOption
	if supportsDefaultFallback(a.model) {
		reqOpts = append(reqOpts,
			option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
			option.WithJSONSet("fallbacks", "default"))
	}

	probes := 0
	for turn := 1; turn <= maxTurns; turn++ {
		if turn == maxTurns && a.sandbox != nil {
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
		start := time.Now()
		resp, err := a.client.Messages.New(ctx, params, reqOpts...)
		if err != nil {
			return nil, err
		}
		a.logf("turn %d: %v, stop=%s, in=%d (cached %d) out=%d",
			turn, time.Since(start).Round(time.Millisecond), resp.StopReason,
			resp.Usage.InputTokens, resp.Usage.CacheReadInputTokens, resp.Usage.OutputTokens)

		switch resp.StopReason {
		case anthropic.StopReasonRefusal:
			return nil, fmt.Errorf("%w: the model declined this request", ErrUnsupported)
		case anthropic.StopReasonMaxTokens:
			return nil, errors.New("the reply was cut off before a command was produced")
		case anthropic.StopReasonToolUse:
			params.Messages = append(params.Messages, resp.ToParam())
			results, calls := a.runTools(ctx, resp.Content)
			probes += calls
			params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))
			continue
		}

		var text strings.Builder
		for _, block := range resp.Content {
			if t, ok := block.AsAny().(anthropic.TextBlock); ok {
				text.WriteString(t.Text)
				text.WriteString("\n")
			}
		}
		return candidates(text.String(), n, probes)
	}
	return nil, errors.New("no answer within the probe turn limit")
}

func candidates(reply string, n, probes int) ([]Candidate, error) {
	cmds, err := prompt.ParseCandidates(reply, n)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unsupported:") {
			return nil, fmt.Errorf("%w: %s", ErrUnsupported, strings.TrimPrefix(err.Error(), "unsupported: "))
		}
		return nil, err
	}
	out := make([]Candidate, len(cmds))
	for i, c := range cmds {
		out[i] = Candidate{Command: c, Probes: probes}
	}
	return out, nil
}

// runTools executes every tool call in a turn concurrently and returns all
// results for a single user message, as the API expects.
func (a *API) runTools(ctx context.Context, content []anthropic.ContentBlockUnion) ([]anthropic.ContentBlockParamUnion, int) {
	type call struct {
		id, name string
		input    json.RawMessage
	}
	var calls []call
	for _, block := range content {
		if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			calls = append(calls, call{tu.ID, tu.Name, json.RawMessage(tu.JSON.Input.Raw())})
		}
	}

	results := make([]anthropic.ContentBlockParamUnion, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := a.runTool(ctx, c.name, c.input)
			if err != nil {
				a.logf("  %s %s -> error: %v", c.name, c.input, err)
				results[i] = anthropic.NewToolResultBlock(c.id, err.Error(), true)
				return
			}
			a.logf("  %s %s -> %d bytes", c.name, c.input, len(out))
			results[i] = anthropic.NewToolResultBlock(c.id, out, false)
		}()
	}
	wg.Wait()
	return results, len(calls)
}

// runTool dispatches one call to the sandbox. Inputs are decoded strictly; a
// malformed call is reported back to the model as an error, not guessed at.
func (a *API) runTool(ctx context.Context, name string, input json.RawMessage) (string, error) {
	decode := func(v any) error {
		dec := json.NewDecoder(strings.NewReader(string(input)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return fmt.Errorf("invalid input for %s: %w", name, err)
		}
		return nil
	}
	sb := a.sandbox
	switch name {
	case "list_dir":
		var in struct {
			Path  string `json:"path"`
			Depth int    `json:"depth"`
		}
		if err := decode(&in); err != nil {
			return "", err
		}
		return sb.ListDir(ctx, in.Path, in.Depth)
	case "peek_file":
		var in struct {
			Path string `json:"path"`
			Head int    `json:"head"`
			Tail int    `json:"tail"`
		}
		if err := decode(&in); err != nil {
			return "", err
		}
		return sb.PeekFile(ctx, in.Path, in.Head, in.Tail)
	case "file_info":
		var in struct {
			Path string `json:"path"`
		}
		if err := decode(&in); err != nil {
			return "", err
		}
		return sb.FileInfo(ctx, in.Path)
	case "which_tool":
		var in struct {
			Name string `json:"name"`
		}
		if err := decode(&in); err != nil {
			return "", err
		}
		return sb.WhichTool(ctx, in.Name)
	case "grep_sample":
		var in struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := decode(&in); err != nil {
			return "", err
		}
		return sb.GrepSample(ctx, in.Pattern, in.Path)
	case "git_state":
		return sb.GitState(ctx)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// probeTools describes the sandbox's model-facing probes. The list is fixed
// and ordered so the cached prefix (tools, then system) never changes.
func probeTools() []anthropic.ToolUnionParam {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	tool := func(name, desc string, props map[string]any, required ...string) anthropic.ToolUnionParam {
		if props == nil {
			props = map[string]any{}
		}
		return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        name,
			Description: anthropic.String(desc),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties:  props,
				Required:    required,
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}}
	}
	return []anthropic.ToolUnionParam{
		tool("list_dir", "List entries under a directory in the working directory, with sizes and an extension histogram. Depth 1 or 2.",
			map[string]any{"path": str("Directory, relative to the working directory. Default '.'."), "depth": integer("1 or 2. Default 1.")}),
		tool("peek_file", "Show the first and/or last lines of a text file, to learn its real format: delimiter, header, field order. Refuses binaries and credential files.",
			map[string]any{"path": str("File, relative to the working directory."), "head": integer("Lines from the start, up to 40. Default 10."), "tail": integer("Lines from the end, up to 40.")}, "path"),
		tool("file_info", "Report a path's type, size, mode, modification time and line count, without its contents.",
			map[string]any{"path": str("Path, relative to the working directory.")}, "path"),
		tool("which_tool", "Report whether a command is installed and, for common tools, its version -- e.g. whether sed is GNU or BSD.",
			map[string]any{"name": str("A bare command name such as jq or gsed.")}, "name"),
		tool("grep_sample", "Search files for a regular expression (RE2) and show up to 5 matching lines, to learn which files hold what.",
			map[string]any{"pattern": str("RE2 regular expression."), "path": str("File or directory to search. Default '.'.")}, "pattern"),
		tool("git_state", "Report the git branch and changed files in the working directory.", nil),
	}
}
