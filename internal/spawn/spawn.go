// Package spawn is the only place in incant that starts a process.
//
// incant's core promise is that it never runs the command it generates. That
// promise is worth more as a property of the code than as a sentence in a
// README, so process creation is confined to this one file and the set of
// binaries that can be started is the two auditable lists below.
//
// Two entry points, deliberately unequal:
//
//   - RunVersion hardcodes the entire argv as [tool, "--version"]. Callers
//     choose only the tool name, and only from versionOnly. This exists
//     because knowing whether the local sed is BSD or GNU changes what a
//     correct one-liner looks like.
//   - Run accepts caller-supplied arguments but only for the binaries in
//     fullAccess. That list is two entries long so that every argument
//     construction in the codebase can be audited by reading two call sites.
//
// Nothing here ever invokes a shell, so there is no quoting or metacharacter
// surface: arguments are passed as argv, and bulk or untrusted text goes over
// stdin rather than the command line.
package spawn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// versionOnly lists tools that may be started with exactly `--version` and
// nothing else. The flag is inert for all of them.
//
// Membership is not a judgement that a tool is harmless -- it is a judgement
// that `<tool> --version` does nothing. Adding a name here grants the power to
// run that binary, so the bar is: does it print a version and exit?
var versionOnly = map[string]bool{
	"awk": true, "bash": true, "curl": true, "fd": true, "ffmpeg": true,
	"find": true, "gawk": true, "gdate": true, "gfind": true, "git": true,
	"grep": true, "gsed": true, "gstat": true, "head": true, "jq": true,
	"node": true, "perl": true, "python3": true, "rg": true, "rsync": true,
	"sed": true, "sort": true, "sqlite3": true, "tail": true, "tar": true,
	"uniq": true, "wc": true, "xargs": true, "xsv": true, "yq": true,
	"zsh": true,
}

// fullAccess lists binaries that may be started with caller-supplied
// arguments. Keep this list tiny; every entry is a call site to audit.
//
//   - git: read-only state queries with a fixed argv (internal/probe).
//   - claude: the no-key fallback backend, which receives the user's request
//     on stdin, never on the command line (internal/backend).
var fullAccess = map[string]bool{
	"git":    true,
	"claude": true,
}

// toolNameRe constrains a tool name to a bare command: no paths, no
// metacharacters, nothing that could be read as anything but a name.
var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

var (
	// ErrNotAllowed means the binary is not on the relevant allowlist.
	ErrNotAllowed = errors.New("binary not on spawn allowlist")
	// ErrBadName means the name is not a bare command name.
	ErrBadName = errors.New("invalid binary name")
	// ErrNotFound means the binary is not present on PATH.
	ErrNotFound = errors.New("binary not found on PATH")
)

// Result is the outcome of a spawned process.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Lookup reports whether a tool exists on PATH without starting it.
// This is a pure lookup and is how presence questions should be answered.
func Lookup(name string) (string, error) {
	if !toolNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return path, nil
}

// RunVersion starts `<tool> --version`. The argv is fixed here; callers
// contribute only the tool name, which must be on versionOnly.
func RunVersion(ctx context.Context, name string, timeout time.Duration) (Result, error) {
	if !toolNameRe.MatchString(name) {
		return Result{}, fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if !versionOnly[name] {
		return Result{}, fmt.Errorf("%w: %s may not be started for a version check", ErrNotAllowed, name)
	}
	path, err := Lookup(name)
	if err != nil {
		return Result{}, err
	}
	return run(ctx, path, []string{"--version"}, "", timeout)
}

// Run starts a binary from fullAccess with the given arguments.
//
// stdin is the right channel for the user's request or any other bulk text:
// it keeps that content off the command line entirely, where it could never be
// re-parsed as an argument.
func Run(ctx context.Context, name string, args []string, stdin string, timeout time.Duration) (Result, error) {
	if !toolNameRe.MatchString(name) {
		return Result{}, fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if !fullAccess[name] {
		return Result{}, fmt.Errorf("%w: %s", ErrNotAllowed, name)
	}
	path, err := Lookup(name)
	if err != nil {
		return Result{}, err
	}
	return run(ctx, path, args, stdin, timeout)
}

// run is the single exec site.
func run(ctx context.Context, path string, args []string, stdin string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, path, args...)
	cmd.Env = minimalEnv()

	// Put the child in its own process group so cancellation can reach its
	// descendants. Killing only the direct child is not enough: a wrapper
	// script's own children survive, keep the inherited stdout pipe open, and
	// Wait then blocks until they finish -- so the timeout elapses without
	// actually releasing incant. That shows up as a frozen prompt, which is
	// the exact failure the timeout exists to prevent.
	//
	// Measured on darwin against `sh -c 'sleep 10'` with a 200ms timeout:
	// 10.2s with neither of the two mechanisms below, 2.2s with WaitDelay
	// alone, 200ms with the process-group kill. The group kill is the fix;
	// WaitDelay is the backstop that bounds the damage if anything escapes it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole group.
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	// Backstop: bound the wait even if a descendant still holds a pipe open.
	cmd.WaitDelay = 2 * time.Second

	if stdin == "" {
		cmd.Stdin = nil
	} else {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errBuf.String()}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		// A nonzero exit is data, not a failure of the spawn itself.
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, err
	}
	if cctx.Err() != nil {
		return res, fmt.Errorf("timed out after %s", timeout)
	}
	return res, nil
}

// minimalEnv is the environment every spawned process receives.
//
// The user's real environment routinely holds API keys and tokens, and none of
// these processes need them, so it is withheld. The exceptions are deliberate:
// PATH because the lookup already resolved through it, HOME because git and
// claude both need their config, and the git variables to guarantee git can
// never prompt, page, or open an editor.
func minimalEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		// USER is load-bearing, not cosmetic: without it the claude CLI's
		// macOS Keychain credential lookup fails and it reports itself as
		// logged out even for a user with a valid session.
		"USER=" + os.Getenv("USER"),
		"LC_ALL=C",
		"LANG=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"GIT_CONFIG_NOSYSTEM=1",
		"TERM=dumb",
	}
	// The fallback backend authenticates as the user's own CLI, so its
	// credential environment is passed through when present.
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_PROFILE", "XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
