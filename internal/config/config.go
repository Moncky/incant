// Package config loads incant's settings and per-directory privacy rules.
//
// Settings live in one small key = value file, $XDG_CONFIG_HOME/incant/config
// (default ~/.config/incant/config). It is deliberately not TOML or YAML: the
// file has a handful of flat keys, and a parser dependency would be the
// largest thing in the module.
//
// Privacy rules live next to the code they protect, in .incantignore files. A
// .incantignore in the cwd or any ancestor applies. Each non-comment line is a
// glob naming paths the model must never see -- not in the directory listing,
// not through any probe. An empty file, or a line that is just *, keeps the
// whole tree private: no listing, no git state, no probes.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Context levels: how much of the session is sent with each request.
const (
	ContextFull    = "full"    // listing, git state, last command, and probes
	ContextMinimal = "minimal" // shell/OS/tool facts and the last command only
	ContextNone    = "none"    // shell/OS/tool facts only
)

// Config is the parsed settings file.
type Config struct {
	Path string // where it was read from, or would be

	Backend string // auto, api, claude
	Model   string
	Effort  string
	APIKey  string
	Context string
	Preview bool
	// Candidates is how many alternates the widget asks for, to cycle
	// through on repeat presses.
	Candidates int

	// Warnings are non-fatal problems worth surfacing in `incant doctor`.
	Warnings []string
}

// DefaultModel is the model the API backend uses when config names none.
const DefaultModel = "claude-opus-5-5"

// Default returns the settings used when no file exists.
func Default() *Config {
	return &Config{
		Backend:    "auto",
		Model:      DefaultModel,
		Effort:     "low",
		Context:    ContextFull,
		Preview:    true,
		Candidates: 3,
	}
}

// Dir is incant's config directory.
func Dir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "incant")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "incant")
}

// Load reads the settings file. A missing file is not an error.
func Load() (*Config, error) {
	c := Default()
	dir := Dir()
	if dir == "" {
		return c, nil
	}
	c.Path = filepath.Join(dir, "config")
	return c, c.read()
}

// LoadFrom reads settings from an explicit path.
func LoadFrom(path string) (*Config, error) {
	c := Default()
	c.Path = path
	return c, c.read()
}

func (c *Config) read() error {
	f, err := os.Open(c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected key = value", c.Path, n)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if err := c.set(k, v); err != nil {
			return fmt.Errorf("%s:%d: %w", c.Path, n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	// A key in a world-readable file is a key anyone on the machine has.
	if c.APIKey != "" {
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"%s holds an api_key but is readable by others (mode %s); run: chmod 600 %s",
				c.Path, fi.Mode().Perm(), c.Path))
		}
	}
	return nil
}

func (c *Config) set(k, v string) error {
	switch k {
	case "backend":
		switch v {
		case "auto", "api", "claude":
		default:
			return fmt.Errorf("backend must be auto, api or claude, not %q", v)
		}
		c.Backend = v
	case "model":
		c.Model = v
	case "effort":
		switch v {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("effort must be low, medium, high, xhigh or max, not %q", v)
		}
		c.Effort = v
	case "api_key":
		c.APIKey = v
	case "context":
		switch v {
		case ContextFull, ContextMinimal, ContextNone:
		default:
			return fmt.Errorf("context must be full, minimal or none, not %q", v)
		}
		c.Context = v
	case "preview":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("preview must be true or false, not %q", v)
		}
		c.Preview = b
	case "candidates":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5 {
			return fmt.Errorf("candidates must be 1 to 5, not %q", v)
		}
		c.Candidates = n
	default:
		c.Warnings = append(c.Warnings, fmt.Sprintf("%s: unknown key %q ignored", c.Path, k))
	}
	return nil
}

// Privacy is the merged effect of every .incantignore above a directory.
type Privacy struct {
	// Files are the .incantignore files that apply, nearest first.
	Files []string
	// Everything means the whole tree is private.
	Everything bool
	// patterns are globs, each anchored at the directory holding its file.
	patterns []pattern
}

type pattern struct {
	base string // absolute directory of the .incantignore
	glob string
}

// IgnoreFile is the per-directory privacy file name.
const IgnoreFile = ".incantignore"

// LoadPrivacy collects .incantignore files from dir up to the filesystem root.
func LoadPrivacy(dir string) (*Privacy, error) {
	p := &Privacy{}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for d := abs; ; d = filepath.Dir(d) {
		file := filepath.Join(d, IgnoreFile)
		data, err := os.ReadFile(file)
		if err == nil {
			p.Files = append(p.Files, file)
			lines := 0
			for _, raw := range strings.Split(string(data), "\n") {
				l := strings.TrimSpace(raw)
				if l == "" || strings.HasPrefix(l, "#") {
					continue
				}
				lines++
				if l == "*" || l == "/" {
					p.Everything = true
				}
				p.patterns = append(p.patterns, pattern{base: d, glob: strings.TrimPrefix(strings.TrimSuffix(l, "/"), "/")})
			}
			if lines == 0 {
				p.Everything = true
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return p, nil
}

// Hidden reports whether an absolute path is private.
//
// A pattern without a slash matches a name at any depth below its file, as in
// .gitignore; one with a slash is matched against the path relative to the
// file's directory. Anything beneath a hidden directory is hidden.
func (p *Privacy) Hidden(abs string) bool {
	if p == nil {
		return false
	}
	if p.Everything {
		return true
	}
	for _, pt := range p.patterns {
		rel, err := filepath.Rel(pt.base, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		rel = filepath.ToSlash(rel)
		parts := strings.Split(rel, "/")
		if !strings.Contains(pt.glob, "/") {
			for _, part := range parts {
				if ok, _ := filepath.Match(pt.glob, part); ok {
					return true
				}
			}
			continue
		}
		for i := range parts {
			if ok, _ := filepath.Match(pt.glob, strings.Join(parts[:i+1], "/")); ok {
				return true
			}
		}
	}
	return false
}
