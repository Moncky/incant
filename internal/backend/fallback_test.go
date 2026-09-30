package backend

import (
	"strings"
	"testing"
)

func TestExtractReplyParsesEnvelope(t *testing.T) {
	got, err := extractReply(`{"type":"result","subtype":"success","is_error":false,"result":"wc -l *.txt"}`)
	if err != nil {
		t.Fatalf("extractReply: %v", err)
	}
	if got != "wc -l *.txt" {
		t.Errorf("got %q, want the result field", got)
	}
}

func TestExtractReplySurfacesCLIErrors(t *testing.T) {
	// The "Not logged in" case is the one users will actually hit, so its
	// message must survive to the surface rather than becoming a parse error.
	_, err := extractReply(`{"is_error":true,"result":"Not logged in · Please run /login"}`)
	if err == nil {
		t.Fatal("expected an error for is_error=true")
	}
	if !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("err = %v, want the CLI's own message preserved", err)
	}
}

func TestExtractReplyRejectsEmptyAndUnknownJSON(t *testing.T) {
	for name, in := range map[string]string{
		"empty":        "",
		"whitespace":   "   \n ",
		"empty result": `{"is_error":false,"result":"  "}`,
		"bad json":     `{"result": `,
	} {
		if got, err := extractReply(in); err == nil {
			t.Errorf("%s: extractReply(%q) = %q, want an error", name, in, got)
		}
	}
}

// A non-JSON reply is tolerated in case the output format changes, so incant
// degrades rather than breaking outright.
func TestExtractReplyAcceptsPlainText(t *testing.T) {
	got, err := extractReply("wc -l *.txt\n")
	if err != nil {
		t.Fatalf("extractReply: %v", err)
	}
	if got != "wc -l *.txt" {
		t.Errorf("got %q", got)
	}
}

// The fallback's whole safety story is that claude gets no tools. If this list
// is emptied or the flag stops being passed, claude -p will answer the
// question by running commands in the user's cwd instead of suggesting one.
func TestDisallowedToolsCoversActingTools(t *testing.T) {
	have := map[string]bool{}
	for _, t := range disallowedTools {
		have[t] = true
	}
	for _, want := range []string{"Bash", "Write", "Edit", "Read", "Grep", "Glob", "WebFetch", "Task"} {
		if !have[want] {
			t.Errorf("disallowedTools is missing %q; the fallback could act on the user's filesystem", want)
		}
	}
}

func TestFirstLineTruncates(t *testing.T) {
	if got := firstLine("one\ntwo\nthree"); got != "one" {
		t.Errorf("got %q, want %q", got, "one")
	}
	long := strings.Repeat("x", 500)
	got := firstLine(long)
	if len([]rune(got)) > 210 {
		t.Errorf("firstLine did not truncate: %d runes", len([]rune(got)))
	}
}
