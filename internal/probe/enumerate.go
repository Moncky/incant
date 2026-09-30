package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/glob"
	"github.com/callumscott/incant/internal/spawn"
)

// The enumeration probes below answer "which paths would this touch?" for the
// effect preview in internal/impact. Like ListDir they report metadata only --
// names, sizes, modes, times -- and never file contents, so they are not gated
// on IsSecretPath: knowing that `rm -rf .` would delete .env is exactly the
// point.

// Entry is the metadata for one path. A symlink is described as itself, not
// its target, because that is what rm, mv and chmod -h act on.
type Entry struct {
	Real    string // absolute path, parent symlinks resolved, leaf not followed
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
	// Children is the number of entries in a directory, or -1 when it was not
	// read (not a directory, or unreadable).
	Children int
}

func (e Entry) IsDir() bool     { return e.Mode.IsDir() }
func (e Entry) IsLink() bool    { return e.Mode&fs.ModeSymlink != 0 }
func (e Entry) IsRegular() bool { return e.Mode.IsRegular() }

func entryFrom(real string, fi fs.FileInfo) Entry {
	return Entry{Real: real, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(), Children: -1}
}

// Stat describes one path without following a final symlink.
//
// The parent is symlink-resolved and the leaf appended, and containment is
// checked on that result. A symlink inside the cwd pointing outside it is
// therefore describable -- deleting the link is local -- while a path whose
// parent escapes is refused. A missing path yields an error wrapping
// fs.ErrNotExist.
func (s *Sandbox) Stat(ctx context.Context, p string) (Entry, error) {
	if err := s.budget.Take(); err != nil {
		return Entry{}, err
	}
	return s.stat(p)
}

func (s *Sandbox) stat(p string) (Entry, error) {
	real, err := s.leafPath(p)
	if err != nil {
		return Entry{}, err
	}
	fi, err := os.Lstat(real)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Entry{}, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
		return Entry{}, err
	}
	return entryFrom(real, fi), nil
}

// leafPath resolves p's parent and appends its base name, then checks
// containment.
func (s *Sandbox) leafPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		p = "."
	}
	cand := p
	if !filepath.IsAbs(cand) {
		cand = filepath.Join(s.roots[0], cand)
	}
	cand = filepath.Clean(cand)

	parent, err := filepath.EvalSymlinks(filepath.Dir(cand))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
		return "", fmt.Errorf("resolving %q: %w", p, err)
	}
	real := filepath.Join(parent, filepath.Base(cand))
	if filepath.Dir(cand) == cand { // cand is "/"
		real = cand
	}
	if !s.contains(real) {
		return "", fmt.Errorf("%w: %s", ErrEscape, p)
	}
	return real, nil
}

// MatchOptions controls pathname expansion.
type MatchOptions struct {
	// Globstar makes a ** segment match any number of directories, as zsh does
	// by default and bash does only with shopt -s globstar.
	Globstar bool
	// Limit caps the number of results. Zero means a generous default.
	Limit int
}

const defaultMatchLimit = 50000

