// Package shellctx collects the facts about the user's shell session that are
// worth sending with every request.
//
// This is the cheap, always-on half of incant's context. It runs before the
// model is called and costs no round trips. The expensive half — probing the
// actual contents of files — is the model's job via internal/probe, and it
// happens only when the model decides it needs to.
package shellctx

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/probe"
)

// gnuProbes are the tools whose presence decides how a one-liner must be
// written on this machine. BSD and GNU userlands differ in exactly the places
// throwaway one-liners live -- `sed -i` wants an argument on BSD and not on
// GNU, `date -d` is GNU-only, `stat` flags are incompatible -- so getting this
// wrong produces a command that looks right and fails.
var gnuProbes = []string{"gsed", "gawk", "gdate", "gstat", "gfind"}

// toolProbes are the tools a one-liner might reach for, where knowing up front
// that they are missing avoids suggesting a pipeline that cannot run.
var toolProbes = []string{"jq", "rg", "fd"}

// Userland describes which flavour of the standard tools is in play.
type Userland string

const (
	UserlandGNU     Userland = "gnu"
	UserlandBSD     Userland = "bsd"
	UserlandUnknown Userland = "unknown"
)

// Context is the collected session state.
type Context struct {
	Cwd       string
	Listing   string
	Shell     string
	OS        string
	Arch      string
	Userland  Userland
	GNUPrefix []string // g-prefixed GNU tools present (gsed, gdate, ...)
	Tools     []string // other notable tools present
	Missing   []string // probed tools that are absent
	Git       string

	// LastCommand and LastStatus come from the shell's preexec/precmd hook.
	// They are what makes `incant --fix` possible: the model sees what failed
	// and why, rather than being asked to guess from a description.
	LastCommand string
	LastStatus  int
	LastStderr  string
	HasLast     bool
}

// Options controls collection. Zero values are sensible.
type Options struct {
	Cwd         string
	Shell       string
	AllowRoots  []string
	LastCommand string
	LastStatus  int
	LastStderr  string
	HasLast     bool

	// Hidden is the user's privacy rule (.incantignore). Hidden paths are
	// left out of the listing and git state.
	Hidden func(abs string) bool
	// NoFiles skips the listing and git state entirely: context = minimal,
	// or a .incantignore that makes the whole tree private.
	NoFiles bool
	// NoHistory drops the previous command and its output: context = none.
	NoHistory bool

	// Budget bounds collection. It is deliberately separate from the model's
	// probe budget: pre-collection must never eat into the allowance the
	// model needs for real reconnaissance.
	MaxCalls  int
	PerCall   time.Duration
	Aggregate time.Duration
}

func (o Options) budget() *probe.Budget {
	maxCalls := o.MaxCalls
	if maxCalls <= 0 {
		maxCalls = len(gnuProbes) + len(toolProbes) + 4
	}
	perCall := o.PerCall
	if perCall <= 0 {
		perCall = 300 * time.Millisecond
	}
	agg := o.Aggregate
	if agg <= 0 {
		agg = 900 * time.Millisecond
	}
	return probe.NewBudget(maxCalls, perCall, agg)
}

