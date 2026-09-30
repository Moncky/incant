package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadParsesAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte(`# incant settings
backend = api
model = "claude-haiku-4-5"
effort = medium
context = minimal
preview = false
candidates = 2
mystery = 1
`), 0o600)

	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != "api" || c.Model != "claude-haiku-4-5" || c.Effort != "medium" ||
		c.Context != ContextMinimal || c.Preview || c.Candidates != 2 {
		t.Errorf("parsed %+v", c)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "mystery") {
		t.Errorf("warnings = %v", c.Warnings)
	}

	os.WriteFile(path, []byte("effort = extreme\n"), 0o600)
	if _, err := LoadFrom(path); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Errorf("bad effort: err = %v, want a line-numbered error", err)
	}
}

func TestMissingConfigIsDefault(t *testing.T) {
	c, err := LoadFrom(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != DefaultModel || !c.Preview || c.Context != ContextFull {
		t.Errorf("defaults = %+v", c)
	}
}

func TestWorldReadableKeyWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte("api_key = sk-ant-test\n"), 0o644)
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) == 0 || !strings.Contains(c.Warnings[0], "chmod 600") {
		t.Errorf("no permission warning: %v", c.Warnings)
	}
}

func TestPrivacyPatterns(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "proj", "sub"), 0o755)
	os.WriteFile(filepath.Join(root, IgnoreFile), []byte("# outer\n*.key\n"), 0o644)
	os.WriteFile(filepath.Join(root, "proj", IgnoreFile), []byte("customers/\nsub/dump.sql\n"), 0o644)

	p, err := LoadPrivacy(filepath.Join(root, "proj", "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 || p.Everything {
		t.Fatalf("files=%v everything=%v", p.Files, p.Everything)
	}
	proj := filepath.Join(root, "proj")
	cases := map[string]bool{
		filepath.Join(proj, "customers"):               true,
		filepath.Join(proj, "a", "customers", "x.csv"): true,
		filepath.Join(proj, "sub", "dump.sql"):         true,
		filepath.Join(proj, "other", "dump.sql"):       false,
		filepath.Join(proj, "deep", "server.key"):      true,
		filepath.Join(proj, "main.go"):                 false,
	}
	for path, want := range cases {
		if got := p.Hidden(path); got != want {
			t.Errorf("Hidden(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestEmptyIgnoreFileHidesEverything(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, IgnoreFile), nil, 0o644)
	p, err := LoadPrivacy(root)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Everything || !p.Hidden(filepath.Join(root, "anything")) {
		t.Error("an empty .incantignore must make the whole tree private")
	}
}
