// Package arch holds structural tests: invariants about the shape of the
// codebase that no single package can assert about itself.
package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod")
		}
		dir = parent
	}
}

// goFiles yields every non-test .go file in the module, keyed by its
// slash-separated path relative to the root.
func goFiles(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("found no Go source files; the walk is wrong")
	}
	return out
}

// execCapableImports are packages that can start a process or replace this
// one.
var execCapableImports = map[string]bool{
	"os/exec": true,
	"syscall": true,
}

// TestOnlyProbeMayExecute is the load-bearing structural invariant.
//
// incant generates shell commands. If any package could start a process, a bug
// or a prompt injection could turn a suggestion into an execution. Confining
// the capability to internal/spawn -- one file, with two allowlists naming
// every binary that can be started -- is what makes "incant never runs what it
// suggests" a property of the code rather than a promise in the README.
//
// Callers that legitimately need a subprocess (internal/probe for git state,
// internal/backend for the claude fallback) go through spawn's narrow API
// instead of holding the capability themselves.
func TestOnlyProbeMayExecute(t *testing.T) {
	const sanctioned = "internal/spawn/"

	for rel, abs := range goFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, abs, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", rel, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !execCapableImports[path] {
				continue
			}
			if strings.HasPrefix(rel, sanctioned) {
				continue
			}
			t.Errorf("%s imports %q; process execution belongs only in %s (use its Run/RunVersion/Lookup API)",
				rel, path, sanctioned)
		}
	}
}

// TestNoShellInvocation bans handing a command string to a shell anywhere,
// including inside internal/probe.
//
// Every probe is a Go function operating on typed arguments. The moment any of
// them builds a string and passes it to `sh -c`, the model gains an arbitrary
// execution path and every quoting rule in the sandbox stops mattering.
//
// The check looks at call-argument positions specifically, not at every string
// literal: a shell's *name* is legitimate data (a $SHELL fallback, an entry in
// the version allowlist), while a shell name or `-c` passed as an argument is
// an invocation. `-c` is the strongest signal, since it catches
// exec.Command(someVar, "-c", cmd) where the shell itself is a variable.
func TestNoShellInvocation(t *testing.T) {
	banned := map[string]bool{
		`"-c"`: true, `"sh"`: true, `"bash"`: true, `"zsh"`: true,
		`"/bin/sh"`: true, `"/bin/bash"`: true, `"/bin/zsh"`: true,
		`"-ic"`: true, `"--command"`: true,
	}

	for rel, abs := range goFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, abs, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", rel, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !banned[lit.Value] {
					continue
				}
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: %s passed as a call argument suggests shell invocation; probes must be Go functions, not command strings",
					rel, pos.Line, lit.Value)
			}
			return true
		})
	}
}
