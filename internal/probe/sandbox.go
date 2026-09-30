// Package probe implements incant's read-only reconnaissance tools.
//
// Every tool the model can call lives here, and every one of them is a Go
// function rather than a shell command string. That is deliberate: the model
// never gets to compose a command that this process executes, so there is no
// metacharacter or quoting surface to get wrong. The Sandbox below is the only
// path through which those functions touch the filesystem.
package probe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Default budget limits. A probe loop that blows through these answers with
// whatever it has rather than stalling the user's prompt.
const (
	DefaultMaxCalls    = 8
	DefaultPerCall     = 2 * time.Second
	DefaultAggregate   = 5 * time.Second
	DefaultMaxFileRead = 64 << 10 // 64KB
)

var (
	// ErrEscape means a path resolved outside every allowed root.
	ErrEscape = errors.New("path escapes sandbox root")
	// ErrSecret means the path looks like it holds credentials.
	ErrSecret = errors.New("path matches a secret pattern; contents withheld")
	// ErrBudget means the probe budget for this invocation is spent.
	ErrBudget = errors.New("probe budget exhausted")
	// ErrNotRegular means the path is not a regular file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrBinary means the file does not look like text.
	ErrBinary = errors.New("file appears to be binary")
	// ErrPrivate means the user has marked the path private in .incantignore.
	ErrPrivate = errors.New("path is private (.incantignore)")
)

// secretNames are exact basenames whose contents are never read.
var secretNames = map[string]bool{
	".env": true, ".netrc": true, "_netrc": true, ".npmrc": true,
	".pypirc": true, ".htpasswd": true, "credentials": true,
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	".git-credentials": true, ".pgpass": true,
}

// secretSuffixes are extensions whose contents are never read.
var secretSuffixes = []string{
	".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".kdbx", ".asc",
	"_rsa", "_dsa", "_ecdsa", "_ed25519",
}

// secretDirs are directory names that make everything beneath them off limits.
var secretDirs = map[string]bool{
	".ssh": true, ".aws": true, ".gnupg": true, ".gpg": true,
	".docker": true, ".kube": true,
}

// IsSecretPath reports whether a path's contents should never be sampled.
//
// Directory listings are deliberately NOT gated on this: knowing that a .env
// exists is useful to the model and is far less sensitive than its contents.
// Only the content-reading probes consult this.
func IsSecretPath(p string) bool {
	base := filepath.Base(p)
	lower := strings.ToLower(base)

	if secretNames[lower] {
		return true
	}
	// .env.production, .env.local, ...
	if strings.HasPrefix(lower, ".env.") {
		return true
	}
	for _, suf := range secretSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if secretDirs[strings.ToLower(part)] {
			return true
		}
	}
	return false
}

// Budget caps how much work one incant invocation may do. Probes are
// parallel-safe and run concurrently, so every method is mutex-guarded.
type Budget struct {
	mu       sync.Mutex
	maxCalls int
	calls    int
	perCall  time.Duration
	deadline time.Time
}

// NewBudget returns a budget starting its aggregate clock now.
func NewBudget(maxCalls int, perCall, aggregate time.Duration) *Budget {
	return &Budget{
		maxCalls: maxCalls,
		perCall:  perCall,
		deadline: time.Now().Add(aggregate),
	}
}

// DefaultBudget is the budget used when config says nothing.
func DefaultBudget() *Budget {
	return NewBudget(DefaultMaxCalls, DefaultPerCall, DefaultAggregate)
}

// Take reserves one probe call. It returns ErrBudget once the call count or
// the aggregate deadline is spent.
func (b *Budget) Take() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls >= b.maxCalls {
		return fmt.Errorf("%w: %d calls used", ErrBudget, b.calls)
	}
	if !time.Now().Before(b.deadline) {
		return fmt.Errorf("%w: aggregate deadline passed", ErrBudget)
	}
	b.calls++
	return nil
}

// Calls reports how many probe calls have been taken.
func (b *Budget) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// PerCall is the wall-clock ceiling for a single probe.
func (b *Budget) PerCall() time.Duration { return b.perCall }

// Remaining is the time left in the aggregate budget, floored at zero.
func (b *Budget) Remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := time.Until(b.deadline); d > 0 {
		return d
	}
	return 0
}

