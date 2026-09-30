package impact

import (
	"fmt"
	"strings"
)

// This is a lexer for the subset of POSIX/zsh syntax that one-liners use. It
// does not try to be a shell: its job is to recover, for each simple command,
// the argument words and the redirections, and to say honestly when a word's
// value cannot be known without running something. A word containing $VAR,
// $(...), backticks, brace expansion or a glob qualifier is marked dynamic and
// the analyzer reports it as unpreviewable rather than guessing.

// word is one shell word after quote removal.
type word struct {
	lit     string // value with quotes removed
	pat     string // same, as a glob pattern: quoted metacharacters backslash-escaped
	glob    bool   // contains an unquoted *, ? or [
	dynamic string // non-empty when the value depends on expansion; says which kind
	tilde   bool   // begins with an unquoted ~ that expands to $HOME
	raw     string // source text, for messages
}

// redirect is one redirection attached to a simple command.
type redirect struct {
	op     string // >, >>, >|, &>, &>>, <, <<, <<<, <>, >&, <&
	target word
}

// simpleCmd is one command in a pipeline.
type simpleCmd struct {
	words  []word
	redirs []redirect
}

// item is one step of a command line: a pipeline, or the opening or closing
// of a subshell, which scopes any cd inside it.
type item struct {
	pipeline []simpleCmd
	open     bool
	close    bool
}

type lexer struct {
	s   string
	i   int
	out []item
	cur []simpleCmd
	cmd simpleCmd
}

// parse splits a command line into items.
func parse(line string) ([]item, error) {
	l := &lexer{s: line}
	if err := l.run(); err != nil {
		return nil, err
	}
	return l.out, nil
}

func (l *lexer) endCmd() {
	if len(l.cmd.words) > 0 || len(l.cmd.redirs) > 0 {
		l.cur = append(l.cur, l.cmd)
	}
	l.cmd = simpleCmd{}
}

func (l *lexer) endPipeline() {
	l.endCmd()
	if len(l.cur) > 0 {
		l.out = append(l.out, item{pipeline: l.cur})
	}
	l.cur = nil
}

func (l *lexer) peek(n int) string {
	if l.i+n > len(l.s) {
		return l.s[l.i:]
	}
	return l.s[l.i : l.i+n]
}

var redirOps = []string{"&>>", "<<<", "&>", ">>", ">|", ">&", "<&", "<<", "<>", ">", "<"}

func (l *lexer) run() error {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			l.i++
		case c == '#':
			return l.finish() // a comment runs to end of line
		case l.peek(2) == "&&" || l.peek(2) == "||":
			l.i += 2
			l.endPipeline()
		case l.peek(2) == "|&":
			l.i += 2
			l.endCmd()
		case c == '|':
			l.i++
			l.endCmd()
		case c == ';' || (c == '&' && l.peek(2) != "&>"):
			l.i++
			l.endPipeline()
		case c == '(':
			l.i++
			l.endPipeline()
			l.out = append(l.out, item{open: true})
		case c == ')':
			l.i++
			l.endPipeline()
			l.out = append(l.out, item{close: true})
		default:
			if op := l.redirOp(); op != "" {
				t, err := l.word()
				if err != nil {
					return err
				}
				l.cmd.redirs = append(l.cmd.redirs, redirect{op: op, target: t})
				continue
			}
			w, err := l.word()
			if err != nil {
				return err
			}
			// An all-digit word directly followed by a redirection is its fd.
			if l.i < len(l.s) && isAllDigits(w.raw) && (l.s[l.i] == '>' || l.s[l.i] == '<') {
				continue
			}
			l.cmd.words = append(l.cmd.words, w)
		}
	}
	return l.finish()
}

func (l *lexer) finish() error {
	l.endPipeline()
	return nil
}