// Match performs shell pathname expansion of pattern inside the sandbox.
//
// The pattern uses backslash escapes for characters that were quoted in the
// original command. A pattern without wildcards is a plain Stat. Expansion
// never leaves the sandbox: a pattern whose literal prefix escapes is refused,
// and a symlinked directory crossed mid-pattern is re-checked for containment.
// Hidden names match only a segment that itself starts with a dot, as in the
// shell. Missing literal paths and wildcards with no match both yield an empty
// result rather than an error.
func (s *Sandbox) Match(ctx context.Context, pattern string, opt MatchOptions) ([]Entry, bool, error) {
	if err := s.budget.Take(); err != nil {
		return nil, false, err
	}
	if opt.Limit <= 0 {
		opt.Limit = defaultMatchLimit
	}
	ctx, cancel := s.callContext(ctx)
	defer cancel()

	if !glob.HasMeta(pattern) {
		e, err := s.stat(glob.Unescape(pattern))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return []Entry{e}, false, nil
	}

	dirsOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimRight(pattern, "/")

	// Split off the literal directory prefix; expansion starts below it.
	start := s.roots[0]
	rest := pattern
	if filepath.IsAbs(pattern) {
		start = "/"
		rest = strings.TrimLeft(pattern, "/")
	}
	segs := strings.Split(rest, "/")
	lit := 0
	for lit < len(segs)-1 && !glob.HasMeta(segs[lit]) {
		lit++
	}
	if lit > 0 {
		start = filepath.Join(start, glob.Unescape(strings.Join(segs[:lit], "/")))
	}
	base, err := s.Resolve(start)
	if err != nil {
		if errors.Is(err, ErrEscape) {
			return nil, false, err
		}
		return nil, false, nil // missing prefix directory: no matches
	}

	m := &matcher{s: s, ctx: ctx, opt: opt, seen: map[string]bool{}}
	m.expand(base, segs[lit:])
	if err := m.err; err != nil {
		return nil, false, err
	}

	out := m.out
	if dirsOnly {
		kept := out[:0]
		for _, e := range out {
			if e.IsDir() {
				kept = append(kept, e)
			}
		}
		out = kept
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Real < out[j].Real })
	return out, m.truncated, nil
}

type matcher struct {
	s         *Sandbox
	ctx       context.Context
	opt       MatchOptions
	out       []Entry
	seen      map[string]bool
	truncated bool
	err       error
}

func (m *matcher) add(real string) {
	if m.seen[real] {
		return
	}
	if len(m.out) >= m.opt.Limit {
		m.truncated = true
		return
	}
	fi, err := os.Lstat(real)
	if err != nil {
		return
	}
	m.seen[real] = true
	m.out = append(m.out, entryFrom(real, fi))
}

func (m *matcher) stop() bool {
	if m.truncated || m.err != nil {
		return true
	}
	if m.ctx.Err() != nil {
		m.truncated = true
		return true
	}
	return false
}

// expand matches segs below the resolved directory dir.
func (m *matcher) expand(dir string, segs []string) {
	if m.stop() || len(segs) == 0 {
		return
	}
	seg, more := segs[0], segs[1:]

	if seg == "**" && m.opt.Globstar && len(more) > 0 {
		// Zero directories...
		m.expand(dir, more)
		// ...or one more, keeping ** in front. zsh's ** neither follows
		// symlinks nor descends into hidden directories.
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if m.stop() {
				return
			}
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				m.expand(filepath.Join(dir, e.Name()), segs)
			}
		}
		return
	}

	if !glob.HasMeta(seg) {
		m.descend(filepath.Join(dir, glob.Unescape(seg)), more)
		return
	}

	re, err := glob.Compile(seg, glob.Options{})
	if err != nil {
		m.err = err
		return
	}
	showHidden := strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, `\.`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if m.stop() {
			return
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") && !showHidden {
			continue
		}
		if re.MatchString(name) {
			m.descend(filepath.Join(dir, name), more)
		}
	}
}

// descend adds path when it is the last segment, or crosses into it as a
// directory, following a symlink as the shell does. A crossing that leaves the
// sandbox is an error, not a non-match: the shell would expand it, so treating
// it as empty would report a command as harmless when it is merely unseen.
func (m *matcher) descend(path string, more []string) {
	if len(more) == 0 {
		m.add(path)
		return
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return
	}
	if !m.s.contains(real) {
		m.err = fmt.Errorf("%w: %s", ErrEscape, m.s.Rel(path))
		return
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return
	}
	m.expand(real, more)
}

// ErrSkipDir, returned from a Walk callback on a directory, skips its contents.
var ErrSkipDir = fs.SkipDir

