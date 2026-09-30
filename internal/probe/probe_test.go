package probe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// IDEA.md's motivating scenario: two .txt files in a directory otherwise full
// of .mp3. The listing must make both the two text files and the shape of the
// noise around them obvious.
func TestListDirIdeaScenario(t *testing.T) {
	s, dir := newTestSandbox(t)
	mustWrite(t, filepath.Join(dir, "file_a.txt"), "one\ntwo\nthree\n")
	mustWrite(t, filepath.Join(dir, "file_b.txt"), "four\nfive\n")
	for i := 0; i < 12; i++ {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("file_z%02d.mp3", i)), "ID3")
	}

	got, err := s.ListDir(context.Background(), ".", 1)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	for _, want := range []string{"file_a.txt", "file_b.txt", ".mp3=12", ".txt=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing missing %q:\n%s", want, got)
		}
	}
}

func TestListDirTruncatesAndRespectsDepth(t *testing.T) {
	s, dir := newTestSandbox(t)
	for i := 0; i < maxListEntries+50; i++ {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("f%04d.dat", i)), "x")
	}
	got, err := s.ListDir(context.Background(), ".", 1)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation notice:\n%s", got[:min(len(got), 300)])
	}

	// Depth must be clamped: a level-3 file stays hidden even when asked for.
	s2, dir2 := newTestSandbox(t)
	mustWrite(t, filepath.Join(dir2, "a", "b", "c", "deep.txt"), "deep\n")
	got2, err := s2.ListDir(context.Background(), ".", 99)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if strings.Contains(got2, "deep.txt") {
		t.Errorf("depth clamp failed, level-3 file listed:\n%s", got2)
	}
}

func TestListDirRejectsNonDirectory(t *testing.T) {
	s, dir := newTestSandbox(t)
	mustWrite(t, filepath.Join(dir, "f.txt"), "x\n")
	if _, err := s.ListDir(context.Background(), "f.txt", 1); err == nil {
		t.Error("ListDir on a file should fail")
	}
}

// The recon fixture the plan names as incant's differentiator: a log whose
// delimiter and field order are not what a model would assume. PeekFile must
// surface enough for the field positions to be read off directly.
func TestPeekFileRevealsRealFieldLayout(t *testing.T) {
	s, dir := newTestSandbox(t)
	// Pipe-delimited, timestamp in field 3 rather than field 1, status last.
	mustWrite(t, filepath.Join(dir, "access.log"),
		"alpha|/index.html|2026-09-30T10:00:01Z|200\n"+
			"beta|/api/v1/users|2026-09-30T10:00:02Z|503\n"+
			"gamma|/health|2026-09-30T10:00:03Z|200\n")

	got, err := s.PeekFile(context.Background(), "access.log", 3, 0)
	if err != nil {
		t.Fatalf("PeekFile: %v", err)
	}
	if !strings.Contains(got, "alpha|/index.html|") {
		t.Errorf("peek did not reveal the pipe delimiter:\n%s", got)
	}
	if !strings.Contains(got, "1: ") || !strings.Contains(got, "3: ") {
		t.Errorf("peek should number lines so field positions are readable:\n%s", got)
	}
}

func TestPeekFileHeadAndTail(t *testing.T) {
	s, dir := newTestSandbox(t)
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "line-%03d\n", i)
	}
	mustWrite(t, filepath.Join(dir, "big.txt"), b.String())

	got, err := s.PeekFile(context.Background(), "big.txt", 2, 2)
	if err != nil {
		t.Fatalf("PeekFile: %v", err)
	}
	for _, want := range []string{"line-001", "line-002", "tail: line-099", "tail: line-100"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "line-050") {
		t.Errorf("head+tail should not include the middle:\n%s", got)
	}
}

