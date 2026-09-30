package impact

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/callumscott/incant/internal/probe"
)

var testNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func write(t *testing.T, root, rel, content string, age time.Duration) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		mt := testNow.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
}

// fixture builds a small project tree:
//
//	a.log (old)  b.log (new)  notes.txt  data.txt  empty.dat
//	build/{x.o,y.o,sub/z.o}   node_modules/pkg/index.js
//	photos/ (holds a.jpg)     a.jpg  b.jpg   cache/{1.tmp,2.tmp}
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	day := 24 * time.Hour
	write(t, dir, "a.log", "old log\n", 10*day)
	write(t, dir, "b.log", "new log\n", 1*time.Hour)
	write(t, dir, "notes.txt", "alpha\n", 0)
	write(t, dir, "data.txt", "3\n1\n2\n", 0)
	write(t, dir, "empty.dat", "", 0)
	write(t, dir, "build/x.o", "12345", 0)
	write(t, dir, "build/y.o", "12345", 0)
	write(t, dir, "build/sub/z.o", "12345", 0)
	write(t, dir, "node_modules/pkg/index.js", "module.exports = 1\n", 0)
	write(t, dir, "photos/a.jpg", "old", 0)
	write(t, dir, "a.jpg", "new", 0)
	write(t, dir, "b.jpg", "new", 0)
	write(t, dir, "cache/1.tmp", "t", 0)
	write(t, dir, "cache/2.tmp", "t", 0)
	return dir
}

func analyze(t *testing.T, dir, cmd string, opt Options) *Report {
	t.Helper()
	sb, err := probe.NewSandbox(dir, nil, probe.NewBudget(64, 2*time.Second, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if opt.Now.IsZero() {
		opt.Now = testNow
	}
	if opt.Shell == "" {
		opt.Shell = "zsh"
	}
	return Analyze(context.Background(), sb, cmd, opt)
}

func TestPreviewLines(t *testing.T) {
	dir := fixture(t)
	outside := t.TempDir()

	cases := []struct {
		name string
		cmd  string
		opt  Options
		want string // exact line; "" means no preview
	}{
		{"read-only commands say nothing",
			"ls -la | grep log | wc -l", Options{}, ""},
		{"rm with a glob lists what it hits",
			"rm *.log", Options{}, "⚠ deletes 2 files (16B) — ./a.log, ./b.log"},
		{"rm -rf counts the whole tree",
			"rm -rf build", Options{}, "⚠ deletes 3 files and 2 dirs (15B) — ./build"},
		{"rm on a directory without -r deletes nothing",
			"rm build", Options{}, "⚠ deletes nothing"},
		{"a glob with no matches is called out",
			"rm -f *.bak", Options{}, "⚠ deletes nothing (no match for *.bak)"},
		{"find -mtime selects only old files",
			"find . -name '*.log' -mtime +7 -delete", Options{}, "⚠ deletes 1 file (8B) — ."},
		{"find -delete refuses non-empty directories",
			"find . -name node_modules -delete", Options{},
			"⚠ deletes nothing — -delete skips 1 non-empty dir"},
		{"find piped to xargs rm",
			"find . -name '*.tmp' -print0 | xargs -0 rm -f", Options{}, "⚠ deletes 2 files (2B) — ./cache"},
		{"find -exec rm",
			`find cache -type f -exec rm {} \;`, Options{}, "⚠ deletes 2 files (2B) — ./cache"},
		{"find -size -1M matches only empty files",
			"find . -type f -size -1M -delete", Options{}, "⚠ deletes 1 file — ."},
		{"variables are unpreviewable, not silently ignored",
			`for f in *.log; do rm "$f"; done`, Options{}, `⚠ can't preview rm "$f" (variable)`},
		{"paths outside the cwd are unpreviewable",
			"rm -rf " + outside, Options{}, "⚠ can't preview rm " + outside + " (outside the current directory)"},
		{"tilde expands to a home outside the cwd",
			"rm -rf ~/Downloads", Options{Home: outside},
			"⚠ can't preview rm ~/Downloads (outside the current directory)"},
		{"redirect onto an input file empties it first",
			"sort data.txt > data.txt", Options{},
			"⚠ ./data.txt is emptied by > before sort reads it; overwrites 1 file (6B) — ./data.txt"},
		{"plain clobbering redirect",
			"echo hi > notes.txt", Options{}, "⚠ overwrites 1 file (6B) — ./notes.txt"},
		{"redirect to a new file or /dev/null is fine",
			"make 2>/dev/null > new.log 2>&1", Options{}, ""},
		{"GNU sed -i",
			"sed -i 's/a/b/' *.txt", Options{}, "⚠ edits in place 2 files (12B) — ./data.txt, ./notes.txt"},
		{"BSD sed -i ''",
			"sed -i '' -e 's/a/b/' notes.txt", Options{BSD: true}, "⚠ edits in place 1 file (6B) — ./notes.txt"},
		{"BSD sed -i with the script where the suffix belongs",
			"sed -i 's/a/b/' notes.txt", Options{BSD: true},
			"⚠ BSD sed takes 's/a/b/' as the -i backup suffix, not the script; it edits nothing (use sed -i '' ...)"},
		{"sed without -i changes nothing",
			"sed 's/a/b/' notes.txt", Options{}, ""},
		{"perl -pi -e",
			"perl -pi -e 's/a/b/' notes.txt", Options{}, "⚠ edits in place 1 file (6B) — ./notes.txt"},
		{"mv into a directory flags the clobber",
			"mv *.jpg photos/", Options{},
			"⚠ overwrites 1 file (3B) — ./photos/a.jpg; moves 2 files (6B) to ./photos"},
		{"mv -n never clobbers",
			"mv -n *.jpg photos/", Options{}, "⚠ moves 2 files (6B) to ./photos"},
		{"cd scopes later relative paths",
			"cd build && rm *.o", Options{}, "⚠ deletes 2 files (10B) — ./build/x.o, ./build/y.o"},
		{"a subshell's cd does not leak",
			"(cd build && rm x.o); rm *.log", Options{}, "⚠ deletes 3 files (21B) — ./build/x.o, ./a.log, ./b.log"},
		{"zsh ** recurses",
			"rm **/*.o", Options{}, "⚠ deletes 3 files (15B) — ./build/sub/z.o, ./build/x.o, ./build/y.o"},
		{"sudo is looked through",
			"sudo -u root rm -f a.log", Options{}, "⚠ deletes 1 file (8B) — ./a.log"},
		{"chmod -R counts the tree",
			"chmod -R 755 build", Options{}, "⚠ changes permissions on 3 files and 2 dirs (15B) — ./build"},
		{"xargs fed by something other than find",
			"ls | xargs rm", Options{}, "⚠ can't preview xargs rm (input comes from ls)"},
		{"find -exec sh -c is opaque",
			`find . -name '*.log' -exec sh -c 'rm "$1"' _ {} \;`, Options{}, "⚠ can't preview find -exec sh (runs a shell script)"},
		{"brace expansion is not guessed at",
			"rm notes.{txt,bak}", Options{}, "⚠ can't preview rm notes.{txt,bak} (brace expansion)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := analyze(t, dir, c.cmd, c.opt).String()
			if got != c.want {
				t.Errorf("%s\n got: %s\nwant: %s", c.cmd, got, c.want)
			}
		})
	}

	// None of the above may have touched the fixture.
	if _, err := os.Stat(filepath.Join(dir, "build", "sub", "z.o")); err != nil {
		t.Fatalf("fixture was modified: %v", err)
	}
}

func TestBSDMtimeRoundsUp(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "f.log", "x", 30*time.Hour) // 1.25 days old
	gnu := analyze(t, dir, "find . -name f.log -mtime 1 -delete", Options{}).String()
	bsd := analyze(t, dir, "find . -name f.log -mtime 1 -delete", Options{BSD: true}).String()
	if !strings.Contains(gnu, "deletes 1 file") {
		t.Errorf("GNU: 30h is -mtime 1 (floor), got %q", gnu)
	}
	if !strings.Contains(bsd, "deletes nothing") {
		t.Errorf("BSD: 30h is -mtime 2 (ceil), got %q", bsd)
	}
}

func TestGitDestructiveSubcommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	write(t, dir, "tracked.go", "package a\n", 0)
	write(t, dir, "other.go", "package a\n", 0)
	git("add", ".")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")
	write(t, dir, "tracked.go", "package a // edited\n", 0)
	write(t, dir, "scratch.txt", "untracked\n", 0)
	write(t, dir, "tmp/junk.txt", "untracked\n", 0)

	cases := map[string]string{
		"git reset --hard":       "⚠ discards uncommitted changes to 1 file (20B) — ./tracked.go",
		"git checkout -- .":      "⚠ discards uncommitted changes to 1 file (20B) — ./tracked.go",
		"git restore other.go":   "⚠ discards uncommitted changes to nothing",
		"git restore --staged .": "",
		"git clean -f":           "⚠ deletes 1 file (10B) — ./scratch.txt",
		"git clean -fd":          "⚠ deletes 2 files and 1 dir (20B) — ./tmp (2), ./scratch.txt",
		"git clean -n -d":        "",
		"git status && git diff": "",
	}
	for cmd, want := range cases {
		if got := analyze(t, dir, cmd, Options{}).String(); got != want {
			t.Errorf("%s\n got: %s\nwant: %s", cmd, got, want)
		}
	}
}

func TestLexer(t *testing.T) {
	items, err := parse(`FOO=1 rm -f "my file.txt" 'a*b' c\ d *.log 2>&1 >out.txt | tee -a x; echo $(ls | wc -l) done`)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	rm := items[0].pipeline[0]
	var lits, pats []string
	for _, w := range rm.words {
		lits = append(lits, w.lit)
		pats = append(pats, w.pat)
	}
	if got := strings.Join(lits, "|"); got != "FOO=1|rm|-f|my file.txt|a*b|c d|*.log" {
		t.Errorf("lits = %s", got)
	}
	if pats[4] != `a\*b` || pats[6] != "*.log" {
		t.Errorf("quoted glob must be escaped, unquoted kept: %q %q", pats[4], pats[6])
	}
	if rm.words[4].glob || !rm.words[6].glob {
		t.Error("glob flag wrong")
	}
	if len(rm.redirs) != 2 || rm.redirs[0].op != ">&" || rm.redirs[1].target.lit != "out.txt" {
		t.Errorf("redirs = %+v", rm.redirs)
	}
	echo := items[1].pipeline[0]
	if len(echo.words) != 3 || echo.words[1].dynamic != "command substitution" {
		t.Errorf("command substitution must stay one dynamic word: %+v", echo.words)
	}
}
