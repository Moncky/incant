package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func matchNames(t *testing.T, s *Sandbox, pattern string, opt MatchOptions) []string {
	t.Helper()
	got, _, err := s.Match(context.Background(), pattern, opt)
	if err != nil {
		t.Fatalf("Match(%q): %v", pattern, err)
	}
	var names []string
	for _, e := range got {
		names = append(names, s.Rel(e.Real))
	}
	return names
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMatchShellSemantics(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSandbox(dir, nil, NewBudget(64, time.Second, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.log", "b.log", ".hidden.log", "c.txt", "logs/x.log", "logs/deep/y.log", ".git/z.log", "star*.log"} {
		mustWrite(t, filepath.Join(dir, f), "x")
	}

	cases := []struct {
		pattern string
		opt     MatchOptions
		want    []string
	}{
		{"*.log", MatchOptions{}, []string{"./a.log", "./b.log", "./star*.log"}},
		{".*.log", MatchOptions{}, []string{"./.hidden.log"}},
		{"logs/*.log", MatchOptions{}, []string{"./logs/x.log"}},
		{"**/*.log", MatchOptions{Globstar: true}, []string{"./a.log", "./b.log", "./logs/deep/y.log", "./logs/x.log", "./star*.log"}},
		{`star\*.log`, MatchOptions{}, []string{"./star*.log"}},
		{"*/", MatchOptions{}, []string{"./logs"}},
		{"nope/*.log", MatchOptions{}, nil},
		{"missing.txt", MatchOptions{}, nil},
		{"c.txt", MatchOptions{}, []string{"./c.txt"}},
	}
	for _, c := range cases {
		if got := matchNames(t, s, c.pattern, c.opt); !equal(got, c.want) {
			t.Errorf("Match(%q) = %v, want %v", c.pattern, got, c.want)
		}
	}
}

func TestMatchAndStatStayInSandbox(t *testing.T) {
	s, dir := newTestSandbox(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.log"), "x")

	if _, _, err := s.Match(context.Background(), filepath.Join(outside, "*.log"), MatchOptions{}); !errors.Is(err, ErrEscape) {
		t.Errorf("absolute pattern outside root: err = %v, want ErrEscape", err)
	}
	if _, _, err := s.Match(context.Background(), "../*", MatchOptions{}); !errors.Is(err, ErrEscape) {
		t.Errorf("../* : err = %v, want ErrEscape", err)
	}

	// A symlinked directory pointing out must be refused, not reported as
	// empty: the shell would expand it, so "no matches" would be a false
	// all-clear. That holds whether the link is the literal prefix or is
	// crossed by a wildcard.
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	for _, pat := range []string{"out/*.log", "o*/*.log"} {
		if _, _, err := s.Match(context.Background(), pat, MatchOptions{}); !errors.Is(err, ErrEscape) {
			t.Errorf("Match(%q) through escaping symlink: err = %v, want ErrEscape", pat, err)
		}
	}
	// ...but the link itself is local and describable, since rm acts on it.
	e, err := s.Stat(context.Background(), "out")
	if err != nil {
		t.Fatalf("Stat(link): %v", err)
	}
	if !e.IsLink() {
		t.Errorf("Stat followed the link: mode %v", e.Mode)
	}
}

func TestWalkCountsAndTruncates(t *testing.T) {
	s, dir := newTestSandbox(t)
	for _, f := range []string{"t/a", "t/b", "t/sub/c", "t/sub/d"} {
		mustWrite(t, filepath.Join(dir, f), "12345")
	}
	root, err := s.Stat(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}

	var files, dirs int
	var bytes int64
	truncated, err := s.Walk(context.Background(), root.Real, -1, 0, func(e Entry, depth int) error {
		if e.IsDir() {
			dirs++
		} else {
			files++
			bytes += e.Size
		}
		return nil
	})
	if err != nil || truncated {
		t.Fatalf("Walk: truncated=%v err=%v", truncated, err)
	}
	if files != 4 || dirs != 2 || bytes != 20 {
		t.Errorf("files=%d dirs=%d bytes=%d, want 4, 2, 20", files, dirs, bytes)
	}

	n := 0
	truncated, err = s.Walk(context.Background(), root.Real, -1, 3, func(Entry, int) error { n++; return nil })
	if err != nil || !truncated || n != 3 {
		t.Errorf("limit 3: visited %d, truncated=%v, err=%v", n, truncated, err)
	}

	n = 0
	if _, err := s.Walk(context.Background(), root.Real, 1, 0, func(Entry, int) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 4 { // t, a, b, sub
		t.Errorf("maxDepth 1 visited %d, want 4", n)
	}
}

func TestRerootGitPath(t *testing.T) {
	cases := []struct{ p, prefix, want string }{
		{"a/b.go", "", "a/b.go"},
		{"sub/x.go", "sub/", "x.go"},
		{"other/y.go", "sub/", "../other/y.go"},
		{"top.go", "a/b/", "../../top.go"},
	}
	for _, c := range cases {
		if got := rerootGitPath(c.p, c.prefix); got != c.want {
			t.Errorf("rerootGitPath(%q, %q) = %q, want %q", c.p, c.prefix, got, c.want)
		}
	}
}
