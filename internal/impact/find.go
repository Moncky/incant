package impact

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/callumscott/incant/internal/glob"
	"github.com/callumscott/incant/internal/probe"
)

// findCmd is a parsed find(1) invocation.
type findCmd struct {
	roots    []word
	expr     node
	maxDepth int // -1 when unset
	minDepth int
	depth    bool // post-order: -depth, or implied by -delete
	// execs are the -exec/-execdir/-ok actions, in order; a node refers to
	// one by index.
	execs [][]word
	// printed says whether matches reach stdout for a following pipe stage:
	// -print, -print0, or the implicit -print of an action-free expression.
	printed bool
}

// node is a find expression.
type node interface {
	match(f *findEval, e probe.Entry, display string, depth int) bool
}

type (
	andNode  struct{ a, b node }
	orNode   struct{ a, b node }
	notNode  struct{ a node }
	trueNode struct{ v bool }
	testNode struct {
		fn func(f *findEval, e probe.Entry, display string, depth int) bool
	}
	actionNode struct {
		kind string // "delete", "exec", "print", "prune"
		exec int
	}
)

func (n andNode) match(f *findEval, e probe.Entry, d string, dep int) bool {
	return n.a.match(f, e, d, dep) && n.b.match(f, e, d, dep)
}
func (n orNode) match(f *findEval, e probe.Entry, d string, dep int) bool {
	return n.a.match(f, e, d, dep) || n.b.match(f, e, d, dep)
}
func (n notNode) match(f *findEval, e probe.Entry, d string, dep int) bool {
	return !n.a.match(f, e, d, dep)
}
func (n trueNode) match(*findEval, probe.Entry, string, int) bool { return n.v }
func (n testNode) match(f *findEval, e probe.Entry, d string, dep int) bool {
	return n.fn(f, e, d, dep)
}
func (n actionNode) match(f *findEval, e probe.Entry, _ string, _ int) bool {
	switch n.kind {
	case "delete":
		f.deleted = append(f.deleted, e)
	case "exec":
		f.execd[n.exec] = append(f.execd[n.exec], e)
	case "print":
		f.printed = append(f.printed, e)
	case "prune":
		if e.IsDir() && !f.cmd.depth {
			f.pruned = true
		}
	}
	return true
}

// findEval accumulates what each action fired on during one walk.
type findEval struct {
	cmd     *findCmd
	now     time.Time
	bsd     bool
	deleted []probe.Entry
	execd   map[int][]probe.Entry
	printed []probe.Entry
	pruned  bool

	stat     func(word) (probe.Entry, error)
	refCache map[string]time.Time
}

// unsupportedErr names a find feature the evaluator does not model.
type unsupportedErr struct{ what string }

func (e unsupportedErr) Error() string { return e.what }

// parseFind parses find's arguments (everything after the command name).
func parseFind(args []word, now time.Time, bsd bool) (*findCmd, error) {
	fc := &findCmd{maxDepth: -1}
	i := 0
	// Leading BSD/GNU options.
	for i < len(args) {
		switch args[i].lit {
		case "-H", "-P", "-E", "-X", "-s", "-x", "-d":
			if args[i].lit == "-d" {
				fc.depth = true
			}
			i++
			continue
		case "-L":
			return nil, unsupportedErr{"find -L (follows symlinks)"}
		}
		break
	}
	for i < len(args) {
		l := args[i].lit
		if strings.HasPrefix(l, "-") || l == "(" || l == "!" || l == ")" {
			break
		}
		if args[i].dynamic != "" {
			return nil, unsupportedErr{fmt.Sprintf("find root %s (%s)", args[i].raw, args[i].dynamic)}
		}
		fc.roots = append(fc.roots, args[i])
		i++
	}
	if len(fc.roots) == 0 {
		fc.roots = []word{{lit: ".", pat: ".", raw: "."}}
	}

	p := &findParser{args: args[i:], fc: fc, now: now, bsd: bsd}
	var expr node = trueNode{true}
	if len(p.args) > 0 {
		var err error
		if expr, err = p.or(); err != nil {
			return nil, err
		}
		if p.i < len(p.args) {
			return nil, unsupportedErr{"find expression near " + p.args[p.i].raw}
		}
	}
	if !p.hasAction {
		expr = andNode{expr, actionNode{kind: "print"}}
		fc.printed = true
	}
	fc.expr = expr
	return fc, nil
}

