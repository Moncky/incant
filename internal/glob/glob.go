// Package glob compiles shell and find(1) wildcard patterns to regexps.
//
// Two callers need the same matcher with slightly different rules: shell
// pathname expansion matches one path segment at a time, and find's -path
// lets * cross slashes. Both treat a backslash as escaping the next character,
// which is how the command lexer marks metacharacters that were quoted in the
// original command and must match literally.
package glob

import (
	"fmt"
	"regexp"
	"strings"
)

// HasMeta reports whether pattern contains an unescaped wildcard.
func HasMeta(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '*', '?', '[':
			return true
		}
	}
	return false
}

// Unescape removes backslash escapes, yielding the literal the pattern names.
func Unescape(pattern string) string {
	if !strings.Contains(pattern, `\`) {
		return pattern
	}
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			i++
		}
		b.WriteByte(pattern[i])
	}
	return b.String()
}

// Options selects matching rules.
type Options struct {
	// CrossSlash lets * and ? match '/', as find's -path does.
	CrossSlash bool
	// Fold matches case-insensitively, as -iname and -ipath do.
	Fold bool
}

// Compile turns a wildcard pattern into an anchored regexp.
func Compile(pattern string, opt Options) (*regexp.Regexp, error) {
	var b strings.Builder
	if opt.Fold {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	any := "[^/]"
	if opt.CrossSlash {
		any = "(?s:.)"
	}

	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '\\':
			if i+1 < len(pattern) {
				i++
				b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			} else {
				b.WriteString(`\\`)
			}
		case '*':
			b.WriteString(any + "*")
		case '?':
			b.WriteString(any)
		case '[':
			class, n, ok := bracket(pattern[i:])
			if !ok {
				// An unterminated [ is a literal, as in the shell.
				b.WriteString(`\[`)
				continue
			}
			b.WriteString(class)
			i += n - 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", pattern, err)
	}
	return re, nil
}

// bracket translates a [...] expression starting at s[0]. It returns the regexp
// class, the number of pattern bytes consumed, and false when s holds no
// closing bracket.
func bracket(s string) (string, int, bool) {
	i := 1
	var b strings.Builder
	b.WriteString("[")
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		b.WriteString("^")
		i++
	}
	// A ] straight after the opening (or its negation) is a literal member.
	first := true
	for i < len(s) {
		c := s[i]
		switch {
		case c == ']' && !first:
			b.WriteString("]")
			return b.String(), i + 1, true
		case c == '[' && i+1 < len(s) && s[i+1] == ':':
			end := strings.Index(s[i:], ":]")
			if end < 0 {
				return "", 0, false
			}
			b.WriteString(s[i : i+end+2])
			i += end + 2
		case c == '\\' && i+1 < len(s):
			b.WriteString(regexp.QuoteMeta(string(s[i+1])))
			i += 2
		case c == '\\' || c == ']' || c == '[' || c == '^':
			b.WriteString(`\` + string(c))
			i++
		default:
			b.WriteByte(c)
			i++
		}
		first = false
	}
	return "", 0, false
}
