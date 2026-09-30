package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/spawn"
)

// Probe limits. These are ceilings on what any single call may return, so a
// pathological directory or a multi-gigabyte log cannot blow out the context
// window or the latency budget.
const (
	maxListEntries = 500
	maxListDepth   = 2
	maxPeekLines   = 40
	maxGrepHits    = 5
	sniffBytes     = 8000
)

// ListDir reports the entries under path, breadth-first to maxListDepth.
//
// Unlike the content probes this uses plain Resolve: revealing that a .env
// exists is useful context and far less sensitive than its contents.
func (s *Sandbox) ListDir(ctx context.Context, path string, depth int) (string, error) {
	if err := s.budget.Take(); err != nil {
		return "", err
	}
	if depth < 1 {
		depth = 1
	}
	if depth > maxListDepth {
		depth = maxListDepth
	}

	root, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	if s.isHidden(root) {
		return "", fmt.Errorf("%w: %s", ErrPrivate, path)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", s.Rel(root))
	}

	var (
		out       strings.Builder
		count     int
		truncated bool
		extCount  = map[string]int{}
	)
	fmt.Fprintf(&out, "%s (depth %d):\n", s.Rel(root), depth)

	var walk func(dir string, level int) error
	walk = func(dir string, level int) error {
		if level > depth || truncated {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil // unreadable subdirectory is not fatal to the listing
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		for _, e := range entries {
			if count >= maxListEntries {
				truncated = true
				return nil
			}
			full := filepath.Join(dir, e.Name())
			if s.isHidden(full) {
				continue // private: not even its name is shown
			}
			indent := strings.Repeat("  ", level)

			if e.IsDir() {
				fmt.Fprintf(&out, "%s%s/\n", indent, e.Name())
				count++
				if err := walk(full, level+1); err != nil {
					return err
				}
				continue
			}

			info, err := e.Info()
			size := int64(-1)
			if err == nil {
				size = info.Size()
			}
			kind := ""
			if e.Type()&os.ModeSymlink != 0 {
				kind = " -> symlink"
			}
			fmt.Fprintf(&out, "%s%s  %s%s\n", indent, e.Name(), humanSize(size), kind)
			count++
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == "" {
				ext = "(none)"
			}
			extCount[ext]++
		}
		return nil
	}

	if err := walk(root, 0); err != nil {
		return "", err
	}

	// An extension histogram is the compact way to describe "a directory full
	// of .mp3 with two .txt in it" without spending 500 lines on filenames.
	if len(extCount) > 1 || truncated {
		type kv struct {
			ext string
			n   int
		}
		var hist []kv
		for k, v := range extCount {
			hist = append(hist, kv{k, v})
		}
		sort.Slice(hist, func(i, j int) bool {
			if hist[i].n != hist[j].n {
				return hist[i].n > hist[j].n
			}
			return hist[i].ext < hist[j].ext
		})
		var parts []string
		for _, h := range hist {
			parts = append(parts, fmt.Sprintf("%s=%d", h.ext, h.n))
		}
		fmt.Fprintf(&out, "by extension: %s\n", strings.Join(parts, " "))
	}
	if truncated {
		fmt.Fprintf(&out, "(truncated at %d entries)\n", maxListEntries)
	}
	return out.String(), nil
}

// PeekFile returns the first and/or last lines of a text file. This is the
// probe that turns a guessed awk into a correct one: it shows the model the
// file's real delimiter and field order.
func (s *Sandbox) PeekFile(ctx context.Context, path string, head, tail int) (string, error) {
	if err := s.budget.Take(); err != nil {
		return "", err
	}
	if head <= 0 && tail <= 0 {
		head = 10
	}
	head = min(head, maxPeekLines)
	tail = min(tail, maxPeekLines)

	real, err := s.ResolveForRead(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s", ErrNotRegular, s.Rel(real))
	}

	f, err := os.Open(real)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Sniff before reading: dumping a binary into the context window is pure
	// waste and may carry secrets that no name-based rule would catch.
	sniff := make([]byte, min(int(fi.Size()), sniffBytes))
	n, _ := io.ReadFull(f, sniff)
	if bytes.IndexByte(sniff[:n], 0) >= 0 {
		return "", fmt.Errorf("%w: %s (%s)", ErrBinary, s.Rel(real), humanSize(fi.Size()))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%s (%s):\n", s.Rel(real), humanSize(fi.Size()))

	if head > 0 {
		lines, err := readHeadLines(ctx, f, head)
		if err != nil {
			return "", err
		}
		for i, l := range lines {
			fmt.Fprintf(&out, "%d: %s\n", i+1, l)
		}
	}
	if tail > 0 && fi.Size() > 0 {
		lines, err := readTailLines(f, fi.Size(), tail)
		if err != nil {
			return "", err
		}
		if head > 0 && len(lines) > 0 {
			out.WriteString("...\n")
		}
		for _, l := range lines {
			fmt.Fprintf(&out, "tail: %s\n", l)
		}
	}
	return out.String(), nil
}

// FileInfo reports type, size, mode, mtime and line count without returning
// any of the file's contents, so it is safe to call on paths PeekFile refuses.
func (s *Sandbox) FileInfo(ctx context.Context, path string) (string, error) {
	if err := s.budget.Take(); err != nil {
		return "", err
	}
	real, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	if s.isHidden(real) {
		return "", fmt.Errorf("%w: %s", ErrPrivate, path)
	}
	fi, err := os.Lstat(real)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	kind := "file"
	switch {
	case fi.IsDir():
		kind = "directory"
	case fi.Mode()&os.ModeSymlink != 0:
		kind = "symlink"
	case !fi.Mode().IsRegular():
		kind = "special"
	}
	fmt.Fprintf(&out, "%s: %s, %s, mode %s, modified %s\n",
		s.Rel(real), kind, humanSize(fi.Size()),
		fi.Mode().Perm(), fi.ModTime().Format(time.RFC3339))

	if !fi.Mode().IsRegular() {
		return out.String(), nil
	}

	// Counting lines reads the body, so it is gated like any content read.
	if IsSecretPath(path) || IsSecretPath(real) {
		out.WriteString("contents: withheld (secret-like path)\n")
		return out.String(), nil
	}
	f, err := os.Open(real)
	if err != nil {
		return out.String(), nil
	}
	defer f.Close()

	lines, binary, err := countLines(ctx, f)
	if err != nil {
		return out.String(), nil
	}
	if binary {
		out.WriteString("contents: binary\n")
	} else {
		fmt.Fprintf(&out, "contents: text, %d lines\n", lines)
	}
	return out.String(), nil
}

// WhichTool answers whether a command exists on PATH, and its version when
// internal/spawn permits starting it.
//
// Presence is a pure PATH lookup that executes nothing. The version is a
// separate question, because running a binary the model named is a real
// capability: spawn gates it behind an allowlist of tools whose --version is
// inert, and an unlisted tool still gets a presence answer.
func (s *Sandbox) WhichTool(ctx context.Context, name string) (string, error) {
	if err := s.budget.Take(); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)

	path, err := spawn.Lookup(name)
	if err != nil {
		if errors.Is(err, spawn.ErrBadName) {
			return "", fmt.Errorf("invalid tool name %q: expected a bare command name", name)
		}
		return fmt.Sprintf("%s: not installed\n", name), nil
	}

	res, err := spawn.RunVersion(ctx, name, s.budget.PerCall())
	if err != nil {
		if errors.Is(err, spawn.ErrNotAllowed) {
			return fmt.Sprintf("%s: installed at %s (version not probed)\n", name, path), nil
		}
		return fmt.Sprintf("%s: installed at %s (version unavailable)\n", name, path), nil
	}
	version := firstLine(res.Stdout + "\n" + res.Stderr)
	if version == "" {
		return fmt.Sprintf("%s: installed at %s (version unavailable)\n", name, path), nil
	}
	return fmt.Sprintf("%s: installed at %s, %s\n", name, path, version), nil
}

// GrepSample reports whether a regex matches under path and shows a few hits.
//
// This is implemented with Go's regexp rather than by shelling out to grep:
// RE2 is linear-time so a model-supplied pattern cannot blow up into
// catastrophic backtracking, and there is no shell to quote against.
func (s *Sandbox) GrepSample(ctx context.Context, pattern, path string) (string, error) {
	if err := s.budget.Take(); err != nil {
		return "", err
	}
	if strings.TrimSpace(pattern) == "" {
		return "", errors.New("empty pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}

	root, err := s.Resolve(path)
	if err != nil {
		return "", err
	}

	cctx, cancel := context.WithTimeout(ctx, s.budget.PerCall())
	defer cancel()

	var (
		out      strings.Builder
		hits     int
		scanned  int
		filesHit int
	)
	fmt.Fprintf(&out, "pattern %q under %s:\n", pattern, s.Rel(root))

	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if cctx.Err() != nil {
			return filepath.SkipAll
		}
		if hits >= maxGrepHits {
			return filepath.SkipAll
		}
		if d.IsDir() {
			// Skip the usual dependency and VCS dumps; they dominate the
			// walk and never hold the answer.
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".venv", "__pycache__", "target":
				if p != root {
					return filepath.SkipDir
				}
			}
			if s.isHidden(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || IsSecretPath(p) || s.isHidden(p) {
			return nil
		}
		// Containment: WalkDir starts inside the sandbox, but a symlinked
		// file within it could still point out, so re-check.
		if _, err := s.ResolveForRead(p); err != nil {
			return nil
		}
		scanned++

		matched, lines := grepFile(cctx, p, re, maxGrepHits-hits)
		if !matched {
			return nil
		}
		filesHit++
		for _, l := range lines {
			fmt.Fprintf(&out, "%s:%d: %s\n", s.Rel(p), l.no, l.text)
			hits++
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return "", walkErr
	}

	if hits == 0 {
		fmt.Fprintf(&out, "no matches (%d files scanned)\n", scanned)
	} else {
		fmt.Fprintf(&out, "%d hit(s) in %d file(s); %d files scanned", hits, filesHit, scanned)
		if hits >= maxGrepHits {
			fmt.Fprintf(&out, "; stopped at the %d-hit sample cap", maxGrepHits)
		}
		out.WriteString("\n")
	}
	return out.String(), nil
}

// GitState reports branch and dirty-file state. The argv is fixed and takes
// nothing from the model, and --porcelain has no history-rewriting behaviour.
func (s *Sandbox) GitState(ctx context.Context) (string, error) {
	if err := s.budget.Take(); err != nil {
		return "", err
	}
	if _, err := spawn.Lookup("git"); err != nil {
		return "git: not installed\n", nil
	}

	res, err := spawn.Run(ctx, "git",
		[]string{"-C", s.Root(), "status", "--porcelain=v1", "-b"},
		"", s.budget.PerCall())
	if err != nil || res.ExitCode != 0 {
		return fmt.Sprintf("%s is not a git repository\n", s.Rel(s.Root())), nil
	}

	lines := strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n")
	var b strings.Builder
	dirty := 0
	for i, l := range lines {
		if i == 0 && strings.HasPrefix(l, "##") {
			fmt.Fprintf(&b, "branch: %s\n", strings.TrimSpace(strings.TrimPrefix(l, "##")))
			continue
		}
		if l == "" {
			continue
		}
		// Porcelain v1 lines are "XY path"; a private path is omitted
		// entirely, as from the listing.
		if len(l) > 3 && s.isHidden(filepath.Join(s.Root(), strings.TrimSpace(l[3:]))) {
			continue
		}
		dirty++
		if dirty <= 20 {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}
	if dirty == 0 {
		b.WriteString("working tree clean\n")
	} else if dirty > 20 {
		fmt.Fprintf(&b, "(+%d more changed paths)\n", dirty-20)
	}
	return b.String(), nil
}
