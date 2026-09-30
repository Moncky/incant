package prompt

import (
	"strings"
	"testing"

	"github.com/callumscott/incant/internal/shellctx"
)

func TestParseCommandAcceptsBareCommand(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"wc -l *.txt", "wc -l *.txt"},
		{"  cat -- *.txt | wc -l  ", "cat -- *.txt | wc -l"},
		{"wc -l *.txt\n", "wc -l *.txt"},
		{"sed -i '' 's/a/b/g' f.txt", "sed -i '' 's/a/b/g' f.txt"},
	} {
		got, err := ParseCommand(tc.in)
		if err != nil {
			t.Errorf("ParseCommand(%q) = error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The contract asks for a bare command, but a model may still wrap it. These
// wrappers would be pasted into the user's prompt verbatim, so stripping them
// is not cosmetic.
func TestParseCommandStripsWrappers(t *testing.T) {
	for name, in := range map[string]string{
		"bash fence":  "```bash\nwc -l *.txt\n```",
		"plain fence": "```\nwc -l *.txt\n```",
		"sh fence":    "```sh\nwc -l *.txt\n```",
		"dollar":      "$ wc -l *.txt",
		"percent":     "% wc -l *.txt",
		"comment":     "# count the lines\nwc -l *.txt",
		"blank lines": "\n\nwc -l *.txt\n\n",
	} {
		got, err := ParseCommand(in)
		if err != nil {
			t.Errorf("%s: ParseCommand(%q) = error %v", name, in, err)
			continue
		}
		if got != "wc -l *.txt" {
			t.Errorf("%s: got %q, want %q", name, got, "wc -l *.txt")
		}
	}
}

func TestParseCommandRejectsControlCharacters(t *testing.T) {
	// A control character in the buffer corrupts the user's line editor
	// rather than merely looking wrong, so these must be refused outright.
	for name, in := range map[string]string{
		"escape sequence": "wc -l\x1b[2K; rm -rf /",
		"carriage return": "echo hi\rrm -rf /",
		"null byte":       "echo\x00hi",
	} {
		if got, err := ParseCommand(in); err == nil {
			t.Errorf("%s: ParseCommand accepted %q -> %q, want an error", name, in, got)
		}
	}
}

func TestParseCommandHandlesUnsupported(t *testing.T) {
	_, err := ParseCommand("UNSUPPORTED: this needs a GUI")
	if err == nil {
		t.Fatal("expected an error for an UNSUPPORTED reply")
	}
	if !strings.Contains(err.Error(), "unsupported:") {
		t.Errorf("err = %v, want it to carry the unsupported marker", err)
	}
	if !strings.Contains(err.Error(), "needs a GUI") {
		t.Errorf("err = %v, want the model's reason preserved", err)
	}

	// A bare marker with no reason must still be reported, not crash.
	if _, err := ParseCommand("UNSUPPORTED:"); err == nil {
		t.Error("expected an error for a bare UNSUPPORTED marker")
	}
}

func TestParseCommandRejectsEmptyAndProse(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n", "```\n```", "# only a comment"} {
		if got, err := ParseCommand(in); err == nil {
			t.Errorf("ParseCommand(%q) = %q, want an error", in, got)
		}
	}
}

// Only the first command line is taken: a model that explains itself after the
// command must not have its prose pasted into the buffer.
func TestParseCommandTakesFirstLineOnly(t *testing.T) {
	got, err := ParseCommand("wc -l *.txt\nThis counts the lines in every text file.")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wc -l *.txt" {
		t.Errorf("got %q, want just the command", got)
	}
}

func TestUserPutsRequestAfterContext(t *testing.T) {
	ctx := &shellctx.Context{
		Cwd: "/tmp/proj", Shell: "zsh", Userland: shellctx.UserlandBSD,
	}
	got := User(ctx, "count the lines")

	ctxIdx := strings.Index(got, "<session>")
	reqIdx := strings.Index(got, "Request: count the lines")
	if ctxIdx < 0 || reqIdx < 0 {
		t.Fatalf("missing context or request:\n%s", got)
	}
	// The volatile request must sit after the stable context so the context
	// block can share a cache prefix across invocations.
	if ctxIdx > reqIdx {
		t.Errorf("request appears before context; this defeats prefix caching:\n%s", got)
	}
}

func TestUserToleratesNilContext(t *testing.T) {
	got := User(nil, "count the lines")
	if !strings.Contains(got, "Request: count the lines") {
		t.Errorf("got %q, want the request", got)
	}
	if strings.Contains(got, "<session>") {
		t.Errorf("got %q, want no empty session block", got)
	}
}

// The system prompt is cached on every request, so an accidental edit is a
// cost regression for every user. This pins the properties that matter.
func TestSystemPromptContract(t *testing.T) {
	for _, want := range []string{
		"ONE command line",
		"UNSUPPORTED:",
		"userland",
		"NOT installed",
	} {
		if !strings.Contains(System, want) {
			t.Errorf("system prompt no longer mentions %q", want)
		}
	}
	if strings.Contains(FixSuffix, "\x00") || FixSuffix == "" {
		t.Error("fix suffix should be non-empty text")
	}
	// The repair instruction must be a suffix, never folded into System, or
	// every non-repair request pays a cache invalidation.
	if strings.Contains(System, "repair request") {
		t.Error("repair instructions belong in FixSuffix, not System (cache stability)")
	}
}

func TestParseCandidates(t *testing.T) {
	reply := "1. find . -name '*.log' -delete\n2. rm -- *.log\n- `fd -e log -x rm`\n2. rm -- *.log\n"
	got, err := ParseCandidates(reply, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"find . -name '*.log' -delete", "rm -- *.log", "fd -e log -x rm"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, _ := ParseCandidates(reply, 1); len(got) != 1 {
		t.Errorf("n=1 returned %d", len(got))
	}
	if _, err := ParseCandidates("UNSUPPORTED: needs a GUI", 3); err == nil || !strings.HasPrefix(err.Error(), "unsupported:") {
		t.Errorf("unsupported: err = %v", err)
	}
}