// Collect gathers session context. It is best-effort throughout: a probe that
// fails or times out leaves its field empty rather than failing the whole
// invocation, because a slightly thinner prompt beats no answer at all.
func Collect(ctx context.Context, opts Options) (*Context, error) {
	c := &Context{
		Cwd:         opts.Cwd,
		Shell:       opts.Shell,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Userland:    UserlandUnknown,
		LastCommand: opts.LastCommand,
		LastStatus:  opts.LastStatus,
		LastStderr:  opts.LastStderr,
		HasLast:     opts.HasLast,
	}
	if c.Shell == "" {
		c.Shell = detectShell()
	}
	if opts.NoHistory {
		c.LastCommand, c.LastStderr, c.HasLast = "", "", false
	}
	c.LastStderr = tailBytes(c.LastStderr, maxStderr)

	sb, err := probe.NewSandbox(opts.Cwd, opts.AllowRoots, opts.budget())
	if err != nil {
		return nil, fmt.Errorf("sandboxing cwd: %w", err)
	}
	sb.SetHidden(opts.Hidden)

	// The listing reuses the same sandboxed probe the model calls, so
	// pre-collection cannot see anything the model's own tools could not.
	if !opts.NoFiles {
		if listing, err := sb.ListDir(ctx, ".", 1); err == nil {
			c.Listing = strings.TrimRight(listing, "\n")
		}
		if git, err := sb.GitState(ctx); err == nil && !strings.Contains(git, "not a git repository") {
			c.Git = strings.TrimRight(git, "\n")
		}
	}

	// Userland flavour. On darwin the base tools are BSD unless the user has
	// put GNU coreutils ahead of them; the g-prefixed names are the reliable
	// signal that GNU equivalents are available at all.
	switch runtime.GOOS {
	case "darwin", "freebsd", "openbsd", "netbsd":
		c.Userland = UserlandBSD
	case "linux":
		c.Userland = UserlandGNU
	}

	for _, tool := range gnuProbes {
		if out, err := sb.WhichTool(ctx, tool); err == nil && strings.Contains(out, "installed at") {
			c.GNUPrefix = append(c.GNUPrefix, tool)
		}
	}
	for _, tool := range toolProbes {
		out, err := sb.WhichTool(ctx, tool)
		if err != nil {
			continue
		}
		if strings.Contains(out, "installed at") {
			c.Tools = append(c.Tools, tool)
		} else {
			c.Missing = append(c.Missing, tool)
		}
	}

	return c, nil
}

// maxStderr caps the previous command's captured stderr. The end of the
// output is where the error that matters usually is.
const maxStderr = 2000

// tailBytes keeps the last n bytes of s, starting at a line boundary when one
// is near.
func tailBytes(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < 200 {
		s = s[i+1:]
	}
	return "…" + s
}

// detectShell falls back to $SHELL when the widget did not say.
func detectShell() string {
	// Read via the environment rather than a process lookup; the widget passes
	// --shell explicitly in the normal path, so this is only a fallback.
	if sh := envShell(); sh != "" {
		return sh
	}
	return "sh"
}

// Render produces the context block sent to the model.
//
// Every line here costs input tokens on every single invocation, so this stays
// terse: facts the model cannot derive, nothing it can. The listing dominates
// and is already capped by the probe layer.
func (c *Context) Render() string {
	var b strings.Builder

	fmt.Fprintf(&b, "cwd: %s\n", c.Cwd)
	fmt.Fprintf(&b, "shell: %s on %s/%s\n", c.Shell, c.OS, c.Arch)

	fmt.Fprintf(&b, "userland: %s", c.Userland)
	if len(c.GNUPrefix) > 0 {
		fmt.Fprintf(&b, " (GNU also available as %s)", strings.Join(c.GNUPrefix, ", "))
	}
	b.WriteString("\n")

	if len(c.Tools) > 0 {
		fmt.Fprintf(&b, "available: %s\n", strings.Join(c.Tools, ", "))
	}
	if len(c.Missing) > 0 {
		fmt.Fprintf(&b, "NOT installed: %s\n", strings.Join(c.Missing, ", "))
	}
	if c.Git != "" {
		fmt.Fprintf(&b, "git: %s\n", strings.ReplaceAll(c.Git, "\n", "; "))
	}
	if c.HasLast && c.LastCommand != "" {
		fmt.Fprintf(&b, "previous command (exit %d): %s\n", c.LastStatus, c.LastCommand)
		if c.LastStderr != "" {
			fmt.Fprintf(&b, "previous stderr:\n%s\n", indent(c.LastStderr))
		}
	}
	if c.Listing != "" {
		fmt.Fprintf(&b, "\ndirectory listing:\n%s\n", c.Listing)
	}
	return b.String()
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