// Sandbox is the single gate between the probe tools and the filesystem.
// Construct it once per invocation with the user's cwd.
type Sandbox struct {
	roots  []string // symlink-resolved absolute allowed roots; roots[0] is cwd
	budget *Budget
	hidden func(abs string) bool
}

// SetHidden installs the user's privacy rule. Hidden paths are left out of
// listings and git state and refused by every content probe, so the model
// never learns they exist.
//
// The enumeration probes used by the effect preview ignore it: the preview
// never leaves the machine, and hiding a file from it would understate what a
// command deletes.
func (s *Sandbox) SetHidden(fn func(abs string) bool) { s.hidden = fn }

// isHidden reports whether an absolute path is private.
func (s *Sandbox) isHidden(abs string) bool {
	return s.hidden != nil && s.hidden(abs)
}

// NewSandbox roots a sandbox at cwd, plus any extra allow-roots from config.
// Every root is symlink-resolved up front so containment checks compare
// like with like.
func NewSandbox(cwd string, allowRoots []string, b *Budget) (*Sandbox, error) {
	if b == nil {
		b = DefaultBudget()
	}
	s := &Sandbox{budget: b}
	for _, r := range append([]string{cwd}, allowRoots...) {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("resolving root %q: %w", r, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			// A configured root that does not exist is skipped rather than
			// fatal, so a stale config entry cannot break every invocation.
			// The cwd itself must exist.
			if len(s.roots) == 0 {
				return nil, fmt.Errorf("resolving cwd %q: %w", r, err)
			}
			continue
		}
		s.roots = append(s.roots, filepath.Clean(real))
	}
	if len(s.roots) == 0 {
		return nil, errors.New("sandbox has no usable roots")
	}
	return s, nil
}

// Budget exposes the invocation budget.
func (s *Sandbox) Budget() *Budget { return s.budget }

// Root is the primary root (the user's cwd).
func (s *Sandbox) Root() string { return s.roots[0] }

// contains reports whether an already-resolved absolute path sits within a root.
func (s *Sandbox) contains(real string) bool {
	for _, root := range s.roots {
		if real == root {
			return true
		}
		// Compare with a trailing separator so /tmp/foobar does not match
		// a /tmp/foo root.
		prefix := root
		if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
			prefix += string(os.PathSeparator)
		}
		if strings.HasPrefix(real, prefix) {
			return true
		}
	}
	return false
}

// Resolve turns a model-supplied path into a real absolute path inside the
// sandbox, or returns an error.
//
// Symlinks are fully resolved BEFORE the containment check, so a symlink inside
// the cwd pointing at /etc/passwd is rejected rather than followed. Relative
// paths resolve against the primary root; absolute paths are permitted only if
// they land inside a root.
func (s *Sandbox) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		p = "."
	}
	cand := p
	if !filepath.IsAbs(cand) {
		cand = filepath.Join(s.roots[0], cand)
	}

	real, err := filepath.EvalSymlinks(cand)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s: no such file or directory", p)
		}
		return "", fmt.Errorf("resolving %q: %w", p, err)
	}
	real = filepath.Clean(real)

	if !s.contains(real) {
		return "", fmt.Errorf("%w: %s", ErrEscape, p)
	}
	return real, nil
}

// ResolveForRead is Resolve plus the secret-path check. Content-reading probes
// must use this; directory listing uses plain Resolve.
func (s *Sandbox) ResolveForRead(p string) (string, error) {
	real, err := s.Resolve(p)
	if err != nil {
		return "", err
	}
	// Check both the path as given and the resolved path: a symlink named
	// innocuously must not launder a secret target.
	rel, _ := filepath.Rel(s.roots[0], real)
	if IsSecretPath(p) || IsSecretPath(real) || (rel != "" && IsSecretPath(rel)) {
		return "", fmt.Errorf("%w: %s", ErrSecret, filepath.Base(p))
	}
	if s.isHidden(real) {
		return "", fmt.Errorf("%w: %s", ErrPrivate, p)
	}
	return real, nil
}

// Rel renders a resolved path relative to the primary root for display back to
// the model, so probe results never leak the user's home directory layout.
func (s *Sandbox) Rel(real string) string {
	if rel, err := filepath.Rel(s.roots[0], real); err == nil && !strings.HasPrefix(rel, "..") {
		if rel == "." {
			return "."
		}
		return "./" + filepath.ToSlash(rel)
	}
	return filepath.ToSlash(real)
}
