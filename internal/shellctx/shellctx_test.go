package shellctx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"file_a.txt": "one\ntwo\nthree\n",
		"file_b.txt": "four\nfive\n",
		"song.mp3":   "ID3",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCollectGathersSessionFacts(t *testing.T) {
	dir := fixture(t)
	c, err := Collect(context.Background(), Options{Cwd: dir, Shell: "zsh"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if c.Cwd != dir {
		t.Errorf("Cwd = %q, want %q", c.Cwd, dir)
	}
	if c.Shell != "zsh" {
		t.Errorf("Shell = %q, want zsh", c.Shell)
	}
	if c.OS != runtime.GOOS || c.Arch != runtime.GOARCH {
		t.Errorf("OS/Arch = %s/%s, want %s/%s", c.OS, c.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if !strings.Contains(c.Listing, "file_a.txt") {
		t.Errorf("listing missing fixture files:\n%s", c.Listing)
	}

	// The userland call is the one that most often decides whether a
	// generated one-liner actually runs, so assert it rather than trust it.
	want := UserlandUnknown
	switch runtime.GOOS {
	case "darwin":
		want = UserlandBSD
	case "linux":
		want = UserlandGNU
	}
	if c.Userland != want {
		t.Errorf("Userland = %q, want %q on %s", c.Userland, want, runtime.GOOS)
	}
}

// Collection runs on every invocation and sits in front of the user's prompt,
// so it must stay inside its own budget and never block on a slow probe.
func TestCollectRespectsItsOwnTimeBudget(t *testing.T) {
	dir := fixture(t)
	start := time.Now()
	if _, err := Collect(context.Background(), Options{
		Cwd:       dir,
		Aggregate: 400 * time.Millisecond,
		PerCall:   150 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// Generous ceiling: the point is that it terminates promptly, not that it
	// hits a precise number on a loaded CI box.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("collection took %v; it runs in front of the user's prompt", elapsed)
	}
}

func TestCollectFailsOnMissingCwd(t *testing.T) {
	if _, err := Collect(context.Background(), Options{Cwd: filepath.Join(t.TempDir(), "gone")}); err == nil {
		t.Error("Collect with a nonexistent cwd should fail")
	}
}

func TestCollectReportsMissingTools(t *testing.T) {
	dir := fixture(t)
	// An empty PATH makes every probed tool absent, which is the branch that
	// matters: incant must know not to suggest a jq pipeline without jq.
	t.Setenv("PATH", filepath.Join(dir, "empty-bin"))

	c, err := Collect(context.Background(), Options{Cwd: dir})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(c.Missing) == 0 {
		t.Error("expected tools to be reported missing with an empty PATH")
	}
	if len(c.Tools) != 0 {
		t.Errorf("Tools = %v, want none with an empty PATH", c.Tools)
	}
	if !strings.Contains(c.Render(), "NOT installed:") {
		t.Errorf("render should flag missing tools:\n%s", c.Render())
	}
}

func TestRenderIncludesFactsAndStaysCompact(t *testing.T) {
	c := &Context{
		Cwd:       "/tmp/proj",
		Shell:     "zsh",
		OS:        "darwin",
		Arch:      "arm64",
		Userland:  UserlandBSD,
		GNUPrefix: []string{"gsed", "gdate"},
		Tools:     []string{"jq"},
		Missing:   []string{"rg"},
		Git:       "branch: main\nworking tree clean",
		Listing:   "./ (depth 1):\nfile_a.txt  14B",
	}
	got := c.Render()

	for _, want := range []string{
		"cwd: /tmp/proj",
		"shell: zsh on darwin/arm64",
		"userland: bsd",
		"GNU also available as gsed, gdate",
		"available: jq",
		"NOT installed: rg",
		"branch: main",
		"file_a.txt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}

	// Multi-line git state is flattened so it cannot dominate the block.
	if strings.Contains(got, "git: branch: main\nworking") {
		t.Errorf("git state should be flattened onto one line:\n%s", got)
	}
}

// The previous command only appears when the shell hook actually supplied one;
// a stale or absent value must not be presented to the model as current.
func TestRenderLastCommandOnlyWhenPresent(t *testing.T) {
	base := Context{Cwd: "/tmp", Shell: "zsh", Userland: UserlandBSD}

	if got := base.Render(); strings.Contains(got, "previous command") {
		t.Errorf("no last command should mean no line:\n%s", got)
	}

	withLast := base
	withLast.HasLast = true
	withLast.LastCommand = "ls -la | grep txt | xargs wc -l"
	withLast.LastStatus = 1
	withLast.LastStderr = "wc: illegal option -- l"

	got := withLast.Render()
	for _, want := range []string{
		"previous command (exit 1): ls -la | grep txt",
		"previous stderr:\n  wc: illegal option",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}

	// HasLast false must suppress the block even if the fields are populated,
	// so a stale ring-buffer entry cannot leak in as if it just happened.
	stale := withLast
	stale.HasLast = false
	if strings.Contains(stale.Render(), "previous command") {
		t.Error("HasLast=false must suppress the previous-command block")
	}
}

func TestDetectShellFallsBackToEnv(t *testing.T) {
	t.Setenv("SHELL", "/opt/homebrew/bin/fish")
	if got := detectShell(); got != "fish" {
		t.Errorf("detectShell() = %q, want fish", got)
	}
	t.Setenv("SHELL", "")
	if got := detectShell(); got != "sh" {
		t.Errorf("detectShell() with no SHELL = %q, want sh", got)
	}
}

func TestTailBytesKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("noise line\n", 400) + "error: the part that matters"
	got := tailBytes(long, maxStderr)
	if len(got) > maxStderr+len("…") || !strings.HasSuffix(got, "the part that matters") {
		t.Errorf("tailBytes kept the wrong end (%d bytes): %q", len(got), got[len(got)-40:])
	}
}

func TestContextLevels(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret-plans.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("x"), 0o644)

	full, err := Collect(context.Background(), Options{
		Cwd: dir, LastCommand: "make", HasLast: true,
		Hidden: func(abs string) bool { return strings.HasSuffix(abs, "secret-plans.txt") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Listing, "readme.md") || strings.Contains(full.Listing, "secret-plans") {
		t.Errorf("privacy rule not applied to listing:\n%s", full.Listing)
	}

	none, err := Collect(context.Background(), Options{
		Cwd: dir, LastCommand: "make", HasLast: true, NoFiles: true, NoHistory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := none.Render()
	if strings.Contains(r, "readme.md") || strings.Contains(r, "make") {
		t.Errorf("context=none still sends files or history:\n%s", r)
	}
}
