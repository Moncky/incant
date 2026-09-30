// Package eval measures whether incant's commands actually work.
//
// Each case in cases.json is a request plus a fixture directory and an
// expected outcome: what the command prints, which files it removes, what it
// leaves in them. The suite asks the configured backend for a command, then
// runs it -- in a throwaway directory, with HOME pointed there too -- and
// grades the result.
//
// This is the one place incant's suggestions are ever executed, so it lives in
// a _test.go file, outside the binary and outside the architecture tests that
// forbid process execution. Two further guards apply: a command whose effect
// preview reaches outside the fixture, or cannot be previewed, is failed
// without being run; and the run has a short timeout.
//
// It calls a model for every case, which costs money, so it only runs when
// asked:
//
//	INCANT_EVAL=1 go test ./eval -run TestEval -v
//	INCANT_EVAL=1 go test ./eval -run TestEval/csv -v     # one case
//
// Backend and model come from ~/.config/incant/config as for the CLI; override
// with INCANT_EVAL_BACKEND (api|claude) and INCANT_EVAL_MODEL.
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/callumscott/incant/internal/backend"
	"github.com/callumscott/incant/internal/config"
	"github.com/callumscott/incant/internal/impact"
	"github.com/callumscott/incant/internal/probe"
	"github.com/callumscott/incant/internal/shellctx"
)

type evalCase struct {
	Name    string            `json:"name"`
	Note    string            `json:"note"`
	Request string            `json:"request"`
	Files   map[string]string `json:"files"`
	Mtimes  map[string]string `json:"mtimes"`
	Expect  expectation       `json:"expect"`
}

type expectation struct {
	StdoutLastNumber    *float64          `json:"stdout_last_number"`
	StdoutLines         []string          `json:"stdout_lines"`
	StdoutContains      []string          `json:"stdout_contains"`
	StdoutExcludes      []string          `json:"stdout_excludes"`
	StdoutDateOffsetDay *int              `json:"stdout_date_offset_days"`
	Gone                []string          `json:"gone"`
	Kept                []string          `json:"kept"`
	Contents            map[string]string `json:"contents"`
}

func loadCases(t *testing.T) []evalCase {
	t.Helper()
	data, err := os.ReadFile("cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []evalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("cases.json: %v", err)
	}
	return cases
}

