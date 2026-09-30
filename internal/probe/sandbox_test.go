package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestSandbox builds a sandbox over a temp dir. t.TempDir on macOS lives
// under /var -> /private/var, which is itself a symlink, so this exercises the
// resolved-root path by construction.
func newTestSandbox(t *testing.T) (*Sandbox, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSandbox(dir, nil, DefaultBudget())
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	return s, dir
}

func TestResolveRejectsEscape(t *testing.T) {
	s, dir := newTestSandbox(t)

	// A file outside the root that genuinely exists, so rejection is on
	// containment grounds and not merely because the path is missing.
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	for _, p := range []string{
		"../outside.txt",
		"./sub/../../outside.txt",
		outside,
		"/etc/passwd",
		"/",
	} {
		if _, err := s.Resolve(p); !errors.Is(err, ErrEscape) {
			t.Errorf("Resolve(%q) = %v, want ErrEscape", p, err)
		}
	}
}

// A symlink inside the sandbox pointing outside it must be rejected, not
// followed. This is the single most important test in the package: it is the
// difference between "reads your project" and "reads your whole disk".
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	s, dir := newTestSandbox(t)

	target := filepath.Join(filepath.Dir(dir), "escape-target.txt")
	if err := os.WriteFile(target, []byte("sensitive\n"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	t.Cleanup(func() { os.Remove(target) })

	link := filepath.Join(dir, "innocent.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := s.Resolve("innocent.txt"); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(symlink out) = %v, want ErrEscape", err)
	}

	// Same for a symlinked directory used as a path prefix.
	dirLink := filepath.Join(dir, "up")
	if err := os.Symlink(filepath.Dir(dir), dirLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := s.Resolve("up/escape-target.txt"); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(via symlinked dir) = %v, want ErrEscape", err)
	}
}

func TestResolveAllowsInsideRoot(t *testing.T) {
	s, dir := newTestSandbox(t)
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "a", "b", "ok.txt")
	if err := os.WriteFile(f, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"a/b/ok.txt", "./a/b/ok.txt", f, ".", "a"} {
		if _, err := s.Resolve(p); err != nil {
			t.Errorf("Resolve(%q) = %v, want nil", p, err)
		}
	}
}

// A sibling directory whose name shares a prefix with the root must not be
// treated as contained (/tmp/foo must not admit /tmp/foobar).
func TestResolveRejectsPrefixSibling(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "foo")
	sibling := filepath.Join(base, "foobar")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(sibling, "x.txt")
	if err := os.WriteFile(victim, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewSandbox(root, nil, DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(victim); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(prefix sibling) = %v, want ErrEscape", err)
	}
}