// redirOp consumes a redirection operator and the whitespace after it.
func (l *lexer) redirOp() string {
	for _, op := range redirOps {
		if strings.HasPrefix(l.s[l.i:], op) {
			l.i += len(op)
			for l.i < len(l.s) && (l.s[l.i] == ' ' || l.s[l.i] == '\t') {
				l.i++
			}
			return op
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isMeta(c byte) bool { return c == '*' || c == '?' || c == '[' }

// word consumes one word.
func (l *lexer) word() (word, error) {
	var w word
	var lit, pat strings.Builder
	start := l.i

	quoted := func(s string) {
		lit.WriteString(s)
		for i := 0; i < len(s); i++ {
			if isMeta(s[i]) || s[i] == '\\' {
				pat.WriteByte('\\')
			}
			pat.WriteByte(s[i])
		}
	}
	dyn := func(kind string) {
		if w.dynamic == "" {
			w.dynamic = kind
		}
	}

	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '|' || c == '<' || c == '>' || c == ')':
			goto done
		case c == '&':
			goto done
		case c == '(':
			if l.i == start {
				goto done
			}
			// Mid-word parens are a zsh glob qualifier such as *(.om[1,3]).
			if err := l.skipBalanced('(', ')'); err != nil {
				return w, err
			}
			dyn("glob qualifier")
		case c == '\\':
			if l.i+1 < len(l.s) {
				quoted(string(l.s[l.i+1]))
				l.i += 2
			} else {
				l.i++
			}
		case c == '\'':
			end := strings.IndexByte(l.s[l.i+1:], '\'')
			if end < 0 {
				return w, fmt.Errorf("unterminated single quote")
			}
			quoted(l.s[l.i+1 : l.i+1+end])
			l.i += end + 2
		case c == '"':
			l.i++
			for {
				if l.i >= len(l.s) {
					return w, fmt.Errorf("unterminated double quote")
				}
				d := l.s[l.i]
				if d == '"' {
					l.i++
					break
				}
				if d == '\\' && l.i+1 < len(l.s) && strings.IndexByte("$`\"\\\n", l.s[l.i+1]) >= 0 {
					quoted(string(l.s[l.i+1]))
					l.i += 2
					continue
				}
				if d == '$' || d == '`' {
					kind, err := l.expansion()
					if err != nil {
						return w, err
					}
					dyn(kind)
					continue
				}
				quoted(string(d))
				l.i++
			}
		case c == '$' && l.peek(2) == "$'":
			// ANSI-C quoting: static, and rare enough that its escapes are
			// taken literally.
			end := strings.IndexByte(l.s[l.i+2:], '\'')
			if end < 0 {
				return w, fmt.Errorf("unterminated $' quote")
			}
			quoted(l.s[l.i+2 : l.i+2+end])
			l.i += end + 3
		case c == '$' || c == '`':
			kind, err := l.expansion()
			if err != nil {
				return w, err
			}
			dyn(kind)
		case c == '~' && l.i == start:
			rest := l.s[l.i+1:]
			if rest == "" || rest[0] == '/' || rest[0] == ' ' {
				w.tilde = true
				lit.WriteByte('~')
				pat.WriteByte('~')
				l.i++
				continue
			}
			dyn("~user")
			l.i++
		case c == '{':
			// Brace expansion ({a,b} or {1..3}) multiplies the word; a lone
			// { or {} (find's placeholder) is literal.
			end := strings.IndexByte(l.s[l.i:], '}')
			if end > 0 {
				inner := l.s[l.i+1 : l.i+end]
				if strings.Contains(inner, ",") || strings.Contains(inner, "..") {
					dyn("brace expansion")
				}
			}
			lit.WriteByte(c)
			pat.WriteString(`\{`)
			l.i++
		default:
			if isMeta(c) {
				w.glob = true
			}
			lit.WriteByte(c)
			pat.WriteByte(c)
			l.i++
		}
	}
done:
	w.lit, w.pat, w.raw = lit.String(), pat.String(), l.s[start:l.i]
	return w, nil
}

// expansion consumes $name, ${...}, $(...), $((...)) or `...` and names it.
func (l *lexer) expansion() (string, error) {
	if l.s[l.i] == '`' {
		end := strings.IndexByte(l.s[l.i+1:], '`')
		if end < 0 {
			return "", fmt.Errorf("unterminated backquote")
		}
		l.i += end + 2
		return "command substitution", nil
	}
	l.i++ // the $
	if l.i >= len(l.s) {
		return "variable", nil
	}
	switch c := l.s[l.i]; {
	case c == '(':
		return "command substitution", l.skipBalanced('(', ')')
	case c == '{':
		return "variable", l.skipBalanced('{', '}')
	case strings.IndexByte("?#@*$!-0123456789", c) >= 0:
		l.i++
	default:
		for l.i < len(l.s) {
			c := l.s[l.i]
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				break
			}
			l.i++
		}
	}
	return "variable", nil
}

// skipBalanced consumes a bracketed span, honouring nesting and quotes.
func (l *lexer) skipBalanced(open, close byte) error {
	depth := 0
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch c {
		case '\\':
			l.i++
		case '\'':
			end := strings.IndexByte(l.s[l.i+1:], '\'')
			if end < 0 {
				return fmt.Errorf("unterminated single quote")
			}
			l.i += end + 1
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				l.i++
				return nil
			}
		}
		l.i++
	}
	return fmt.Errorf("unbalanced %c", open)
}
