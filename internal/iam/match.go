package iam

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// globMatch reports whether s matches pattern, where '*' matches any
// sequence of characters (including none) and '?' exactly one character.
// fold enables case-insensitive comparison. When escaped is true a backslash
// in pattern makes the following character literal (used for patterns
// produced by expandVars).
//
// The algorithm backtracks only to the most recent '*', so it runs in
// O(len(pattern)*len(s)) time and constant memory even for hostile input.
func globMatch(pattern, s string, fold, escaped bool) bool {
	p, i := 0, 0
	starP, starI := -1, 0
	for i < len(s) {
		if p < len(pattern) {
			c, size := utf8.DecodeRuneInString(pattern[p:])
			switch {
			case c == '*':
				p += size
				starP, starI = p, i
				continue
			case c == '?':
				_, sz := utf8.DecodeRuneInString(s[i:])
				p += size
				i += sz
				continue
			default:
				lit, lsize := c, size
				if escaped && c == '\\' && p+size < len(pattern) {
					var n int
					lit, n = utf8.DecodeRuneInString(pattern[p+size:])
					lsize = size + n
				}
				r, sz := utf8.DecodeRuneInString(s[i:])
				if runeEqual(lit, r, fold) {
					p += lsize
					i += sz
					continue
				}
			}
		}
		if starP < 0 {
			return false
		}
		// Let the last star absorb one more character and retry.
		_, sz := utf8.DecodeRuneInString(s[starI:])
		starI += sz
		i = starI
		p = starP
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func runeEqual(a, b rune, fold bool) bool {
	if a == b {
		return true
	}
	return fold && unicode.ToLower(a) == unicode.ToLower(b)
}

// varLookup resolves a policy variable name (e.g. "aws:username").
type varLookup func(name string) (string, bool)

// expandVars substitutes IAM policy variables in s: ${aws:username},
// ${aws:userid}, any condition context key (${s3:prefix}, ...), the escapes
// ${*}, ${?} and ${$}, and defaults (${aws:username, 'nobody'}).
//
// When pattern is true the result is a glob pattern for globMatch with
// escaped=true: substituted values are matched literally while '*' and '?'
// written in s keep their wildcard meaning. Otherwise the result is the plain
// substituted string. ok is false if a variable cannot be resolved and has no
// default; AWS then treats the element as not matching.
func expandVars(s string, lookup varLookup, pattern bool) (string, bool) {
	if !strings.Contains(s, "${") {
		if pattern && strings.IndexByte(s, '\\') >= 0 {
			return strings.ReplaceAll(s, `\`, `\\`), true
		}
		return s, true
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	writeLit := func(v string) {
		if !pattern {
			b.WriteString(v)
			return
		}
		for _, r := range v {
			if r == '\\' || r == '*' || r == '?' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
	}
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end >= 0 {
				inner := s[i+2 : i+2+end]
				switch inner {
				case "*", "?", "$":
					writeLit(inner)
				default:
					name, def, hasDef := parseVarSpec(inner)
					v, ok := lookup(name)
					if !ok {
						if !hasDef {
							return "", false
						}
						v = def
					}
					writeLit(v)
				}
				i += end + 3
				continue
			}
		}
		if pattern && s[i] == '\\' {
			b.WriteString(`\\`)
		} else {
			b.WriteByte(s[i])
		}
		i++
	}
	return b.String(), true
}

// parseVarSpec splits "key, 'default'" into its parts.
func parseVarSpec(inner string) (name, def string, hasDef bool) {
	name, rest, found := strings.Cut(inner, ",")
	name = strings.TrimSpace(name)
	if !found {
		return name, "", false
	}
	rest = strings.TrimSpace(rest)
	if len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'' {
		return name, rest[1 : len(rest)-1], true
	}
	return strings.TrimSpace(inner), "", false
}

// matchPattern matches subject against a policy pattern that may contain
// wildcards and policy variables.
func matchPattern(pat, subject string, fold bool, lookup varLookup) bool {
	if !strings.Contains(pat, "${") {
		return globMatch(pat, subject, fold, false)
	}
	exp, ok := expandVars(pat, lookup, true)
	if !ok {
		return false
	}
	return globMatch(exp, subject, fold, true)
}
