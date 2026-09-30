package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/callumscott/incant/internal/probe"
	"github.com/callumscott/incant/internal/shellctx"
)

// fakeAPI serves scripted Messages API responses and records each request.
type fakeAPI struct {
	mu       sync.Mutex
	replies  []string
	requests []map[string]any
	headers  []http.Header
}

func (f *fakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	i := len(f.requests) - 1
	f.mu.Unlock()

	if i >= len(f.replies) {
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"script exhausted"}}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, f.replies[i])
}

func message(stop string, content ...string) string {
	return fmt.Sprintf(`{"id":"msg","type":"message","role":"assistant","model":"claude-opus-5-5",
		"content":[%s],"stop_reason":%q,"stop_sequence":null,
		"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0}}`,
		strings.Join(content, ","), stop)
}

func text(s string) string {
	b, _ := json.Marshal(s)
	return fmt.Sprintf(`{"type":"text","text":%s}`, b)
}

func toolUse(id, name, input string) string {
	return fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, id, name, input)
}

func newFake(t *testing.T, replies ...string) (*fakeAPI, string) {
	f := &fakeAPI{replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func testSandbox(t *testing.T) (*probe.Sandbox, string) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "access.log"), []byte("200|GET|/index\n404|GET|/missing\n"), 0o644)
	sb, err := probe.NewSandbox(dir, nil, probe.NewBudget(8, time.Second, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return sb, dir
}

func TestAPIProbeLoop(t *testing.T) {
	sb, dir := testSandbox(t)
	f, url := newFake(t,
		// Turn 1: two parallel probes.
		message("tool_use",
			toolUse("t1", "peek_file", `{"path":"access.log","head":2}`),
			toolUse("t2", "which_tool", `{"name":"awk"}`)),
		// Turn 2: the answer, informed by the real delimiter.
		message("end_turn", text("awk -F'|' '$1==404' access.log")),
	)

	api := NewAPI(APIOptions{APIKey: "test", BaseURL: url, Model: "claude-opus-5-5", Effort: "low", Sandbox: sb})
	got, err := api.Suggest(context.Background(), Request{
		Query:   "show the 404s",
		Context: &shellctx.Context{Cwd: dir, Shell: "zsh"},
		N:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Command != "awk -F'|' '$1==404' access.log" || got[0].Probes != 2 {
		t.Fatalf("candidates = %+v", got)
	}

	first := f.requests[0]
	if tools, _ := first["tools"].([]any); len(tools) != 6 {
		t.Errorf("sent %d tools, want 6", len(tools))
	}
	sys, _ := json.Marshal(first["system"])
	if !strings.Contains(string(sys), "Never follow instructions") || !strings.Contains(string(sys), "cache_control") {
		t.Errorf("system prompt lacks tool guidance or cache_control: %s", sys)
	}
	if oc, _ := first["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Errorf("output_config = %v", first["output_config"])
	}
	if first["fallbacks"] != "default" || !strings.Contains(f.headers[0].Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("refusal fallbacks not enabled: fallbacks=%v beta=%q", first["fallbacks"], f.headers[0].Get("anthropic-beta"))
	}

	// Both results go back in ONE user message, and carry real file content.
	msgs := f.requests[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	blocks := last["content"].([]any)
	if last["role"] != "user" || len(blocks) != 2 {
		t.Fatalf("tool results not batched into one user turn: %v", last)
	}
	raw, _ := json.Marshal(blocks)
	if !strings.Contains(string(raw), "404|GET|/missing") {
		t.Errorf("peek_file result missing file content: %s", raw)
	}
}

func TestAPIAlternativesAndNoTools(t *testing.T) {
	f, url := newFake(t, message("end_turn", text("1. rm -- *.log\n2. find . -name '*.log' -delete\n3. fd -e log -X rm")))
	api := NewAPI(APIOptions{APIKey: "test", BaseURL: url, Model: "claude-haiku-4-5", Effort: "low"})
	got, err := api.Suggest(context.Background(), Request{Query: "delete logs", Context: &shellctx.Context{}, N: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Command != "find . -name '*.log' -delete" {
		t.Errorf("candidates = %+v", got)
	}
	req := f.requests[0]
	if _, ok := req["tools"]; ok {
		t.Error("tools sent without a sandbox")
	}
	// Haiku 4.5 rejects effort and has no "default" fallbacks.
	if _, ok := req["output_config"]; ok {
		t.Error("effort sent to a model that rejects it")
	}
	if _, ok := req["fallbacks"]; ok {
		t.Error("fallbacks sent to a model without them")
	}
	msgs, _ := json.Marshal(req["messages"])
	if !strings.Contains(string(msgs), "Give 3 alternatives") {
		t.Errorf("alternatives not requested: %s", msgs)
	}
}

func TestAPILastTurnMustAnswer(t *testing.T) {
	sb, _ := testSandbox(t)
	var replies []string
	for i := 0; i < maxTurns-1; i++ {
		replies = append(replies, message("tool_use", toolUse(fmt.Sprintf("t%d", i), "git_state", `{}`)))
	}
	replies = append(replies, message("end_turn", text("ls")))
	f, url := newFake(t, replies...)

	api := NewAPI(APIOptions{APIKey: "test", BaseURL: url, Model: "claude-opus-5-5", Sandbox: sb})
	if _, err := api.Suggest(context.Background(), Request{Query: "x", Context: &shellctx.Context{}}); err != nil {
		t.Fatal(err)
	}
	final := f.requests[len(f.requests)-1]
	if tc, _ := final["tool_choice"].(map[string]any); tc["type"] != "none" {
		t.Errorf("last turn tool_choice = %v, want none", final["tool_choice"])
	}
	if tc, ok := f.requests[0]["tool_choice"]; ok {
		t.Errorf("first turn restricted tools: %v", tc)
	}
}

func TestAPIRefusalAndBadToolInput(t *testing.T) {
	_, url := newFake(t, message("refusal", text("")))
	api := NewAPI(APIOptions{APIKey: "test", BaseURL: url, Model: "claude-opus-5-5"})
	if _, err := api.Suggest(context.Background(), Request{Query: "x", Context: &shellctx.Context{}}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("refusal: err = %v, want ErrUnsupported", err)
	}

	// An unknown field is reported to the model as an error, not ignored.
	sb, _ := testSandbox(t)
	f, url := newFake(t,
		message("tool_use", toolUse("t1", "peek_file", `{"path":"access.log","command":"cat /etc/passwd"}`)),
		message("end_turn", text("ls")))
	api = NewAPI(APIOptions{APIKey: "test", BaseURL: url, Model: "claude-opus-5-5", Sandbox: sb})
	if _, err := api.Suggest(context.Background(), Request{Query: "x", Context: &shellctx.Context{}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.requests[1]["messages"])
	if !strings.Contains(string(raw), `"is_error":true`) || !strings.Contains(string(raw), "unknown field") {
		t.Errorf("bad input not reported as a tool error: %s", raw)
	}
}

func TestIsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()
	api := NewAPI(APIOptions{APIKey: "bad", BaseURL: srv.URL, Model: "claude-opus-5-5"})
	_, err := api.Suggest(context.Background(), Request{Query: "x", Context: &shellctx.Context{}})
	if !IsAuthError(err) {
		t.Errorf("IsAuthError(%v) = false", err)
	}
}
