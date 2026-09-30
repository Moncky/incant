package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/backend"
	"github.com/callumscott/incant/internal/config"
	"github.com/callumscott/incant/internal/shellctx"
)

// runDoctor checks the installation and says exactly what a request from the
// current directory would send. Most "it doesn't work" reports are setup: no
// credentials, no hook in the rc file, a bash too old for the keybindings.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	ping := fs.Bool("ping", false, "make one real request and time it (uses a few tokens)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ok, warn, bad := "✓", "!", "✗"
	problems := 0
	line := func(mark, label, format string, a ...any) {
		if mark == bad {
			problems++
		}
		fmt.Printf("%s %-10s %s\n", mark, label, fmt.Sprintf(format, a...))
	}

	fmt.Printf("incant %s\n\n", version)

	// Config.
	cfg, err := config.Load()
	switch {
	case err != nil:
		line(bad, "config", "%v", err)
		cfg = config.Default()
	case cfg.Path == "":
		line(warn, "config", "no home directory; using defaults")
	default:
		if _, statErr := os.Stat(cfg.Path); statErr != nil {
			line(ok, "config", "%s (not present; using defaults)", tilde(cfg.Path))
		} else {
			line(ok, "config", "%s", tilde(cfg.Path))
		}
	}
	for _, w := range cfg.Warnings {
		line(warn, "", "%s", w)
	}

	// Backend.
	creds := backend.Credentials(cfg.APIKey)
	cli := backend.NewFallback()
	switch {
	case cfg.Backend == "claude":
		if cli.Available() {
			line(ok, "backend", "claude CLI (forced by config; slower, and cannot look at files)")
		} else {
			line(bad, "backend", "config says claude, but the claude CLI is not on PATH")
		}
	case creds != "":
		line(ok, "backend", "api: %s, effort %s, credentials from %s", cfg.Model, cfg.Effort, creds)
		if cli.Available() {
			line(ok, "", "claude CLI available as a fallback if the API rejects those credentials")
		}
	case cfg.Backend == "api":
		line(bad, "backend", "config says api, but no credentials: set ANTHROPIC_API_KEY or api_key in config")
	case cli.Available():
		line(warn, "backend", "claude CLI fallback: works, but takes seconds per request and cannot look at files")
		line(warn, "", "for the fast path, set ANTHROPIC_API_KEY (or api_key in %s)", tilde(cfg.Path))
	default:
		line(bad, "backend", "no API credentials and no claude CLI; set ANTHROPIC_API_KEY")
	}

	// Shell integration.
	shell := filepath.Base(os.Getenv("SHELL"))
	checkRC(shell, line, ok, warn, bad)

	// Session facts and privacy.
	cwd, _ := os.Getwd()
	priv, err := config.LoadPrivacy(cwd)
	if err != nil {
		line(bad, "privacy", "reading %s: %v", config.IgnoreFile, err)
		priv = &config.Privacy{}
	}
	noFiles := cfg.Context != config.ContextFull || priv.Everything
	sctx, err := shellctx.Collect(ctx, shellctx.Options{
		Cwd: cwd, Shell: shell, Hidden: priv.Hidden,
		NoFiles: noFiles, NoHistory: cfg.Context == config.ContextNone,
	})
	if err != nil {
		line(bad, "context", "%v", err)
		return fmt.Errorf("%d problem(s)", problems)
	}
	ul := string(sctx.Userland)
	if len(sctx.GNUPrefix) > 0 {
		ul += " (GNU as " + strings.Join(sctx.GNUPrefix, ", ") + ")"
	}
	line(ok, "userland", "%s; tools: %s; missing: %s", ul, orNone(sctx.Tools), orNone(sctx.Missing))

	switch {
	case priv.Everything:
		line(ok, "privacy", "this tree is private (%s): no files, listing or git state are sent", tilde(priv.Files[0]))
	case len(priv.Files) > 0:
		line(ok, "privacy", "context = %s; hiding paths per %s", cfg.Context, tildeAll(priv.Files))
	default:
		line(ok, "privacy", "context = %s; no %s applies here", cfg.Context, config.IgnoreFile)
	}
	if !noFiles && creds != "" && cfg.Backend != "claude" {
		line(ok, "", "the model may also read files here through the probe tools (secrets and hidden paths refused)")
	}

	fmt.Println("\nA request from this directory sends, besides your request text:")
	fmt.Println(indent(sctx.Render()))

	if *ping {
		fmt.Println()
		pingBackend(ctx, cfg, sctx, line, ok, bad)
	}

	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	return nil
}

// checkRC looks for the shell-init line in the user's rc file.
func checkRC(shell string, line func(mark, label, format string, a ...any), ok, warn, bad string) {
	home, _ := os.UserHomeDir()
	var rc string
	switch shell {
	case "zsh":
		dir := os.Getenv("ZDOTDIR")
		if dir == "" {
			dir = home
		}
		rc = filepath.Join(dir, ".zshrc")
	case "bash":
		// The version is not checked here: that would mean starting a shell.
		// The integration itself warns at startup when bash is older than 4.
		rc = filepath.Join(home, ".bashrc")
	default:
		line(warn, "shell", "%q has no integration yet; zsh and bash are supported", shell)
		return
	}
	data, err := os.ReadFile(rc)
	if err == nil && strings.Contains(string(data), "incant shell-init") {
		line(ok, "shell", "%s: %s loads the integration", shell, tilde(rc))
		return
	}
	line(bad, "shell", "%s: add to %s:  eval \"$(incant shell-init %s)\"", shell, tilde(rc), shell)
}

// pingBackend makes one real request and reports its latency.
func pingBackend(ctx context.Context, cfg *config.Config, sctx *shellctx.Context, line func(mark, label, format string, a ...any), ok, bad string) {
	opt := options{backendName: cfg.Backend, model: cfg.Model, candidates: 1}
	primary, _, err := selectBackend(opt, cfg, nil, func(string, ...any) {})
	if err != nil {
		line(bad, "ping", "%v", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	got, err := primary.Suggest(ctx, backend.Request{Query: "count the files in this directory", Context: sctx, N: 1})
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		line(bad, "ping", "%s failed after %v: %v", primary.Name(), took, err)
		return
	}
	mark := ok
	note := ""
	if took > 3*time.Second {
		note = " — slow for a keybinding; the api backend with a faster model helps (model = claude-haiku-4-5)"
	}
	line(mark, "ping", "%s answered in %v: %s%s", primary.Name(), took, got[0].Command, note)
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func tildeAll(ps []string) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = tilde(p)
	}
	return strings.Join(out, ", ")
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