// build creates the fixture directory.
func build(t *testing.T, c evalCase) string {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	for name, content := range c.Files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, age := range c.Mtimes {
		d, err := time.ParseDuration(age)
		if err != nil {
			t.Fatalf("%s: mtime %q: %v", c.Name, age, err)
		}
		mt := now.Add(-d)
		if err := os.Chtimes(filepath.Join(dir, name), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// execute runs cmd in dir after the preview gate. It returns stdout, or a
// reason the command was not run.
func execute(t *testing.T, shell, dir, cmd string) (string, error) {
	t.Helper()
	sb, err := probe.NewSandbox(dir, nil, probe.NewBudget(64, 2*time.Second, 5*time.Second))
	if err != nil {
		return "", err
	}
	rep := impact.Analyze(context.Background(), sb, cmd, impact.Options{
		Shell: shell, BSD: isBSD(), Home: dir,
	})
	if len(rep.Unknown) > 0 {
		return "", fmt.Errorf("not run: preview could not verify it: %s", strings.Join(rep.Unknown, "; "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run := exec.CommandContext(ctx, shell, "-c", cmd)
	run.Dir = dir
	run.Env = []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "TMPDIR=" + dir}
	var out, errb bytes.Buffer
	run.Stdout, run.Stderr = &out, &errb
	err = run.Run()
	if err != nil {
		return out.String(), fmt.Errorf("exit: %v; stderr: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func isBSD() bool {
	switch runtimeGOOS() {
	case "darwin", "freebsd", "openbsd", "netbsd":
		return true
	}
	return false
}

var numberRe = regexp.MustCompile(`-?\d+(\.\d+)?`)

// grade checks the outcome against the expectation, returning every miss.
func grade(e expectation, dir, stdout string) []string {
	var misses []string
	out := strings.TrimSpace(stdout)

	if e.StdoutLastNumber != nil {
		nums := numberRe.FindAllString(out, -1)
		if len(nums) == 0 {
			misses = append(misses, fmt.Sprintf("want number %v, stdout has none: %q", *e.StdoutLastNumber, out))
		} else if n, _ := strconv.ParseFloat(nums[len(nums)-1], 64); math.Abs(n-*e.StdoutLastNumber) > 1e-9 {
			misses = append(misses, fmt.Sprintf("want %v, got %v", *e.StdoutLastNumber, n))
		}
	}
	if e.StdoutLines != nil {
		var got []string
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				got = append(got, l)
			}
		}
		if strings.Join(got, "\n") != strings.Join(e.StdoutLines, "\n") {
			misses = append(misses, fmt.Sprintf("want lines %q, got %q", e.StdoutLines, got))
		}
	}
	for _, s := range e.StdoutContains {
		if !strings.Contains(out, s) {
			misses = append(misses, fmt.Sprintf("stdout lacks %q", s))
		}
	}
	for _, s := range e.StdoutExcludes {
		if strings.Contains(out, s) {
			misses = append(misses, fmt.Sprintf("stdout has %q", s))
		}
	}
	if e.StdoutDateOffsetDay != nil {
		want := time.Now().AddDate(0, 0, *e.StdoutDateOffsetDay).Format("2006-01-02")
		if out != want {
			misses = append(misses, fmt.Sprintf("want date %s, got %q", want, out))
		}
	}
	for _, f := range e.Gone {
		if exists(dir, f) {
			misses = append(misses, fmt.Sprintf("%q should be gone", f))
		}
	}
	for _, f := range e.Kept {
		if !exists(dir, f) {
			misses = append(misses, fmt.Sprintf("%q should exist", f))
		}
	}
	for f, want := range e.Contents {
		got, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			misses = append(misses, fmt.Sprintf("%q unreadable: %v", f, err))
		} else if string(got) != want {
			misses = append(misses, fmt.Sprintf("%q = %q, want %q", f, got, want))
		}
	}
	return misses
}

// exists checks for an exact name in its directory listing. A plain stat is
// not enough: on a case-insensitive filesystem (macOS by default) IMG.JPG
// still "exists" after it has been renamed to IMG.jpg.
func exists(dir, rel string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, filepath.Dir(rel)))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(rel) {
			return true
		}
	}
	return false
}

func evalShell() string {
	if p, err := exec.LookPath("zsh"); err == nil {
		return p
	}
	return "/bin/sh"
}

// selectBackend mirrors the CLI's choice, with environment overrides.
func selectBackend(t *testing.T, sb *probe.Sandbox) backend.Backend {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	name := cfg.Backend
	if v := os.Getenv("INCANT_EVAL_BACKEND"); v != "" {
		name = v
	}
	model := cfg.Model
	if v := os.Getenv("INCANT_EVAL_MODEL"); v != "" {
		model = v
	}
	if name == "claude" || (name == "auto" && backend.Credentials(cfg.APIKey) == "") {
		fb := backend.NewFallback()
		if !fb.Available() {
			t.Skip("no API credentials and no claude CLI")
		}
		return fb
	}
	return backend.NewAPI(backend.APIOptions{APIKey: cfg.APIKey, Model: model, Effort: cfg.Effort, Sandbox: sb})
}

// TestEval runs every case against a real model.
func TestEval(t *testing.T) {
	if os.Getenv("INCANT_EVAL") == "" {
		t.Skip("set INCANT_EVAL=1 to run the model eval (calls the API; costs money)")
	}
	shell := evalShell()
	passed, total := 0, 0
	var slowest time.Duration

	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			total++
			dir := build(t, c)
			sctx, err := shellctx.Collect(context.Background(), shellctx.Options{Cwd: dir, Shell: filepath.Base(shell)})
			if err != nil {
				t.Fatal(err)
			}
			sb, _ := probe.NewSandbox(dir, nil, probe.DefaultBudget())
			be := selectBackend(t, sb)

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			start := time.Now()
			got, err := be.Suggest(ctx, backend.Request{Query: c.Request, Context: sctx, N: 1})
			took := time.Since(start)
			slowest = max(slowest, took)
			if err != nil {
				t.Fatalf("%s: %v", be.Name(), err)
			}
			cmd := got[0].Command
			t.Logf("%-32s %6v  %d probe(s)  %s", c.Name, took.Round(time.Millisecond), got[0].Probes, cmd)

			stdout, err := execute(t, shell, dir, cmd)
			if err != nil {
				t.Fatalf("command failed: %v\n  command: %s", err, cmd)
			}
			if misses := grade(c.Expect, dir, stdout); len(misses) > 0 {
				t.Fatalf("wrong result:\n  command: %s\n  %s", cmd, strings.Join(misses, "\n  "))
			}
			passed++
		})
	}
	t.Logf("passed %d/%d; slowest answer %v", passed, total, slowest.Round(time.Millisecond))
}