// Walk visits root and everything beneath it, pre-order, without following
// symlinks. maxDepth < 0 means unlimited; root is depth 0.
//
// Walk stops early -- reporting truncated rather than an error -- when limit
// entries have been visited or the budget's time runs out, so a preview of
// `rm -rf ~/huge` degrades to "at least N files" instead of stalling the
// prompt.
func (s *Sandbox) Walk(ctx context.Context, root string, maxDepth, limit int, fn func(e Entry, depth int) error) (bool, error) {
	if err := s.budget.Take(); err != nil {
		return false, err
	}
	if !s.contains(root) {
		return false, fmt.Errorf("%w: %s", ErrEscape, root)
	}
	if limit <= 0 {
		limit = defaultMatchLimit
	}
	ctx, cancel := s.callContext(ctx)
	defer cancel()

	fi, err := os.Lstat(root)
	if err != nil {
		return false, err
	}

	count := 0
	truncated := false
	var visit func(e Entry, depth int) error
	visit = func(e Entry, depth int) error {
		if count >= limit || ctx.Err() != nil {
			truncated = true
			return fs.SkipAll
		}
		count++

		var children []os.DirEntry
		if e.IsDir() {
			if entries, err := os.ReadDir(e.Real); err == nil {
				children = entries
				e.Children = len(entries)
			}
		}
		if err := fn(e, depth); err != nil {
			if errors.Is(err, fs.SkipDir) {
				return nil
			}
			return err
		}
		if maxDepth >= 0 && depth >= maxDepth {
			return nil
		}
		for _, c := range children {
			info, err := c.Info()
			if err != nil {
				continue
			}
			if err := visit(entryFrom(filepath.Join(e.Real, c.Name()), info), depth+1); err != nil {
				return err
			}
		}
		return nil
	}

	err = visit(entryFrom(root, fi), 0)
	if errors.Is(err, fs.SkipAll) {
		err = nil
	}
	return truncated, err
}

// callContext bounds one enumeration by the per-call ceiling and whatever is
// left of the aggregate budget.
func (s *Sandbox) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	d := min(s.budget.PerCall(), s.budget.Remaining())
	return context.WithTimeout(ctx, d)
}

// GitChange is one line of `git status --porcelain`.
type GitChange struct {
	Code string // the two-letter XY status, e.g. " M", "??", "R "
	Path string // relative to the sandbox root
}

// Untracked reports whether the change is an untracked path.
func (c GitChange) Untracked() bool { return c.Code == "??" }

// GitStatus lists working-tree changes. ok is false outside a repository.
//
// Both argvs are fixed. Porcelain paths are relative to the repository root,
// so the cwd's prefix within the repo is fetched too and paths are re-rooted
// at the cwd; changes elsewhere in the repo come back with a ../ prefix.
func (s *Sandbox) GitStatus(ctx context.Context) ([]GitChange, bool, error) {
	if err := s.budget.Take(); err != nil {
		return nil, false, err
	}
	if _, err := spawn.Lookup("git"); err != nil {
		return nil, false, nil
	}
	pre, err := spawn.Run(ctx, "git",
		[]string{"-C", s.Root(), "rev-parse", "--show-prefix"},
		"", s.budget.PerCall())
	if err != nil || pre.ExitCode != 0 {
		return nil, false, nil
	}
	prefix := strings.TrimSpace(pre.Stdout)

	res, err := spawn.Run(ctx, "git",
		[]string{"-C", s.Root(), "status", "--porcelain=v1", "-z", "--untracked-files=normal"},
		"", s.budget.PerCall())
	if err != nil {
		return nil, true, err
	}
	if res.ExitCode != 0 {
		return nil, true, fmt.Errorf("git status exited %d", res.ExitCode)
	}

	var out []GitChange
	fields := strings.Split(res.Stdout, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		code, p := f[:2], f[3:]
		// A rename or copy is followed by its source path as its own field.
		if code[0] == 'R' || code[0] == 'C' {
			i++
		}
		out = append(out, GitChange{Code: code, Path: rerootGitPath(p, prefix)})
	}
	return out, true, nil
}

// rerootGitPath turns a repo-root-relative path into a cwd-relative one.
func rerootGitPath(p, prefix string) string {
	if prefix == "" {
		return p
	}
	if strings.HasPrefix(p, prefix) {
		return strings.TrimPrefix(p, prefix)
	}
	up := strings.Repeat("../", strings.Count(strings.TrimSuffix(prefix, "/"), "/")+1)
	return up + p
}