func TestPeekFileRejectsBinaryAndSecrets(t *testing.T) {
	s, dir := newTestSandbox(t)
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, ".env"), "API_KEY=sk-live-abc123\n")

	if _, err := s.PeekFile(context.Background(), "a.bin", 5, 0); !errors.Is(err, ErrBinary) {
		t.Errorf("PeekFile(binary) = %v, want ErrBinary", err)
	}
	_, err := s.PeekFile(context.Background(), ".env", 5, 0)
	if !errors.Is(err, ErrSecret) {
		t.Fatalf("PeekFile(.env) = %v, want ErrSecret", err)
	}
	if strings.Contains(err.Error(), "sk-live") {
		t.Errorf("error text leaked file contents: %v", err)
	}
}

func TestPeekFileClipsPathologicalLine(t *testing.T) {
	s, dir := newTestSandbox(t)
	mustWrite(t, filepath.Join(dir, "min.js"), strings.Repeat("a", 5000)+"\n")
	got, err := s.PeekFile(context.Background(), "min.js", 1, 0)
	if err != nil {
		t.Fatalf("PeekFile: %v", err)
	}
	if !strings.Contains(got, "(clipped)") {
		t.Error("expected a long line to be clipped")
	}
	if len(got) > maxLineLen+200 {
		t.Errorf("clipped output still too large: %d bytes", len(got))
	}
}

// FileInfo reports metadata for a secret path but never its contents: the
// model may legitimately need to know a .env exists and how big it is.
func TestFileInfoWithholdsSecretContents(t *testing.T) {
	s, dir := newTestSandbox(t)
	mustWrite(t, filepath.Join(dir, ".env"), "API_KEY=sk-live-abc123\nDB=postgres\n")
	mustWrite(t, filepath.Join(dir, "notes.txt"), "a\nb\nc\n")

	secret, err := s.FileInfo(context.Background(), ".env")
	if err != nil {
		t.Fatalf("FileInfo(.env): %v", err)
	}
	if !strings.Contains(secret, "withheld") {
		t.Errorf("expected withheld notice:\n%s", secret)
	}
	for _, leak := range []string{"sk-live", "postgres", "2 lines"} {
		if strings.Contains(secret, leak) {
			t.Errorf("FileInfo leaked %q for a secret path:\n%s", leak, secret)
		}
	}

	ordinary, err := s.FileInfo(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("FileInfo(notes.txt): %v", err)
	}
	if !strings.Contains(ordinary, "3 lines") {
		t.Errorf("expected a line count:\n%s", ordinary)
	}
}

func TestWhichTool(t *testing.T) {
	s, _ := newTestSandbox(t)
	ctx := context.Background()

	// git is allowlisted and present in this environment, so a version shows.
	got, err := s.WhichTool(ctx, "git")
	if err != nil {
		t.Fatalf("WhichTool(git): %v", err)
	}
	if !strings.Contains(got, "installed at") || !strings.Contains(got, "git version") {
		t.Errorf("WhichTool(git) = %q, want path and version", got)
	}

	if got, err := s.WhichTool(ctx, "definitely-not-a-real-tool-xyz"); err != nil {
		t.Fatalf("WhichTool(absent): %v", err)
	} else if !strings.Contains(got, "not installed") {
		t.Errorf("WhichTool(absent) = %q", got)
	}
}

// The version probe executes a binary, so anything that is not a bare command
// name on the allowlist must be refused or answered without execution.
func TestWhichToolRefusesUnsafeNames(t *testing.T) {
	s, _ := newTestSandbox(t)
	ctx := context.Background()

	for _, bad := range []string{
		"git; rm -rf /", "git && whoami", "/bin/sh", "../../bin/sh",
		"$(whoami)", "git|cat", "", "  ", "a`b`",
	} {
		if _, err := s.WhichTool(ctx, bad); err == nil {
			t.Errorf("WhichTool(%q) succeeded; want rejection", bad)
		}
	}
}

