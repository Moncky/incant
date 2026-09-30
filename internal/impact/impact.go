// Package impact previews what a suggested command would do to the user's
// files, without running it.
//
// incant's user reads every command before pressing Enter, but reading
// `find . -name '*.log' -mtime +7 -delete` says what the command means, not
// what it will do here. This package closes that gap: it lexes the command,
// recognises the operations that change files -- rm, find -delete, mv, sed -i,
// redirections that clobber, git reset --hard, and a few more -- and evaluates
// their operands against the real filesystem through the probe sandbox. The
// result is one line such as
//
//	⚠ deletes 212 files (1.4G) — ./logs (200), ./tmp/cache (12)
//
// Two rules keep that line honest. Nothing is ever executed: globs, find
// expressions and git pathspecs are evaluated in Go over metadata the sandbox
// hands back. And anything that would change files but cannot be evaluated --
// a $VAR operand, a path outside the cwd, find -exec sh -c -- is reported as
// unpreviewable rather than left out, because silence reads as "harmless".
package impact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/glob"
	"github.com/callumscott/incant/internal/probe"
)

// Options describes the environment the command will run in.
type Options struct {
	// Shell is the target shell; zsh turns on ** recursive globbing.
	Shell string
	// BSD selects BSD semantics where they differ from GNU (sed -i, find
	// -mtime rounding).
	BSD bool
	// Home expands a leading ~. Empty leaves ~ paths unpreviewable.
	Home string
	// Now anchors find's -mtime and -mmin. Zero means time.Now().
	Now time.Time
	// Limit caps entries visited per operand. Zero means a default.
	Limit int
}

// Verbs, in the order effects are reported.
const (
	verbDelete    = "deletes"
	verbDiscard   = "discards uncommitted changes to"
	verbOverwrite = "overwrites"
	verbTruncate  = "truncates"
	verbEdit      = "edits in place"
	verbMove      = "moves"
	verbMode      = "changes permissions on"
	verbOwner     = "changes ownership of"
)

var verbOrder = []string{verbDelete, verbDiscard, verbOverwrite, verbTruncate, verbEdit, verbMove, verbMode, verbOwner}

// Effect is one kind of change, aggregated across the command line.
type Effect struct {
	Verb    string
	Files   int
	Dirs    int
	Bytes   int64
	AtLeast bool     // a walk was cut short; the counts are lower bounds
	To      string   // destination, for moves
	Misses  []string // operands that matched nothing
	Notes   []string

	labels []string
	counts map[string]int
	seen   map[string]bool
}

// Report is the preview of one command line.
type Report struct {
	// Warnings are hazards worth reading before anything else, such as a
	// redirection that empties a file the same pipeline is about to read.
	Warnings []string
	Effects  []*Effect
	// Unknown lists operations that would change files but could not be
	// evaluated, each with the reason.
	Unknown []string
}

// Empty reports whether there is nothing to show.
func (r *Report) Empty() bool {
	return len(r.Warnings) == 0 && len(r.Effects) == 0 && len(r.Unknown) == 0
}

// Analyze previews command. It never fails: a command it cannot lex is itself
// reported as unpreviewable.
func Analyze(ctx context.Context, sb *probe.Sandbox, command string, opt Options) *Report {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.Limit <= 0 {
		opt.Limit = 50000
	}
	a := &analyzer{ctx: ctx, sb: sb, opt: opt, effects: map[string]*Effect{}}

	items, err := parse(command)
	if err != nil {
		a.unknown("the command", "could not parse: "+err.Error())
		return a.report()
	}
	for _, it := range items {
		switch {
		case it.open:
			a.stack = append(a.stack, a.dir)
		case it.close:
			if n := len(a.stack); n > 0 {
				a.dir, a.stack = a.stack[n-1], a.stack[:n-1]
			}
		default:
			a.pipeline(it.pipeline)
		}
	}
	return a.report()
}

// dirState is the working directory the next command runs in, relative to the
// sandbox root, as moved by cd.
type dirState struct {
	rel string // "" is the root
	bad string // non-empty: relative paths cannot be resolved, and why
}

type analyzer struct {
	ctx     context.Context
	sb      *probe.Sandbox
	opt     Options
	dir     dirState
	stack   []dirState
	effects map[string]*Effect
	r       Report

	gitChanges []probe.GitChange
	gitLoaded  bool
	gitOK      bool
}

// arg is one operand: a word from the command line, or entries supplied by a
// find or xargs upstream.
type arg struct {
	w        word
	entries  []probe.Entry
	supplied bool
}

func wordArgs(ws []word) []arg {
	out := make([]arg, len(ws))
	for i, w := range ws {
		out[i] = arg{w: w}
	}
	return out
}

func (a *analyzer) unknown(what, why string) {
	a.r.Unknown = append(a.r.Unknown, fmt.Sprintf("%s (%s)", what, why))
}

func (a *analyzer) effect(verb string) *Effect {
	e, ok := a.effects[verb]
	if !ok {
		e = &Effect{Verb: verb, counts: map[string]int{}, seen: map[string]bool{}}
		a.effects[verb] = e
	}
	return e
}