type findParser struct {
	args      []word
	i         int
	fc        *findCmd
	now       time.Time
	bsd       bool
	hasAction bool
}

func (p *findParser) peek() string {
	if p.i < len(p.args) {
		return p.args[p.i].lit
	}
	return ""
}

func (p *findParser) or() (node, error) {
	a, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peek() == "-o" || p.peek() == "-or" {
		p.i++
		b, err := p.and()
		if err != nil {
			return nil, err
		}
		a = orNode{a, b}
	}
	return a, nil
}

func (p *findParser) and() (node, error) {
	a, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.i < len(p.args) {
		switch p.peek() {
		case "-o", "-or", ")":
			return a, nil
		case "-a", "-and":
			p.i++
		}
		b, err := p.unary()
		if err != nil {
			return nil, err
		}
		a = andNode{a, b}
	}
	return a, nil
}

func (p *findParser) unary() (node, error) {
	switch p.peek() {
	case "!", "-not":
		p.i++
		a, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{a}, nil
	case "(":
		p.i++
		a, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, unsupportedErr{"unbalanced ( in find expression"}
		}
		p.i++
		return a, nil
	}
	return p.primary()
}

func (p *findParser) arg(flag string) (word, error) {
	if p.i >= len(p.args) {
		return word{}, unsupportedErr{"find " + flag + " without an argument"}
	}
	w := p.args[p.i]
	p.i++
	if w.dynamic != "" {
		return w, unsupportedErr{fmt.Sprintf("find %s %s (%s)", flag, w.raw, w.dynamic)}
	}
	return w, nil
}