func TestWhichToolDoesNotExecuteUnlistedTool(t *testing.T) {
	s, _ := newTestSandbox(t)

	// A tool that exists but is not allowlisted must be reported present
	// without its --version being run.
	marker := filepath.Join(t.TempDir(), "incant-probe-ran")
	script := filepath.Join(t.TempDir(), "notallowed")
	mustWrite(t, script, "#!/bin/sh\ntouch "+marker+"\necho ran\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(script)+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := s.WhichTool(context.Background(), "notallowed")
	if err != nil {
		t.Fatalf("WhichTool: %v", err)
	}
	if !strings.Contains(got, "version not probed") {
		t.Errorf("WhichTool(unlisted) = %q, want a not-probed answer", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an unlisted tool was executed; the version allowlist is not holding")
	}
}

func TestGrepSample(t *testing.T) {
	s, dir := newTestSandbox(t)
	mustWrite(t, filepath.Join(dir, "app.log"), "ok\nERROR db down\nok\nERROR timeout\n")
	mustWrite(t, filepath.Join(dir, "sub", "other.log"), "ERROR disk\n")
	ctx := context.Background()

	got, err := s.GrepSample(ctx, `^ERROR`, ".")
	if err != nil {
		t.Fatalf("GrepSample: %v", err)
	}
	if !strings.Contains(got, "ERROR db down") || !strings.Contains(got, "hit(s)") {
		t.Errorf("expected hits:\n%s", got)
	}
	if !strings.Contains(got, "./app.log:2") {
		t.Errorf("expected relative path and line number:\n%s", got)
	}

	none, err := s.GrepSample(ctx, `zzz-no-match-zzz`, ".")
	if err != nil {
		t.Fatalf("GrepSample: %v", err)
	}
	if !strings.Contains(none, "no matches") {
		t.Errorf("expected a no-match answer:\n%s", none)
	}

	if _, err := s.GrepSample(ctx, `[unclosed`, "."); err == nil {
		t.Error("invalid regex should error")
	}
}

func TestGrepSampleCapsHitsAndSkipsSecrets(t *testing.T) {
	s, dir := newTestSandbox(t)
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("NEEDLE\n")
	}
	mustWrite(t, filepath.Join(dir, "many.log"), b.String())
	mustWrite(t, filepath.Join(dir, ".env"), "NEEDLE=sk-live-abc123\n")

	got, err := s.GrepSample(context.Background(), "NEEDLE", ".")
	if err != nil {
		t.Fatalf("GrepSample: %v", err)
	}
	if strings.Count(got, "many.log:") > maxGrepHits {
		t.Errorf("hit cap exceeded:\n%s", got)
	}
	if strings.Contains(got, ".env") || strings.Contains(got, "sk-live") {
		t.Errorf("grep sampled a secret file:\n%s", got)
	}
}

func TestGitState(t *testing.T) {
	s, _ := newTestSandbox(t)
	got, err := s.GitState(context.Background())
	if err != nil {
		t.Fatalf("GitState: %v", err)
	}
	if !strings.Contains(got, "not a git repository") {
		t.Errorf("GitState on a bare temp dir = %q", got)
	}
}

// Every probe must draw from the shared budget, so a runaway loop stops
// instead of stalling the user's prompt.
func TestProbesConsumeBudget(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "f.txt"), "a\n")
	s, err := NewSandbox(dir, nil, NewBudget(2, time.Second, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := s.ListDir(ctx, ".", 1); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, err := s.PeekFile(ctx, "f.txt", 1, 0); err != nil {
		t.Fatalf("call 2: %v", err)
	}

	// Budget is spent; every probe must now refuse.
	checks := map[string]error{
		"ListDir":    firstErr(s.ListDir(ctx, ".", 1)),
		"PeekFile":   firstErr(s.PeekFile(ctx, "f.txt", 1, 0)),
		"FileInfo":   firstErr(s.FileInfo(ctx, "f.txt")),
		"WhichTool":  firstErr(s.WhichTool(ctx, "git")),
		"GrepSample": firstErr(s.GrepSample(ctx, "a", ".")),
		"GitState":   firstErr(s.GitState(ctx)),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrBudget) {
			t.Errorf("%s past budget = %v, want ErrBudget", name, err)
		}
	}
}

func firstErr(_ string, err error) error { return err }