// add records one entry under verb, attributed to label.
func (a *analyzer) add(verb string, e probe.Entry, label string) {
	eff := a.effect(verb)
	if eff.seen[e.Real] {
		return
	}
	eff.seen[e.Real] = true
	if e.IsDir() {
		eff.Dirs++
	} else {
		eff.Files++
		eff.Bytes += e.Size
	}
	if _, ok := eff.counts[label]; !ok {
		eff.labels = append(eff.labels, label)
	}
	eff.counts[label]++
}

// addTree records e and, for a real directory, everything beneath it.
func (a *analyzer) addTree(verb string, e probe.Entry, label string) {
	if !e.IsDir() || e.IsLink() {
		a.add(verb, e, label)
		return
	}
	truncated, err := a.sb.Walk(a.ctx, e.Real, -1, a.opt.Limit, func(c probe.Entry, _ int) error {
		a.add(verb, c, label)
		return nil
	})
	if err != nil {
		a.unknown(verb+" "+a.sb.Rel(e.Real), reason(err))
		return
	}
	if truncated {
		a.effect(verb).AtLeast = true
	}
}

func (a *analyzer) miss(verb, what string) {
	eff := a.effect(verb)
	eff.Misses = append(eff.Misses, what)
}

// label is how an entry is named in the summary.
func (a *analyzer) label(e probe.Entry, supplied bool) string {
	if supplied {
		// Found entries are grouped by their directory: "./logs (200)"
		// says more than 200 file names.
		return a.sb.Rel(path.Dir(e.Real))
	}
	return a.sb.Rel(e.Real)
}

// resolve turns an operand into the entries it names. ok is false when it
// could not be evaluated; the reason has already been recorded against what.
func (a *analyzer) resolve(x arg, what string) ([]probe.Entry, bool) {
	if x.supplied {
		return x.entries, true
	}
	w := x.w
	if w.dynamic != "" {
		a.unknown(what+" "+w.raw, w.dynamic)
		return nil, false
	}
	pat := w.pat
	switch {
	case w.tilde:
		if a.opt.Home == "" {
			a.unknown(what+" "+w.raw, "home directory unknown")
			return nil, false
		}
		pat = escapeGlob(a.opt.Home) + strings.TrimPrefix(pat, "~")
	case !strings.HasPrefix(pat, "/"):
		if a.dir.bad != "" {
			a.unknown(what+" "+w.raw, a.dir.bad)
			return nil, false
		}
		if a.dir.rel != "" {
			pat = escapeGlob(a.dir.rel) + "/" + pat
		}
	}
	if !w.glob {
		pat = escapeGlob(glob.Unescape(pat))
	}

	ents, truncated, err := a.sb.Match(a.ctx, pat, probe.MatchOptions{
		Globstar: a.opt.Shell == "zsh",
		Limit:    a.opt.Limit,
	})
	if err != nil {
		a.unknown(what+" "+w.raw, reason(err))
		return nil, false
	}
	if truncated {
		a.unknown(what+" "+w.raw, "too many matches to count")
		return nil, false
	}
	return ents, true
}