func TestAllowRootsPermitExtraTree(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	extra := filepath.Join(base, "logs")
	for _, d := range []string{root, extra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := filepath.Join(extra, "app.log")
	if err := os.WriteFile(f, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewSandbox(root, []string{extra}, DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(f); err != nil {
		t.Errorf("Resolve(allow-root file) = %v, want nil", err)
	}
	if got := s.Root(); filepath.Base(got) != "proj" {
		t.Errorf("Root() = %q, want the cwd root", got)
	}
}

// A nonexistent configured allow-root must not break the sandbox; a
// nonexistent cwd must.
func TestNewSandboxRootHandling(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewSandbox(dir, []string{filepath.Join(dir, "nope")}, nil); err != nil {
		t.Errorf("stale allow-root should be skipped, got %v", err)
	}
	if _, err := NewSandbox(filepath.Join(dir, "missing"), nil, nil); err == nil {
		t.Error("missing cwd should be fatal, got nil")
	}
}

func TestIsSecretPath(t *testing.T) {
	secret := []string{
		".env", ".env.production", ".ENV", "id_rsa", "deploy_ed25519",
		"server.pem", "tls.key", "store.p12", "creds.kdbx", ".netrc",
		".git-credentials", "credentials", ".ssh/config", ".aws/credentials",
		"home/.gnupg/secring", "sub/.env.local", ".pgpass", ".npmrc",
	}
	for _, p := range secret {
		if !IsSecretPath(p) {
			t.Errorf("IsSecretPath(%q) = false, want true", p)
		}
	}

	ordinary := []string{
		"main.go", "access.log", "README.md", "environment.yml",
		"keyboard.txt", "pemberton.csv", "envoy.conf", "data.json",
	}
	for _, p := range ordinary {
		if IsSecretPath(p) {
			t.Errorf("IsSecretPath(%q) = true, want false", p)
		}
	}
}

// Listing a directory may reveal that a secret file exists; reading its
// contents must not be possible. This asymmetry is intentional.
func TestResolveForReadDeniesSecretsButResolveAllowsListing(t *testing.T) {
	s, dir := newTestSandbox(t)
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("API_KEY=sk-live-abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Resolve(".env"); err != nil {
		t.Errorf("Resolve(.env) = %v; listing-level resolution should succeed", err)
	}
	if _, err := s.ResolveForRead(".env"); !errors.Is(err, ErrSecret) {
		t.Errorf("ResolveForRead(.env) = %v, want ErrSecret", err)
	}
}

// An innocuously-named symlink must not launder a secret target past the
// content-read gate.
func TestResolveForReadDeniesLaunderedSecret(t *testing.T) {
	s, dir := newTestSandbox(t)
	real := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(real, []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "notes.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := s.ResolveForRead("notes.txt"); !errors.Is(err, ErrSecret) {
		t.Errorf("ResolveForRead(laundered) = %v, want ErrSecret", err)
	}
}

func TestBudgetExhaustsByCallCount(t *testing.T) {
	b := NewBudget(3, time.Second, time.Minute)
	for i := 0; i < 3; i++ {
		if err := b.Take(); err != nil {
			t.Fatalf("Take %d = %v, want nil", i, err)
		}
	}
	if err := b.Take(); !errors.Is(err, ErrBudget) {
		t.Errorf("Take past limit = %v, want ErrBudget", err)
	}
	if got := b.Calls(); got != 3 {
		t.Errorf("Calls() = %d, want 3", got)
	}
}

func TestBudgetExhaustsByDeadline(t *testing.T) {
	b := NewBudget(100, time.Second, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if err := b.Take(); !errors.Is(err, ErrBudget) {
		t.Errorf("Take after deadline = %v, want ErrBudget", err)
	}
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining() = %v, want 0", got)
	}
}

// Probes are parallel-safe and run concurrently, so the budget must hand out
// exactly maxCalls permits under contention.
func TestBudgetIsConcurrencySafe(t *testing.T) {
	const limit = 8
	b := NewBudget(limit, time.Second, time.Minute)

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Take(); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != limit {
		t.Errorf("granted %d permits, want exactly %d", granted, limit)
	}
}

func TestRelHidesAbsolutePaths(t *testing.T) {
	s, dir := newTestSandbox(t)
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "a", "x.txt")
	if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, err := s.Resolve("a/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Rel(real), "./a/x.txt"; got != want {
		t.Errorf("Rel() = %q, want %q", got, want)
	}
	if got, want := s.Rel(s.Root()), "."; got != want {
		t.Errorf("Rel(root) = %q, want %q", got, want)
	}
}

// A path the user marked private must be invisible to every model-facing
// probe: not listed, not readable, not grepped, not in git state.
func TestHiddenPathsAreInvisible(t *testing.T) {
	s, dir := newTestSandbox(t)
	mustWrite(t, filepath.Join(dir, "public.txt"), "needle\n")
	mustWrite(t, filepath.Join(dir, "customers", "list.csv"), "needle\n")
	s.SetHidden(func(abs string) bool {
		return strings.Contains(abs, string(filepath.Separator)+"customers")
	})
	ctx := context.Background()

	listing, err := s.ListDir(ctx, ".", 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing, "customers") || strings.Contains(listing, "list.csv") {
		t.Errorf("hidden path listed:\n%s", listing)
	}
	for name, probe := range map[string]func() (string, error){
		"PeekFile": func() (string, error) { return s.PeekFile(ctx, "customers/list.csv", 5, 0) },
		"FileInfo": func() (string, error) { return s.FileInfo(ctx, "customers/list.csv") },
		"ListDir":  func() (string, error) { return s.ListDir(ctx, "customers", 1) },
	} {
		if _, err := probe(); !errors.Is(err, ErrPrivate) {
			t.Errorf("%s on hidden path: err = %v, want ErrPrivate", name, err)
		}
	}
	grep, err := s.GrepSample(ctx, "needle", ".")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(grep, "customers") || !strings.Contains(grep, "public.txt") {
		t.Errorf("grep leaked or missed:\n%s", grep)
	}
}