func (p *findParser) primary() (node, error) {
	if p.i >= len(p.args) {
		return nil, unsupportedErr{"incomplete find expression"}
	}
	flag := p.args[p.i].lit
	p.i++

	switch flag {
	case "-true":
		return trueNode{true}, nil
	case "-false":
		return trueNode{false}, nil
	case "-xdev", "-mount", "-noleaf", "-ignore_readdir_race", "-daystart", "-follow":
		if flag == "-follow" {
			return nil, unsupportedErr{"find -follow (follows symlinks)"}
		}
		if flag == "-daystart" {
			return nil, unsupportedErr{"find -daystart"}
		}
		return trueNode{true}, nil
	case "-depth":
		p.fc.depth = true
		return trueNode{true}, nil
	case "-maxdepth", "-mindepth":
		w, err := p.arg(flag)
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(w.lit)
		if err != nil || n < 0 {
			return nil, unsupportedErr{"find " + flag + " " + w.raw}
		}
		if flag == "-maxdepth" {
			p.fc.maxDepth = n
		} else {
			p.fc.minDepth = n
		}
		return trueNode{true}, nil

	case "-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename":
		w, err := p.arg(flag)
		if err != nil {
			return nil, err
		}
		onPath := !strings.HasSuffix(flag, "name") || strings.HasSuffix(flag, "wholename")
		re, err := glob.Compile(w.lit, glob.Options{
			CrossSlash: onPath,
			Fold:       strings.HasPrefix(flag, "-i"),
		})
		if err != nil {
			return nil, unsupportedErr{err.Error()}
		}
		return testNode{func(_ *findEval, e probe.Entry, display string, _ int) bool {
			if onPath {
				return re.MatchString(display)
			}
			return re.MatchString(path.Base(display))
		}}, nil

	case "-type":
		w, err := p.arg(flag)
		if err != nil {
			return nil, err
		}
		kinds := strings.Split(w.lit, ",")
		for _, k := range kinds {
			if k != "f" && k != "d" && k != "l" {
				return nil, unsupportedErr{"find -type " + w.lit}
			}
		}
		return testNode{func(_ *findEval, e probe.Entry, _ string, _ int) bool {
			for _, k := range kinds {
				if (k == "f" && e.IsRegular()) || (k == "d" && e.IsDir()) || (k == "l" && e.IsLink()) {
					return true
				}
			}
			return false
		}}, nil

	case "-empty":
		return testNode{func(_ *findEval, e probe.Entry, _ string, _ int) bool {
			if e.IsDir() {
				return e.Children == 0
			}
			return e.IsRegular() && e.Size == 0
		}}, nil

	case "-mtime", "-mmin":
		w, err := p.arg(flag)
		if err != nil {
			return nil, err
		}
		cmp, n, err := parseNumeric(w.lit)
		if err != nil {
			return nil, unsupportedErr{"find " + flag + " " + w.raw}
		}
		unit := 24 * time.Hour
		if flag == "-mmin" {
			unit = time.Minute
		}
		bsd := p.bsd
		now := p.now
		return testNode{func(_ *findEval, e probe.Entry, _ string, _ int) bool {
			age := float64(now.Sub(e.ModTime)) / float64(unit)
			// GNU discards the fraction; BSD rounds -mtime up to the next
			// whole period. The difference decides whether a file modified
			// 30 hours ago is "-mtime 1".
			var periods float64
			if bsd && flag == "-mtime" {
				periods = math.Ceil(age)
			} else {
				periods = math.Floor(age)
			}
			return compare(cmp, periods, float64(n))
		}}, nil

	case "-size":
		w, err := p.arg(flag)
		if err != nil {
			return nil, err
		}
		cmp, n, unit, err := parseSize(w.lit)
		if err != nil {
			return nil, unsupportedErr{"find -size " + w.raw}
		}
		return testNode{func(_ *findEval, e probe.Entry, _ string, _ int) bool {
			// Sizes round up to whole units, which is why -size -1M only
			// matches empty files.
			units := math.Ceil(float64(e.Size) / float64(unit))
			return compare(cmp, units, float64(n))
		}}, nil

	case "-newer":
		w, err := p.arg(flag)
		if err != nil {
			return nil, err
		}
		ref := w
		return testNode{func(f *findEval, e probe.Entry, _ string, _ int) bool {
			t, ok := f.refTime(ref)
			return ok && e.ModTime.After(t)
		}}, nil

	case "-print", "-print0", "-ls", "-fls", "-printf", "-fprint", "-fprint0", "-fprintf":
		p.hasAction = true
		// Consume the format or file operand; its value does not matter here.
		takes := map[string]int{"-fls": 1, "-printf": 1, "-fprint": 1, "-fprint0": 1, "-fprintf": 2}[flag]
		if p.i+takes > len(p.args) {
			return nil, unsupportedErr{"find " + flag + " without an argument"}
		}
		p.i += takes
		if flag == "-print" || flag == "-print0" {
			p.fc.printed = true
			return actionNode{kind: "print"}, nil
		}
		return trueNode{true}, nil

	case "-delete":
		p.hasAction = true
		p.fc.depth = true
		return actionNode{kind: "delete"}, nil

	case "-prune":
		return actionNode{kind: "prune"}, nil

	case "-exec", "-execdir", "-ok", "-okdir":
		p.hasAction = true
		var cmd []word
		for p.i < len(p.args) {
			w := p.args[p.i]
			p.i++
			if w.lit == ";" || (w.lit == "+" && len(cmd) > 0 && cmd[len(cmd)-1].lit == "{}") {
				p.fc.execs = append(p.fc.execs, cmd)
				return actionNode{kind: "exec", exec: len(p.fc.execs) - 1}, nil
			}
			cmd = append(cmd, w)
		}
		return nil, unsupportedErr{"find " + flag + " without a terminating ;"}
	}

	return nil, unsupportedErr{"find " + flag}
}

// refTime resolves a -newer reference file, once per walk.
func (f *findEval) refTime(w word) (time.Time, bool) {
	if t, ok := f.refCache[w.pat]; ok {
		return t, !t.IsZero()
	}
	var t time.Time
	if f.stat != nil {
		if e, err := f.stat(w); err == nil {
			t = e.ModTime
		}
	}
	f.refCache[w.pat] = t
	return t, !t.IsZero()
}

func parseNumeric(s string) (byte, int, error) {
	cmp := byte('=')
	if s != "" && (s[0] == '+' || s[0] == '-') {
		cmp, s = s[0], s[1:]
	}
	n, err := strconv.Atoi(s)
	return cmp, n, err
}

var sizeRe = regexp.MustCompile(`^([+-]?)(\d+)([ckMGTPbw]?)$`)

func parseSize(s string) (byte, int, int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("bad size")
	}
	cmp := byte('=')
	if m[1] != "" {
		cmp = m[1][0]
	}
	n, _ := strconv.Atoi(m[2])
	unit := map[string]int64{
		"": 512, "b": 512, "c": 1, "w": 2, "k": 1 << 10,
		"M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "P": 1 << 50,
	}[m[3]]
	return cmp, n, unit, nil
}

func compare(cmp byte, v, n float64) bool {
	switch cmp {
	case '+':
		return v > n
	case '-':
		return v < n
	}
	return v == n
}