func escapeGlob(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isMeta(s[i]) || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func reason(err error) string {
	switch {
	case errors.Is(err, probe.ErrEscape):
		return "outside the current directory"
	case errors.Is(err, probe.ErrBudget), errors.Is(err, context.DeadlineExceeded):
		return "preview ran out of time"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return err.Error()
}

// pipeline analyses one pipeline, left to right, so a find can feed an xargs.
func (a *analyzer) pipeline(cmds []simpleCmd) {
	var upstream []probe.Entry
	upstreamOK := false
	upstreamName := ""

	for i, c := range cmds {
		words := strip(c.words)
		a.redirects(c, cmds)

		var out []probe.Entry
		outOK := false
		if len(words) > 0 {
			name := path.Base(words[0].lit)
			if words[0].dynamic != "" {
				name = ""
			}
			switch name {
			case "cd", "pushd", "popd":
				a.cd(name, words[1:])
			case "xargs":
				a.xargs(words[1:], upstream, upstreamOK, upstreamName, i > 0)
			case "find", "gfind":
				out, outOK = a.find(words[1:])
			default:
				a.run(name, wordArgs(words[1:]))
			}
			upstreamName = name
		}
		upstream, upstreamOK = out, outOK
	}
}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// strip removes assignments, reserved words and wrapper commands that only
// change how the real command runs: `sudo -u x rm` is rm.
func strip(ws []word) []word {
	for len(ws) > 0 {
		w := ws[0]
		switch {
		case assignRe.MatchString(w.raw):
			ws = ws[1:]
		case w.dynamic != "":
			return ws
		default:
			switch w.lit {
			case "{", "}", "!", "do", "then", "else", "elif", "if", "while", "until", "time",
				"nohup", "command", "builtin", "exec", "nocorrect", "noglob":
				ws = ws[1:]
			case "sudo", "doas", "env", "nice", "ionice", "caffeinate", "timeout", "gtimeout", "stdbuf":
				ws = skipWrapperFlags(w.lit, ws[1:])
			default:
				return ws
			}
		}
	}
	return ws
}

func skipWrapperFlags(wrapper string, ws []word) []word {
	takesArg := map[string]bool{
		"-u": true, "-g": true, "-U": true, "-C": true, "-h": true, "-p": true,
		"-r": true, "-t": true, "-T": true, "-n": true, "-c": true, "-s": true,
		"-k": true, "-i": true, "-o": true, "-e": true,
	}
	for len(ws) > 0 {
		l := ws[0].lit
		switch {
		case l == "--":
			return ws[1:]
		case wrapper == "env" && assignRe.MatchString(ws[0].raw):
			ws = ws[1:]
		case strings.HasPrefix(l, "-") && len(l) > 1:
			ws = ws[1:]
			if takesArg[l] && wrapper != "env" && len(ws) > 0 {
				ws = ws[1:]
			}
		case (wrapper == "timeout" || wrapper == "gtimeout"):
			return ws[1:] // the duration
		default:
			return ws
		}
	}
	return ws
}

// cd moves the directory relative operands resolve against.
func (a *analyzer) cd(name string, args []word) {
	if name != "cd" || len(args) == 0 || args[0].lit == "-" || (args[0].tilde && args[0].lit == "~") {
		a.dir.bad = "after " + name + " to an unpreviewed directory"
		return
	}
	target := args[0]
	if target.lit == "--" && len(args) > 1 {
		target = args[1]
	}
	ents, ok := a.resolveQuiet(target)
	if !ok || len(ents) != 1 || !ents[0].IsDir() && !ents[0].IsLink() {
		a.dir.bad = "after cd " + target.raw
		return
	}
	real, err := a.sb.Resolve(ents[0].Real)
	if err != nil {
		a.dir.bad = "after cd " + target.raw + " outside the current directory"
		return
	}
	rel := strings.TrimPrefix(a.sb.Rel(real), "./")
	if strings.HasPrefix(rel, "/") {
		a.dir.bad = "after cd " + target.raw + " outside the current directory"
		return
	}
	if rel == "." {
		rel = ""
	}
	a.dir = dirState{rel: rel}
}

// resolveQuiet is resolve without recording failures.
func (a *analyzer) resolveQuiet(w word) ([]probe.Entry, bool) {
	saved := len(a.r.Unknown)
	ents, ok := a.resolve(arg{w: w}, "")
	a.r.Unknown = a.r.Unknown[:saved]
	return ents, ok
}

// redirects reports clobbering output redirections, and the classic
// `sort f > f`, where the shell empties f before sort ever reads it.
func (a *analyzer) redirects(c simpleCmd, pipeline []simpleCmd) {
	for _, r := range c.redirs {
		if r.op != ">" && r.op != ">|" && r.op != "&>" {
			continue
		}
		t := r.target
		if strings.HasPrefix(t.lit, "/dev/") || isAllDigits(t.lit) || t.lit == "" {
			continue
		}
		if t.dynamic != "" {
			a.unknown("> "+t.raw, t.dynamic)
			continue
		}
		ents, ok := a.resolve(arg{w: t}, ">")
		if !ok || len(ents) != 1 || !ents[0].IsRegular() {
			continue
		}
		target := ents[0]
		a.add(verbOverwrite, target, a.sb.Rel(target.Real))

		for _, other := range pipeline {
			ws := strip(other.words)
			if len(ws) == 0 {
				continue
			}
			for _, w := range ws[1:] {
				if w.dynamic != "" || strings.HasPrefix(w.lit, "-") {
					continue
				}
				got, ok := a.resolveQuiet(w)
				if ok && len(got) == 1 && got[0].Real == target.Real {
					a.r.Warnings = append(a.r.Warnings, fmt.Sprintf(
						"%s is emptied by > before %s reads it", a.sb.Rel(target.Real), path.Base(ws[0].lit)))
					goto next
				}
			}
		}
	next:
	}
}

// find evaluates a find command, records the effects of -delete and -exec,
// and returns what it prints for a downstream xargs.
func (a *analyzer) find(args []word) ([]probe.Entry, bool) {
	fc, err := parseFind(args, a.opt.Now, a.opt.BSD)
	if err != nil {
		if hasFindAction(args) {
			a.unknown("find", err.Error())
		}
		return nil, false
	}

	ev := &findEval{
		cmd: fc, now: a.opt.Now, bsd: a.opt.BSD,
		execd:    map[int][]probe.Entry{},
		refCache: map[string]time.Time{},
		stat: func(w word) (probe.Entry, error) {
			ents, ok := a.resolveQuiet(w)
			if !ok || len(ents) != 1 {
				return probe.Entry{}, fs.ErrNotExist
			}
			return ents[0], nil
		},
	}

	for _, root := range fc.roots {
		ents, ok := a.resolve(arg{w: root}, "find")
		if !ok {
			return nil, false
		}
		for _, re := range ents {
			prefix := strings.TrimSuffix(root.lit, "/")
			if prefix == "" {
				prefix = "/"
			}
			truncated, err := a.sb.Walk(a.ctx, re.Real, fc.maxDepth, a.opt.Limit, func(e probe.Entry, depth int) error {
				if depth < fc.minDepth {
					return nil
				}
				display := prefix + strings.TrimPrefix(e.Real, re.Real)
				ev.pruned = false
				fc.expr.match(ev, e, display, depth)
				if ev.pruned {
					return probe.ErrSkipDir
				}
				return nil
			})
			if err != nil {
				a.unknown("find "+root.raw, reason(err))
				return nil, false
			}
			if truncated {
				for _, v := range []string{verbDelete, verbEdit, verbMove, verbMode, verbOwner} {
					if _, ok := a.effects[v]; ok {
						a.effects[v].AtLeast = true
					}
				}
				if len(ev.deleted) > 0 || len(ev.execd) > 0 {
					a.effect(verbDelete).AtLeast = true
				}
			}
		}
	}

	if hasAction(fc, "delete") {
		a.findDelete(ev.deleted)
	}
	for i, cmd := range fc.execs {
		a.findExec(cmd, ev.execd[i])
	}
	return ev.printed, fc.printed
}

func hasFindAction(args []word) bool {
	for _, w := range args {
		switch w.lit {
		case "-delete", "-exec", "-execdir", "-ok", "-okdir":
			return true
		}
	}
	return false
}

func hasAction(fc *findCmd, kind string) bool {
	var walk func(n node) bool
	walk = func(n node) bool {
		switch n := n.(type) {
		case andNode:
			return walk(n.a) || walk(n.b)
		case orNode:
			return walk(n.a) || walk(n.b)
		case notNode:
			return walk(n.a)
		case actionNode:
			return n.kind == kind
		}
		return false
	}
	return walk(fc.expr)
}

// findDelete records -delete, which removes a directory only if it is empty
// once its own matched children are gone. That is why
// `find . -name node_modules -delete` deletes nothing at all.
func (a *analyzer) findDelete(matched []probe.Entry) {
	if len(matched) == 0 {
		a.miss(verbDelete, "find matches nothing")
		return
	}
	gone := map[string]bool{}
	removedChildren := map[string]int{}
	// Deepest first, so a directory sees its children's fate.
	sort.Slice(matched, func(i, j int) bool {
		return strings.Count(matched[i].Real, "/") > strings.Count(matched[j].Real, "/")
	})
	var kept []probe.Entry
	refused := 0
	for _, e := range matched {
		if e.IsDir() && !e.IsLink() && e.Children != removedChildren[e.Real] {
			refused++
			continue
		}
		gone[e.Real] = true
		removedChildren[path.Dir(e.Real)]++
		kept = append(kept, e)
	}
	for _, e := range kept {
		a.add(verbDelete, e, a.label(e, true))
	}
	if refused > 0 {
		eff := a.effect(verbDelete)
		eff.Notes = append(eff.Notes, fmt.Sprintf("-delete skips %d non-empty %s", refused, plural(refused, "dir", "dirs")))
	}
}

// findExec treats `-exec cmd {} ;` as cmd run over the matched entries.
func (a *analyzer) findExec(cmd []word, matched []probe.Entry) {
	if len(cmd) == 0 {
		return
	}
	name := path.Base(cmd[0].lit)
	switch name {
	case "sh", "bash", "zsh", "dash":
		a.unknown("find -exec "+name, "runs a shell script")
		return
	}
	var args []arg
	placed := false
	for _, w := range cmd[1:] {
		switch {
		case w.lit == "{}":
			args = append(args, arg{entries: matched, supplied: true})
			placed = true
		case strings.Contains(w.lit, "{}"):
			w.dynamic = "built from find's {}"
			args = append(args, arg{w: w})
		default:
			args = append(args, arg{w: w})
		}
	}
	if !placed {
		return
	}
	a.run(name, args)
}

// xargs runs its command over the entries an upstream find printed.
func (a *analyzer) xargs(args []word, upstream []probe.Entry, upstreamOK bool, from string, piped bool) {
	placeholder := ""
	i := 0
	valued := map[string]bool{"-n": true, "-P": true, "-L": true, "-s": true, "-d": true, "-E": true, "-a": true, "-I": true}
	for i < len(args) {
		l := args[i].lit
		if l == "--" {
			i++
			break
		}
		if !strings.HasPrefix(l, "-") || l == "-" {
			break
		}
		switch {
		case l == "-I" && i+1 < len(args):
			placeholder = args[i+1].lit
			i += 2
			continue
		case strings.HasPrefix(l, "-I") && len(l) > 2:
			placeholder = l[2:]
		case l == "-i" || l == "--replace":
			placeholder = "{}"
		case strings.HasPrefix(l, "-i") && len(l) > 2:
			placeholder = l[2:]
		case valued[l]:
			i++
		}
		i++
	}
	if i >= len(args) {
		return // bare xargs echoes
	}
	name := path.Base(args[i].lit)
	rest := args[i+1:]

	if !mutating[name] {
		return
	}
	if !piped || !upstreamOK || from == "" {
		src := from
		if src == "" {
			src = "stdin"
		}
		a.unknown("xargs "+name, "input comes from "+src)
		return
	}

	input := arg{entries: upstream, supplied: true}
	var out []arg
	placed := false
	for _, w := range rest {
		if placeholder != "" && w.lit == placeholder {
			out = append(out, input)
			placed = true
			continue
		}
		out = append(out, arg{w: w})
	}
	if !placed {
		out = append(out, input)
	}
	a.run(name, out)
}

// mutating names the commands this package models.
var mutating = map[string]bool{
	"rm": true, "grm": true, "unlink": true, "rmdir": true,
	"mv": true, "gmv": true, "cp": true, "gcp": true,
	"sed": true, "gsed": true, "perl": true,
	"chmod": true, "chown": true, "chgrp": true,
	"truncate": true, "tee": true, "dd": true, "shred": true,
	"git": true, "rsync": true,
}

// run dispatches one command.
func (a *analyzer) run(name string, args []arg) {
	switch name {
	case "rm", "grm", "unlink", "rmdir":
		a.rm(name, args)
	case "mv", "gmv":
		a.mv(name, args, true)
	case "cp", "gcp":
		a.mv(name, args, false)
	case "sed", "gsed":
		a.sed(name, args)
	case "perl":
		a.perl(args)
	case "chmod", "chown", "chgrp":
		a.chmod(name, args)
	case "truncate":
		a.simpleWrite(name, args, verbTruncate, map[string]bool{"-s": true, "-r": true})
	case "tee":
		a.simpleWrite(name, args, verbOverwrite, nil)
	case "shred":
		a.simpleWrite(name, args, verbOverwrite, map[string]bool{"-n": true, "-s": true})
	case "dd":
		a.dd(args)
	case "git":
		a.git(args)
	case "rsync":
		for _, x := range args {
			if strings.HasPrefix(x.w.lit, "--delete") || x.w.lit == "--remove-source-files" {
				a.unknown("rsync "+x.w.lit, "depends on comparing two trees")
				return
			}
		}
	}
}

// flagged splits args into flags and operands. Flags may follow operands, as
// GNU tools allow, until a bare --. valued names flags that take the next
// word as their value.
func flagged(args []arg, valued map[string]bool) (flags []string, values map[string]string, ops []arg) {
	values = map[string]string{}
	done := false
	for i := 0; i < len(args); i++ {
		x := args[i]
		l := x.w.lit
		switch {
		case x.supplied || done || !strings.HasPrefix(l, "-") || l == "-":
			ops = append(ops, x)
		case l == "--":
			done = true
		default:
			flags = append(flags, l)
			if valued[l] && i+1 < len(args) {
				values[l] = args[i+1].w.lit
				i++
			}
		}
	}
	return flags, values, ops
}

// hasFlag reports whether a short letter appears in any bundle, or a long
// flag is present.
func hasFlag(flags []string, short string, long ...string) bool {
	for _, f := range flags {
		if strings.HasPrefix(f, "--") {
			for _, l := range long {
				if f == l || strings.HasPrefix(f, l+"=") {
					return true
				}
			}
			continue
		}
		if short != "" && strings.ContainsAny(f[1:], short) {
			return true
		}
	}
	return false
}

func (a *analyzer) rm(name string, args []arg) {
	flags, _, ops := flagged(args, nil)
	recursive := name != "rmdir" && hasFlag(flags, "rR", "--recursive")
	emptyDirs := name == "rmdir" || hasFlag(flags, "d", "--dir")

	evaluated := false
	for _, x := range ops {
		ents, ok := a.resolve(x, name)
		if !ok {
			continue
		}
		evaluated = true
		if len(ents) == 0 && !x.supplied {
			a.miss(verbDelete, "no match for "+x.w.raw)
			continue
		}
		for _, e := range ents {
			label := a.label(e, x.supplied)
			switch {
			case !e.IsDir() || e.IsLink():
				if name != "rmdir" {
					a.add(verbDelete, e, label)
				}
			case recursive:
				a.addTree(verbDelete, e, label)
			case emptyDirs && e.Children == 0:
				a.add(verbDelete, e, label)
			}
		}
	}
	// An rm that was evaluated and matched nothing still says so; one whose
	// operands were all unpreviewable must not claim "deletes nothing".
	if evaluated {
		a.effect(verbDelete)
	}
}

// mv previews mv (move=true) or cp: what moves, and what gets clobbered.
func (a *analyzer) mv(name string, args []arg, move bool) {
	flags, values, ops := flagged(args, map[string]bool{"-t": true, "-S": true})
	noClobber := hasFlag(flags, "ni", "--no-clobber", "--interactive", "--update")

	var destArg arg
	if t, ok := values["-t"]; ok {
		destArg = arg{w: word{lit: t, pat: escapeGlob(t), raw: t}}
	} else {
		if len(ops) < 2 {
			return
		}
		destArg, ops = ops[len(ops)-1], ops[:len(ops)-1]
	}
	if destArg.supplied {
		return
	}

	var sources []probe.Entry
	for _, x := range ops {
		ents, ok := a.resolve(x, name)
		if !ok {
			return
		}
		if len(ents) == 0 && !x.supplied && move {
			a.miss(verbMove, "no match for "+x.w.raw)
		}
		sources = append(sources, ents...)
	}
	if len(sources) == 0 {
		return
	}

	dests, ok := a.resolve(destArg, name)
	if !ok {
		return
	}
	var dest *probe.Entry
	if len(dests) == 1 {
		dest = &dests[0]
	} else if len(dests) > 1 {
		return // a glob destination: the shell splits it into more sources
	}
	intoDir := dest != nil && (dest.IsDir() || dest.IsLink() && strings.HasSuffix(destArg.w.lit, "/"))

	if move {
		eff := a.effect(verbMove)
		to := destArg.w.lit
		if dest != nil {
			to = a.sb.Rel(dest.Real)
		}
		eff.To = to
		for _, s := range sources {
			a.add(verbMove, s, "")
		}
	}

	if noClobber {
		return
	}
	for _, s := range sources {
		var target string
		switch {
		case intoDir:
			target = path.Join(dest.Real, path.Base(s.Real))
		case len(sources) == 1 && dest != nil:
			target = dest.Real
		default:
			continue
		}
		if s.IsDir() {
			continue
		}
		if got, ok := a.resolveQuiet(word{lit: target, pat: escapeGlob(target), raw: target}); ok && len(got) == 1 && got[0].IsRegular() && got[0].Real != s.Real {
			a.add(verbOverwrite, got[0], a.sb.Rel(got[0].Real))
		}
	}
}

// looksLikeSedScript spots a word that is plainly an s or y command, such as
// s/a/b/ or s|x|y|, rather than a backup suffix.
func looksLikeSedScript(w string) bool {
	if len(w) < 3 || (w[0] != 's' && w[0] != 'y') || !strings.ContainsRune("/|#,:", rune(w[1])) {
		return false
	}
	return strings.Count(w[2:], w[1:2]) >= 2
}

// sed previews sed -i. BSD sed's -i always takes the next word as the backup
// suffix, so `sed -i 's/a/b/' f` on macOS treats the script as a suffix and
// the file name as the script: it edits nothing and errors.
func (a *analyzer) sed(name string, args []arg) {
	inPlace := false
	scriptGiven := false
	bsd := a.opt.BSD && name == "sed"
	var ops []arg
	for i := 0; i < len(args); i++ {
		x := args[i]
		l := x.w.lit
		if x.supplied || !strings.HasPrefix(l, "-") || l == "-" {
			ops = append(ops, x)
			continue
		}
		if l == "--in-place" || strings.HasPrefix(l, "--in-place=") {
			inPlace = true
			continue
		}
		if l == "--expression" || l == "--file" {
			scriptGiven = true
			i++
			continue
		}
		if strings.HasPrefix(l, "--") {
			continue
		}
	bundle:
		for j := 1; j < len(l); j++ {
			switch l[j] {
			case 'i':
				inPlace = true
				if bsd && j == len(l)-1 {
					i++ // BSD: the suffix is the next word, even ''
					if i < len(args) && looksLikeSedScript(args[i].w.lit) {
						a.r.Warnings = append(a.r.Warnings, fmt.Sprintf(
							"BSD sed takes %s as the -i backup suffix, not the script; it edits nothing (use sed -i '' ...)",
							args[i].w.raw))
						return
					}
				}
				break bundle // GNU: the rest of the bundle is the suffix
			case 'e', 'f':
				scriptGiven = true
				if j == len(l)-1 {
					i++
				}
				break bundle
			case 'l':
				if j == len(l)-1 {
					i++
				}
				break bundle
			}
		}
	}
	if !inPlace {
		return
	}
	if !scriptGiven && len(ops) > 0 {
		ops = ops[1:]
	}
	a.editFiles(name+" -i", ops)
}

// perl previews perl -i, including the -pie trap: -i swallows the rest of its
// bundle as a backup suffix, so -pie means "suffix e", not "-p -i -e".
func (a *analyzer) perl(args []arg) {
	inPlace := false
	scriptGiven := false
	var ops []arg
	for i := 0; i < len(args); i++ {
		x := args[i]
		l := x.w.lit
		if x.supplied || !strings.HasPrefix(l, "-") || l == "-" || l == "--" {
			if l == "--" {
				ops = append(ops, args[i+1:]...)
				break
			}
			ops = append(ops, x)
			continue
		}
	bundle:
		for j := 1; j < len(l); j++ {
			switch l[j] {
			case 'i':
				inPlace = true
				break bundle
			case 'e', 'E':
				scriptGiven = true
				if j == len(l)-1 {
					i++
				}
				break bundle
			case 'M', 'm', 'I', 'F', 'x', 'C', 'd', 'D':
				break bundle
			}
		}
	}
	if !inPlace {
		return
	}
	if !scriptGiven && len(ops) > 0 {
		ops = ops[1:]
	}
	a.editFiles("perl -i", ops)
}

func (a *analyzer) editFiles(what string, ops []arg) {
	for _, x := range ops {
		ents, ok := a.resolve(x, what)
		if !ok {
			continue
		}
		if len(ents) == 0 && !x.supplied {
			a.miss(verbEdit, "no match for "+x.w.raw)
		}
		for _, e := range ents {
			if e.IsRegular() {
				a.add(verbEdit, e, a.label(e, x.supplied))
			}
		}
	}
}

var symbolicModeRe = regexp.MustCompile(`^-[rwxXst]+$`)

func (a *analyzer) chmod(name string, args []arg) {
	flags, _, ops := flagged(args, nil)
	// A symbolic mode like -x looks like a flag; chmod takes it as the mode.
	if name == "chmod" && len(ops) > 0 && !ops[0].supplied {
		for _, f := range flags {
			if symbolicModeRe.MatchString(f) {
				ops = append([]arg{{w: word{lit: f}}}, ops...)
				break
			}
		}
	}
	recursive := hasFlag(flags, "R", "--recursive")
	hasRef := hasFlag(flags, "", "--reference")
	if !hasRef {
		if len(ops) == 0 || ops[0].supplied {
			return
		}
		ops = ops[1:] // the mode or owner
	}
	verb := verbMode
	if name != "chmod" {
		verb = verbOwner
	}
	for _, x := range ops {
		ents, ok := a.resolve(x, name)
		if !ok {
			continue
		}
		for _, e := range ents {
			if recursive {
				a.addTree(verb, e, a.label(e, x.supplied))
			} else {
				a.add(verb, e, a.label(e, x.supplied))
			}
		}
	}
}

// simpleWrite handles commands whose operands are files they overwrite.
func (a *analyzer) simpleWrite(name string, args []arg, verb string, valued map[string]bool) {
	flags, values, ops := flagged(args, valued)
	if name == "tee" && hasFlag(flags, "a", "--append") {
		return
	}
	if name == "truncate" {
		size := values["-s"]
		for _, f := range flags {
			if strings.HasPrefix(f, "-s") && len(f) > 2 {
				size = f[2:]
			}
		}
		if strings.HasPrefix(size, "+") || strings.HasPrefix(size, ">") {
			return // growing, not truncating
		}
	}
	for _, x := range ops {
		ents, ok := a.resolve(x, name)
		if !ok {
			continue
		}
		for _, e := range ents {
			if e.IsRegular() {
				a.add(verb, e, a.label(e, x.supplied))
			}
		}
	}
}

func (a *analyzer) dd(args []arg) {
	for _, x := range args {
		if !strings.HasPrefix(x.w.lit, "of=") {
			continue
		}
		target := strings.TrimPrefix(x.w.lit, "of=")
		if strings.HasPrefix(target, "/dev/") {
			a.unknown("dd "+x.w.raw, "writes to a device")
			return
		}
		w := x.w
		w.lit, w.pat, w.glob = target, escapeGlob(target), false
		if ents, ok := a.resolve(arg{w: w}, "dd"); ok && len(ents) == 1 && ents[0].IsRegular() {
			a.add(verbOverwrite, ents[0], a.sb.Rel(ents[0].Real))
		}
	}
}

// git previews the working-tree-destroying subcommands.
func (a *analyzer) git(args []arg) {
	i := 0
	for i < len(args) {
		l := args[i].w.lit
		switch {
		case l == "-C":
			if i+1 < len(args) && args[i+1].w.lit != "." {
				a.unknown("git -C "+args[i+1].w.raw, "runs in another directory")
				return
			}
			i += 2
		case l == "-c":
			i += 2
		case strings.HasPrefix(l, "--git-dir") || strings.HasPrefix(l, "--work-tree"):
			a.unknown("git "+l, "runs against another repository")
			return
		case strings.HasPrefix(l, "-"):
			i++
		default:
			goto sub
		}
	}
	return
sub:
	sub := args[i].w.lit
	flags, _, ops := flagged(args[i+1:], nil)

	var (
		verb    string
		scoped  bool // pathspecs limit the effect; else the whole repo
		untrack bool // acts on untracked files
		dirs    bool
		what    = "git " + sub
	)
	switch sub {
	case "reset":
		if !hasFlag(flags, "", "--hard") {
			return
		}
		verb, what = verbDiscard, "git reset --hard"
	case "checkout":
		dashdash := false
		for _, x := range args[i+1:] {
			if x.w.lit == "--" {
				dashdash = true
			}
		}
		force := hasFlag(flags, "f", "--force")
		switch {
		case dashdash || len(ops) == 1 && ops[0].w.lit == ".":
			verb, scoped = verbDiscard, true
		case force:
			verb = verbDiscard
		default:
			return
		}
	case "restore":
		if hasFlag(flags, "S", "--staged") && !hasFlag(flags, "W", "--worktree") {
			return
		}
		verb, scoped = verbDiscard, true
	case "clean":
		if !hasFlag(flags, "f", "--force") || hasFlag(flags, "n", "--dry-run") {
			return
		}
		verb, scoped, untrack = verbDelete, true, true
		dirs = hasFlag(flags, "d", "")
		if hasFlag(flags, "xX", "") {
			eff := a.effect(verbDelete)
			eff.AtLeast = true
			eff.Notes = append(eff.Notes, "plus ignored files, not counted")
		}
	case "stash":
		if len(ops) > 0 && (ops[0].w.lit == "clear" || ops[0].w.lit == "drop") {
			a.unknown("git stash "+ops[0].w.lit, "discards stashed work")
		}
		return
	default:
		return
	}

	if a.dir.rel != "" || a.dir.bad != "" {
		a.unknown(what, "after cd")
		return
	}
	changes, ok := a.gitStatus()
	if !ok {
		a.unknown(what, "git status unavailable")
		return
	}

	var specs []*regexp.Regexp
	for _, x := range ops {
		if x.supplied || x.w.dynamic != "" {
			a.unknown(what+" "+x.w.raw, "pathspec not previewable")
			return
		}
		if x.w.lit == "." {
			continue
		}
		spec := strings.TrimSuffix(strings.TrimPrefix(x.w.lit, "./"), "/")
		re, err := glob.Compile(spec, glob.Options{CrossSlash: true})
		if err != nil {
			a.unknown(what+" "+x.w.raw, "pathspec not previewable")
			return
		}
		specs = append(specs, re)
	}
	inScope := func(p string) bool {
		if !scoped {
			return true
		}
		if strings.HasPrefix(p, "../") {
			return false // pathspecs are relative to the cwd
		}
		if len(specs) == 0 {
			return true
		}
		for _, re := range specs {
			if re.MatchString(strings.TrimSuffix(p, "/")) {
				return true
			}
			for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
				if re.MatchString(d) {
					return true
				}
			}
		}
		return false
	}

	for _, c := range changes {
		if c.Untracked() != untrack || !inScope(c.Path) {
			continue
		}
		if untrack {
			isDir := strings.HasSuffix(c.Path, "/")
			if isDir && !dirs {
				continue
			}
			ents, ok := a.resolveQuiet(word{lit: c.Path, pat: escapeGlob(strings.TrimSuffix(c.Path, "/")), raw: c.Path})
			if !ok || len(ents) != 1 {
				continue
			}
			a.addTree(verb, ents[0], a.sb.Rel(ents[0].Real))
			continue
		}
		label := "./" + c.Path
		if strings.HasPrefix(c.Path, "../") {
			label = c.Path
		}
		if ents, ok := a.resolveQuiet(word{lit: c.Path, pat: escapeGlob(c.Path), raw: c.Path}); ok && len(ents) == 1 {
			a.add(verb, ents[0], label)
		} else {
			// A deleted file has no entry, but restoring it is still a change.
			a.add(verb, probe.Entry{Real: "git:" + c.Path}, label)
		}
	}
	a.effect(verb)
}

func (a *analyzer) gitStatus() ([]probe.GitChange, bool) {
	if !a.gitLoaded {
		a.gitLoaded = true
		changes, ok, err := a.sb.GitStatus(a.ctx)
		a.gitChanges, a.gitOK = changes, ok && err == nil
	}
	return a.gitChanges, a.gitOK
}

// report assembles effects in a stable order.
func (a *analyzer) report() *Report {
	for _, v := range verbOrder {
		if e, ok := a.effects[v]; ok {
			a.r.Effects = append(a.r.Effects, e)
		}
	}
	return &a.r
}

// String renders the report as the one line shown under the prompt, or "".
func (r *Report) String() string {
	if r.Empty() {
		return ""
	}
	var parts []string
	parts = append(parts, r.Warnings...)
	for _, e := range r.Effects {
		parts = append(parts, e.String())
	}
	if len(r.Unknown) > 0 {
		parts = append(parts, "can't preview "+strings.Join(r.Unknown, ", "))
	}
	return "⚠ " + strings.Join(parts, "; ")
}

func (e *Effect) String() string {
	n := e.Files + e.Dirs
	if n == 0 {
		s := e.Verb + " nothing"
		if len(e.Misses) > 0 {
			s += " (" + strings.Join(e.Misses, ", ") + ")"
		}
		if len(e.Notes) > 0 {
			s += " — " + strings.Join(e.Notes, "; ")
		}
		return s
	}

	var b strings.Builder
	b.WriteString(e.Verb)
	b.WriteString(" ")
	if e.AtLeast {
		b.WriteString("at least ")
	}
	var counts []string
	if e.Files > 0 {
		counts = append(counts, fmt.Sprintf("%d %s", e.Files, plural(e.Files, "file", "files")))
	}
	if e.Dirs > 0 {
		counts = append(counts, fmt.Sprintf("%d %s", e.Dirs, plural(e.Dirs, "dir", "dirs")))
	}
	b.WriteString(strings.Join(counts, " and "))
	if e.Bytes > 0 {
		fmt.Fprintf(&b, " (%s)", probe.HumanSize(e.Bytes))
	}

	if e.To != "" {
		b.WriteString(" to " + e.To)
	} else if len(e.labels) > 0 {
		labels := append([]string(nil), e.labels...)
		sort.SliceStable(labels, func(i, j int) bool { return e.counts[labels[i]] > e.counts[labels[j]] })
		shown := labels
		if len(shown) > 3 {
			shown = shown[:3]
		}
		var ls []string
		for _, l := range shown {
			if len(labels) > 1 && e.counts[l] > 1 {
				ls = append(ls, fmt.Sprintf("%s (%d)", l, e.counts[l]))
			} else {
				ls = append(ls, l)
			}
		}
		b.WriteString(" — " + strings.Join(ls, ", "))
		if more := len(labels) - len(shown); more > 0 {
			fmt.Fprintf(&b, " +%d more", more)
		}
	}
	if len(e.Misses) > 0 {
		b.WriteString("; " + strings.Join(e.Misses, ", "))
	}
	for _, note := range e.Notes {
		b.WriteString("; " + note)
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
