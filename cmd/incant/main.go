// Command incant turns a natural-language request into a shell one-liner.
//
// The contract is deliberately narrow: read a request, print the command to
// stdout, exit 0. Placing that command in the user's line editor is the shell
// integration's job (see `incant shell-init`), and nothing in this binary ever
// executes what it prints.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/backend"
	"github.com/callumscott/incant/internal/config"
	"github.com/callumscott/incant/internal/impact"
	"github.com/callumscott/incant/internal/probe"
	"github.com/callumscott/incant/internal/shellctx"
	"github.com/callumscott/incant/internal/shellinit"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `incant — ask for a shell one-liner without leaving your prompt.

usage:
  incant [flags] <request>
  incant --fix
  incant --check <command>
  incant shell-init <zsh|bash>
  incant doctor [--ping]

flags:
  --shell NAME          shell to target (default: $SHELL)
  --cwd DIR             working directory to reason about (default: current)
  --fix                 repair the previous failed command
  --backend NAME        auto|api|claude (default: auto, or config)
  --model ID            model for the api backend (default: config, else claude-opus-5-5)
  -n, --candidates N    number of alternates to ask for
  --all                 print every alternate, one per line (the widget cycles them)
  --preview=false       skip the effect preview on stderr
  --check CMD           preview what CMD would change, without asking a model
  --last-command CMD    previous command, supplied by the shell hook
  --last-status N       previous exit status, supplied by the shell hook
  --last-stderr TEXT    previous stderr, supplied by the shell hook when enabled
  -v, --verbose         write diagnostics to stderr
  --timeout DURATION    overall deadline (default: 60s)
  -V, --version         print version and exit

setup:
  zsh:   eval "$(incant shell-init zsh)"     in ~/.zshrc
  bash:  eval "$(incant shell-init bash)"    in ~/.bashrc (bash 4+)
  then:  incant doctor

keys:
  ctrl-x ctrl-i   turn the current line into a command; press again for the next alternate
  ctrl-x ctrl-u   put back the request you typed
  ctrl-x ctrl-f   repair the previous failed command
  ctrl-x ctrl-p   preview what the current line would change

effect preview:
  after a suggestion, incant checks which files it would delete, overwrite,
  edit, move or discard, and prints one line to stderr (shown under the
  prompt by the widget), e.g.
      ⚠ deletes 212 files (1.4G) — ./logs (200), ./tmp/cache (12)
  nothing is run to work this out: globs, find expressions and git pathspecs
  are evaluated in-process, read-only, inside the current directory.

privacy:
  settings live in ~/.config/incant/config; see README for every key.
  a .incantignore file hides paths (or, empty, a whole tree) from the model.
  incant doctor shows exactly what a request from here would send.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "incant: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	shell       string
	cwd         string
	fix         bool
	backendName string
	model       string
	lastCommand string
	lastStatus  int
	lastStderr  string
	candidates  int
	all         bool
	verbose     bool
	timeout     time.Duration
	showVersion bool
	preview     bool
	check       string
}

func run(args []string) error {
	// Subcommands are handled before flag parsing so they work regardless of
	// what the rest of the flag surface looks like. They are matched only as
	// the sole first word's exact spelling, so "incant doctor the csv" is
	// still a request.
	if len(args) > 0 {
		switch {
		case args[0] == "shell-init":
			return runShellInit(args[1:])
		case args[0] == "doctor" && (len(args) == 1 || strings.HasPrefix(args[1], "-")):
			return runDoctor(args[1:])
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	var opt options
	fs := flag.NewFlagSet("incant", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	fs.StringVar(&opt.shell, "shell", "", "shell to target")
	fs.StringVar(&opt.cwd, "cwd", "", "working directory")
	fs.BoolVar(&opt.fix, "fix", false, "repair the previous failed command")
	fs.StringVar(&opt.backendName, "backend", cfg.Backend, "backend to use")
	fs.StringVar(&opt.model, "model", cfg.Model, "model for the api backend")
	fs.StringVar(&opt.lastCommand, "last-command", "", "previous command")
	fs.IntVar(&opt.lastStatus, "last-status", 0, "previous exit status")
	fs.StringVar(&opt.lastStderr, "last-stderr", "", "previous stderr")
	fs.IntVar(&opt.candidates, "candidates", 0, "number of alternates")
	fs.IntVar(&opt.candidates, "n", 0, "number of alternates (shorthand)")
	fs.BoolVar(&opt.all, "all", false, "print every alternate")
	fs.BoolVar(&opt.verbose, "verbose", false, "diagnostics to stderr")
	fs.BoolVar(&opt.verbose, "v", false, "diagnostics to stderr (shorthand)")
	fs.DurationVar(&opt.timeout, "timeout", 60*time.Second, "overall deadline")
	fs.BoolVar(&opt.showVersion, "version", false, "print version")
	fs.BoolVar(&opt.showVersion, "V", false, "print version (shorthand)")
	fs.BoolVar(&opt.preview, "preview", cfg.Preview, "print the effect preview to stderr")
	fs.StringVar(&opt.check, "check", "", "preview a command without asking a model")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if opt.showVersion {
		fmt.Printf("incant %s\n", version)
		return nil
	}
	if opt.candidates <= 0 {
		opt.candidates = 1
		if opt.all {
			opt.candidates = cfg.Candidates
		}
	}
	if opt.verbose {
		for _, w := range cfg.Warnings {
			fmt.Fprintf(os.Stderr, "config: %s\n", w)
		}
	}

	if opt.check != "" {
		return runCheck(opt)
	}

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" && !opt.fix {
		fs.Usage()
		return errors.New("no request given")
	}

	// Ctrl-C at the prompt should abandon the incantation cleanly, leaving the
	// user's buffer untouched.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opt.timeout)
	defer cancel()

	cmds, sctx, err := suggest(ctx, opt, cfg, query)
	if err != nil {
		return err
	}

	// stdout carries the command and nothing else -- or, with --all, the
	// alternates one per line -- because the shell integration assigns it
	// straight into the line editor.
	if opt.all {
		fmt.Println(strings.Join(cmds, "\n"))
	} else {
		fmt.Println(cmds[0])
	}

	// The preview goes to stderr, which the widget shows under the prompt. It
	// is advisory: failing to compute it never fails the suggestion.
	if opt.preview {
		start := time.Now()
		line := preview(ctx, sctx, cmds[0])
		if opt.verbose {
			fmt.Fprintf(os.Stderr, "preview computed in %v\n", time.Since(start).Round(time.Millisecond))
		}
		if line != "" {
			fmt.Fprintln(os.Stderr, line)
		}
	}
	return nil
}

func runShellInit(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: incant shell-init <shell> (have: %s)",
			strings.Join(shellinit.Supported(), ", "))
	}
	script, err := shellinit.Script(args[0])
	if err != nil {
		return err
	}
	fmt.Print(script)
	return nil
}

// runCheck previews a command the user supplies, with no model involved.
// Here the preview is the product, so it goes to stdout. "No changes" is
// spelled out only for a person at a terminal; the widgets want silence.
func runCheck(opt options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cwd, err := resolveCwd(opt.cwd)
	if err != nil {
		return err
	}
	sctx, err := shellctx.Collect(ctx, shellctx.Options{Cwd: cwd, Shell: opt.shell, NoFiles: true})
	if err != nil {
		return err
	}
	line := preview(ctx, sctx, opt.check)
	if line == "" && isTerminal(os.Stdout) {
		line = "no file changes detected"
	}
	if line != "" {
		fmt.Println(line)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func resolveCwd(cwd string) (string, error) {
	if cwd != "" {
		return cwd, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determining cwd: %w", err)
	}
	return wd, nil
}

// Preview limits. The preview runs after the model has answered, while the
// user waits at the prompt, so it gets a small budget of its own; a large tree
// degrades to "at least N files" instead of a stall.
const (
	previewMaxCalls  = 64
	previewPerCall   = 1500 * time.Millisecond
	previewAggregate = 2 * time.Second
)

// preview describes what cmd would change in the session's cwd, or "".
//
// It deliberately ignores .incantignore: the preview never leaves the machine,
// and hiding private files from it would understate what a command deletes.
func preview(ctx context.Context, sctx *shellctx.Context, cmd string) string {
	sb, err := probe.NewSandbox(sctx.Cwd, nil,
		probe.NewBudget(previewMaxCalls, previewPerCall, previewAggregate))
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	return impact.Analyze(ctx, sb, cmd, impact.Options{
		Shell: sctx.Shell,
		BSD:   sctx.Userland == shellctx.UserlandBSD,
		Home:  home,
	}).String()
}

// session is what one request knows about where it runs: the context sent up
// front, and the sandbox (if any) the model may probe.
type session struct {
	sctx    *shellctx.Context
	sandbox *probe.Sandbox
	privacy *config.Privacy
}

// collect gathers the session under the user's context level and privacy
// rules.
func collect(ctx context.Context, opt options, cfg *config.Config, cwd string) (*session, error) {
	priv, err := config.LoadPrivacy(cwd)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", config.IgnoreFile, err)
	}
	noFiles := cfg.Context != config.ContextFull || priv.Everything

	hasLast := strings.TrimSpace(opt.lastCommand) != ""
	sctx, err := shellctx.Collect(ctx, shellctx.Options{
		Cwd:         cwd,
		Shell:       opt.shell,
		LastCommand: opt.lastCommand,
		LastStatus:  opt.lastStatus,
		LastStderr:  opt.lastStderr,
		HasLast:     hasLast,
		Hidden:      priv.Hidden,
		NoFiles:     noFiles,
		NoHistory:   cfg.Context == config.ContextNone,
	})
	if err != nil {
		return nil, err
	}

	s := &session{sctx: sctx, privacy: priv}
	if !noFiles {
		sb, err := probe.NewSandbox(cwd, nil, probe.DefaultBudget())
		if err != nil {
			return nil, err
		}
		sb.SetHidden(priv.Hidden)
		s.sandbox = sb
	}
	return s, nil
}

func suggest(ctx context.Context, opt options, cfg *config.Config, query string) ([]string, *shellctx.Context, error) {
	cwd, err := resolveCwd(opt.cwd)
	if err != nil {
		return nil, nil, err
	}
	if opt.fix && strings.TrimSpace(opt.lastCommand) == "" {
		return nil, nil, errors.New("--fix needs a previous command; is the shell hook installed? (incant doctor)")
	}
	if opt.fix && cfg.Context == config.ContextNone {
		return nil, nil, errors.New("--fix needs the previous command, but config has context = none")
	}

	logf := func(format string, args ...any) {
		if opt.verbose {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}

	start := time.Now()
	sess, err := collect(ctx, opt, cfg, cwd)
	if err != nil {
		return nil, nil, err
	}
	logf("context collected in %v", time.Since(start).Round(time.Millisecond))
	logf("--- context ---\n%s---------------", sess.sctx.Render())

	primary, fallback, err := selectBackend(opt, cfg, sess.sandbox, logf)
	if err != nil {
		return nil, nil, err
	}
	logf("backend: %s", primary.Name())

	req := backend.Request{Query: query, Context: sess.sctx, Fix: opt.fix, N: opt.candidates}
	start = time.Now()
	candidates, err := primary.Suggest(ctx, req)
	if err != nil && fallback != nil && backend.IsAuthError(err) {
		logf("api credentials rejected (%v); falling back to %s", err, fallback.Name())
		candidates, err = fallback.Suggest(ctx, req)
	}
	if err != nil {
		if errors.Is(err, backend.ErrUnsupported) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%s: %w", primary.Name(), err)
	}
	if len(candidates) == 0 {
		return nil, nil, errors.New("backend returned no candidates")
	}
	logf("answered in %v (%d probe(s))", time.Since(start).Round(time.Millisecond), candidates[0].Probes)

	cmds := make([]string, len(candidates))
	for i, c := range candidates {
		cmds[i] = c.Command
	}
	return cmds, sess.sctx, nil
}

// selectBackend resolves the backend. With "auto", the API backend is used
// when credentials exist, with the claude CLI kept as a fallback should the
// API reject them; with no credentials, the CLI is used directly.
func selectBackend(opt options, cfg *config.Config, sb *probe.Sandbox, logf func(string, ...any)) (primary, fallback backend.Backend, err error) {
	api := func() backend.Backend {
		return backend.NewAPI(backend.APIOptions{
			APIKey:  cfg.APIKey,
			Model:   opt.model,
			Effort:  cfg.Effort,
			Sandbox: sb,
			Logf:    logf,
		})
	}
	cli := backend.NewFallback()

	switch opt.backendName {
	case "", "auto":
		if backend.Credentials(cfg.APIKey) != "" {
			if cli.Available() {
				return api(), cli, nil
			}
			return api(), nil, nil
		}
		if cli.Available() {
			return cli, nil, nil
		}
		return nil, nil, errors.New("no API credentials and no claude CLI; set ANTHROPIC_API_KEY or see: incant doctor")
	case "api":
		return api(), nil, nil
	case "claude", "claude-cli":
		if !cli.Available() {
			return nil, nil, errors.New("claude CLI not found on PATH")
		}
		return cli, nil, nil
	}
	return nil, nil, fmt.Errorf("unknown backend %q (have: auto, api, claude)", opt.backendName)
}