// knownGood is a correct command for every case. TestHarness runs them to
// prove the fixtures and grader are right, with no model involved; a case
// with no entry here fails that test, so every case stays verified.
func knownGood(bsd bool) map[string]string {
	date := `date -d '3 days ago' +%F`
	sedInPlace := `sed -i 's/foo/bar/g' *.conf`
	if bsd {
		date = `date -v-3d +%F`
		sedInPlace = `sed -i '' 's/foo/bar/g' *.conf`
	}
	return map[string]string{
		"line-count-txt-among-mp3":     `cat -- *.txt | wc -l`,
		"pipe-delimited-log":           `awk -F'|' '$4 == 404' access.log | wc -l`,
		"semicolon-csv-sum-by-header":  `awk -F';' 'NR==1{for(i=1;i<=NF;i++) if($i=="amount") c=i; next} {s+=$c} END{print s}' sales.csv`,
		"unique-ips":                   `awk '{print $1}' access.log | sort -u | wc -l`,
		"tsv-second-column-unique":     `cut -f2 data.tsv | sort -u`,
		"delete-tmp-with-spaces":       `find . -type f -name '*.tmp' -delete`,
		"delete-tmp-with-newline-name": `find . -maxdepth 1 -type f -name '*.tmp' -delete`,
		"delete-old-logs":              `find . -name '*.log' -mtime +7 -delete`,
		"sed-in-place-bsd-trap":        sedInPlace,
		"rename-upper-jpg":             `for f in *.JPG; do mv -- "$f" "${f%.JPG}.jpg"; done`,
		"crlf-to-lf":                   `tr -d '\r' < data.txt > data.txt.tmp && mv data.txt.tmp data.txt`,
		"date-three-days-ago":          date,
		"largest-file":                 `ls -S | head -1`,
		"files-containing-todo":        `grep -rl TODO .`,
		"json-field":                   `sed -n 's/.*"name": *"\([^"]*\)".*/\1/p' package.json`,
		"modified-today":               `find . -type f -mtime -1`,
	}
}

// TestHarness proves the fixtures and grader with known-good commands, and
// that the preview gate refuses what it cannot verify. No model is called.
func TestHarness(t *testing.T) {
	shell := evalShell()
	good := knownGood(isBSD())
	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			cmd, ok := good[c.Name]
			if !ok {
				t.Fatalf("no known-good command for %s", c.Name)
			}
			dir := build(t, c)
			stdout, err := execute(t, shell, dir, cmd)
			if err != nil {
				// A known-good loop over "$f" is legitimately unpreviewable;
				// run it directly to check the fixture.
				if !strings.Contains(err.Error(), "not run") {
					t.Fatalf("known-good command failed: %v", err)
				}
				out, rerr := exec.Command(shell, "-c", "cd "+shellQuote(dir)+" && "+cmd).Output()
				if rerr != nil {
					t.Fatalf("known-good command failed: %v", rerr)
				}
				stdout = string(out)
			}
			if misses := grade(c.Expect, dir, stdout); len(misses) > 0 {
				t.Fatalf("known-good command graded wrong:\n  %s", strings.Join(misses, "\n  "))
			}
		})
	}

	// And a wrong answer must fail.
	c := loadCases(t)[0]
	dir := build(t, c)
	stdout, _ := execute(t, shell, dir, `wc -l < file_a.txt`)
	if len(grade(c.Expect, dir, stdout)) == 0 {
		t.Error("grader accepted a command that counts only one of the two files")
	}

	// The gate refuses anything reaching outside the fixture.
	if _, err := execute(t, shell, t.TempDir(), "rm -rf /tmp/incant-eval-canary"); err == nil || !strings.Contains(err.Error(), "not run") {
		t.Errorf("gate let an outside path through: %v", err)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
