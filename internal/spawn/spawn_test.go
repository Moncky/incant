package spawn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeScript puts an executable shim on PATH that records having run, so a
// test can prove a binary was *not* started rather than merely that a call
// returned an error.
func writeScript(t *testing.T, name, marker string) string {
	t.Helper()
	bin := t.TempDir()
	path := filepath.Join(bin, name)
	body := "#!/bin/sh\ntouch " + marker + "\necho ran-" + name + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestLookupValidatesNames(t *testing.T) {
	for _, bad := range []string{
		"", "  ", "/bin/sh", "../sh", "git;rm", "git rm", "$(id)", "a`b`",
		strings.Repeat("a", 100),
	} {
		if _, err := Lookup(bad); !errors.Is(err, ErrBadName) {
			t.Errorf("Lookup(%q) = %v, want ErrBadName", bad, err)
		}
	}
	if _, err := Lookup("git"); err != nil {
		t.Errorf("Lookup(git) = %v, want nil", err)
	}
	if _, err := Lookup("definitely-not-real-xyz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup(absent) = %v, want ErrNotFound", err)
	}
}

func TestRunVersionAllowsListedTool(t *testing.T) {
	res, err := RunVersion(context.Background(), "git", 3*time.Second)
	if err != nil {
		t.Fatalf("RunVersion(git): %v", err)
	}
	if !strings.Contains(res.Stdout, "git version") {
		t.Errorf("Stdout = %q, want a git version", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// The important half: an unlisted binary must not be started at all, not
// merely have its output discarded.
func TestRunVersionDoesNotStartUnlistedBinary(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	writeScript(t, "unlisted-tool", marker)

	_, err := RunVersion(context.Background(), "unlisted-tool", time.Second)
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("RunVersion(unlisted) = %v, want ErrNotAllowed", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("unlisted binary was executed; the allowlist is not holding")
	}
}

// versionOnly grants exactly one argv. A tool on that list must not be
// startable through Run with arbitrary arguments.
func TestRunRefusesVersionOnlyTools(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	writeScript(t, "sed", marker)

	_, err := Run(context.Background(), "sed", []string{"-i", "", "s/a/b/", "f"}, "", time.Second)
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("Run(sed) = %v, want ErrNotAllowed", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a version-only tool was started through Run")
	}
}

func TestRunRejectsUnknownBinary(t *testing.T) {
	for _, name := range []string{"rm", "dd", "curl", "python3", "unknown-xyz"} {
		if _, err := Run(context.Background(), name, []string{"--help"}, "", time.Second); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Run(%q) = %v, want ErrNotAllowed", name, err)
		}
	}
}

func TestRunDeliversStdinAndCapturesExitCode(t *testing.T) {
	// `git hash-object --stdin` reads stdin and is read-only, which makes it a
	// convenient way to prove stdin actually arrives.
	res, err := Run(context.Background(), "git",
		[]string{"hash-object", "--stdin"}, "hello incant\n", 3*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if len(strings.TrimSpace(res.Stdout)) != 40 {
		t.Errorf("Stdout = %q, want a 40-char object id (stdin not delivered?)", res.Stdout)
	}

	// A nonzero exit is data, not an error return.
	bad, err := Run(context.Background(), "git",
		[]string{"rev-parse", "--verify", "definitely-not-a-ref-xyz"}, "", 3*time.Second)
	if err != nil {
		t.Fatalf("Run(bad ref) returned a hard error: %v", err)
	}
	if bad.ExitCode == 0 {
		t.Error("expected a nonzero exit code to be reported")
	}
}

// The timeout is what keeps a hung subprocess from freezing the user's
// prompt, so it is exercised against a binary that genuinely blocks. This
// drives run() directly: the allowlists gate *which* binaries callers may
// name, and the concern here is the timeout mechanism underneath them.
//
// This is also a regression test, and the `sleep 10` shape is load-bearing
// rather than arbitrary. /bin/sh forks sleep and waits; cancelling the context
// kills only sh, so sleep survives holding the inherited stdout pipe and Wait
// blocks until it finishes on its own. Measured on darwin: with no process
// group handling this returns after 10.2s -- the timeout fires but never
// releases incant, which is a frozen prompt. WaitDelay alone bounds that to
// 2.2s; the process-group kill in run() brings it to 200ms.
func TestRunTimesOut(t *testing.T) {
	script := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := run(context.Background(), script, nil, "", 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("run(blocking) = %v, want a timeout error", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout took %v to fire; the process was not killed promptly", elapsed)
	}
}

// A cancelled parent context must also tear the child down.
func TestRunHonoursContextCancellation(t *testing.T) {
	script := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := run(ctx, script, nil, "", 30*time.Second); err == nil {
		t.Error("expected an error when the parent context is cancelled")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancellation took %v to take effect", elapsed)
	}
}

// Spawned processes must not inherit the user's environment: it routinely
// holds credentials that none of these subprocesses need.
func TestMinimalEnvWithholdsUserEnvironment(t *testing.T) {
	t.Setenv("MY_COMPANY_SECRET", "hunter2")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak-me")
	t.Setenv("GITHUB_TOKEN", "ghp_leak")

	env := strings.Join(minimalEnv(), "\n")

	for _, leak := range []string{"MY_COMPANY_SECRET", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "hunter2", "leak-me", "ghp_leak"} {
		if strings.Contains(env, leak) {
			t.Errorf("minimalEnv leaked %q:\n%s", leak, env)
		}
	}
	for _, want := range []string{"PATH=", "HOME=", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("minimalEnv missing %q", want)
		}
	}
}

// The fallback backend authenticates as the user's own CLI, so its
// credentials are the deliberate exception to the rule above.
func TestMinimalEnvPassesThroughClaudeCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.invalid")

	env := strings.Join(minimalEnv(), "\n")
	if !strings.Contains(env, "ANTHROPIC_API_KEY=sk-ant-test") {
		t.Error("expected ANTHROPIC_API_KEY to pass through for the claude fallback")
	}
	if !strings.Contains(env, "ANTHROPIC_BASE_URL=https://example.invalid") {
		t.Error("expected ANTHROPIC_BASE_URL to pass through")
	}
}

// An unset credential variable must not appear as an empty assignment: an
// empty ANTHROPIC_API_KEY outranks a working OAuth profile in the SDKs and
// would break the fallback for subscription users.
func TestMinimalEnvOmitsUnsetCredentials(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE"} {
		os.Unsetenv(k)
	}
	env := strings.Join(minimalEnv(), "\n")
	for _, k := range []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_PROFILE="} {
		if strings.Contains(env, k) {
			t.Errorf("unset credential %q must be omitted entirely, not set empty:\n%s", k, env)
		}
	}
}
