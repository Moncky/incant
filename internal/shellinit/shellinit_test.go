package shellinit

import (
	"os/exec"
	"strings"
	"testing"
)

func TestScriptServesZsh(t *testing.T) {
	s, err := Script("zsh")
	if err != nil {
		t.Fatalf("Script(zsh): %v", err)
	}
	for _, want := range []string{
		"incant-widget",     // the in-place rewrite widget
		"incant-fix-widget", // the repair widget
		"add-zsh-hook",      // the outcome hook that feeds --fix
		"print -z",          // the explicit-invocation path
		"BUFFER=$out",       // the buffer assignment, which is the whole point
	} {
		if !strings.Contains(s, want) {
			t.Errorf("zsh script missing %q", want)
		}
	}
	// The widget must never accept the line: that is what guarantees nothing
	// runs without a deliberate Enter.
	if strings.Contains(s, "zle accept-line") {
		t.Error("zsh script accepts the line; incant must never run its own suggestion")
	}
}

func TestScriptRejectsUnknownShell(t *testing.T) {
	if _, err := Script("tcsh"); err == nil {
		t.Error("expected an error for an unsupported shell")
	}
	// Path traversal through the shell name must not reach outside scripts/.
	for _, bad := range []string{"../go", "../../etc/passwd", "zsh/../zsh"} {
		if _, err := Script(bad); err == nil {
			t.Errorf("Script(%q) succeeded; want rejection", bad)
		}
	}
}

func TestSupportedListsZsh(t *testing.T) {
	got := Supported()
	if len(got) == 0 {
		t.Fatal("Supported() is empty")
	}
	var found bool
	for _, s := range got {
		if s == "zsh" {
			found = true
		}
	}
	if !found {
		t.Errorf("Supported() = %v, want it to include zsh", got)
	}
}

// The integration script cannot be unit tested, so at minimum it must parse.
// A syntax error here breaks every user's shell startup.
func TestEmbeddedScriptsAreSyntacticallyValid(t *testing.T) {
	checkers := map[string][]string{
		"zsh":  {"zsh", "-n"},
		"bash": {"bash", "-n"},
		"fish": {"fish", "--no-execute"},
	}
	for _, shell := range Supported() {
		argv, ok := checkers[shell]
		if !ok {
			t.Errorf("no syntax checker configured for %q", shell)
			continue
		}
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			t.Logf("%s not installed; skipping syntax check", shell)
			continue
		}
		src, err := Script(shell)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, append(argv[1:], "/dev/stdin")...)
		cmd.Stdin = strings.NewReader(src)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script has a syntax error: %v\n%s", shell, err, out)
		}
	}
}

func TestScriptServesBash(t *testing.T) {
	s, err := Script("bash")
	if err != nil {
		t.Fatalf("Script(bash): %v", err)
	}
	for _, want := range []string{
		"bind -x",               // the keybindings
		"READLINE_LINE=$1",      // the line assignment, which is the whole point
		"PROMPT_COMMAND",        // the outcome hook that feeds --fix
		"BASH_VERSINFO[0] >= 4", // bind -x needs bash 4
		"history -s",            // the explicit-invocation path
	} {
		if !strings.Contains(s, want) {
			t.Errorf("bash script missing %q", want)
		}
	}
	// Nothing may run without a deliberate Enter.
	for _, bad := range []string{"accept-line", "eval \"$_INCANT_OUT", "eval \"$out"} {
		if strings.Contains(s, bad) {
			t.Errorf("bash script contains %q; incant must never run its own suggestion", bad)
		}
	}
}

// Both integrations must keep stdout and stderr apart: merging them would
// paste the effect preview into the user's command line.
func TestScriptsNeverMergeStreams(t *testing.T) {
	for _, shell := range Supported() {
		s, _ := Script(shell)
		if strings.Contains(s, "_incant_call") && strings.Contains(s, "2>&1)") &&
			strings.Contains(s, "_incant_call \"$@\" 2>&1") {
			t.Errorf("%s script merges incant's stderr into stdout", shell)
		}
	}
}
