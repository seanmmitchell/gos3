package iam

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on the work of evaluating one authorization request. Request
// values that policies match or substitute (aws:UserAgent, aws:Referer,
// s3:prefix, ...) come from the client, so without them one request could
// make a stored policy expand variables into gigabytes or glob-match for
// minutes. Exceeding one denies the request (see Manager.authorize).
const (
	// maxVarRefs is the most policy variable references (${...}) a single
	// Resource or Condition value may contain. Validation refuses
	// documents with more, and evaluation fails closed on them.
	maxVarRefs = 16
	// maxExpandBytes bounds the bytes policy variable expansion may produce
	// while evaluating one request.
	maxExpandBytes = 64 << 10
	// maxGlobSteps bounds the steps globMatch may take while evaluating one
	// request (a few tens of milliseconds of CPU).
	maxGlobSteps = 8 << 20
)

// MaxRequestValueLen is the most bytes of a client-supplied free-form
// request value (User-Agent, Referer, s3:prefix, s3:delimiter) that callers
// put in a condition context; see TruncateRequestValue. Nothing legitimate
// needs more (an S3 key, and so a useful prefix, is at most 1024 bytes).
const MaxRequestValueLen = 1024

// TruncateRequestValue cuts v to at most MaxRequestValueLen bytes, at a
// UTF-8 character boundary. Callers building a condition context apply it
// to free-form client values, which policies glob-match and substitute.
func TruncateRequestValue(v string) string {
	if len(v) <= MaxRequestValueLen {
		return v
	}
	i := MaxRequestValueLen
	for i > 0 && !utf8.RuneStart(v[i]) {
		i--
	}
	return v[:i]
}

// budget meters the work of evaluating one request against the limits
// above. exceeded is set once any limit is hit; the result of the
// operation that hit it is then meaningless and the request must be
// denied. A nil *budget is unlimited.
type budget struct {
	expand   int // bytes of expansion output left
	glob     int // glob steps left
	exceeded bool
}

func newBudget() *budget { return &budget{expand: maxExpandBytes, glob: maxGlobSteps} }

// fail marks b exceeded.
func (b *budget) fail() {
	if b != nil {
		b.exceeded = true
	}
}

// globMatch reports whether s matches pattern, where '*' matches any
// sequence of characters (including none) and '?' exactly one character.
// fold enables case-insensitive comparison. When escaped is true a backslash
// in pattern makes the following character literal (used for patterns
// produced by expandVars).
//
// It uses constant memory and backtracks only to the most recent '*', which
// avoids exponential blowup but still takes O(len(pattern)*len(s)) steps in
// the worst case (e.g. "*aaa…ab" against "aaa…a"), so callers matching
// client-supplied subjects use globMatchBudget.
func globMatch(pattern, s string, fold, escaped bool) bool {
	return globMatchBudget(pattern, s, fold, escaped, nil)
}

// globMatchBudget is globMatch charging each step to b. It returns false
// and marks b exceeded when b runs out.
func globMatchBudget(pattern, s string, fold, escaped bool, b *budget) bool {
	if b != nil && b.exceeded {
		return false
	}
	p, i := 0, 0
	starP, starI := -1, 0
	for i < len(s) {
		if b != nil {
			if b.glob--; b.glob < 0 {
				b.exceeded = true
				return false
			}
		}
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
//
// The output is charged to b: ok is false and b is marked exceeded when s
// has more than maxVarRefs references or the output exceeds b's
// allowance.
func expandVars(s string, lookup varLookup, pattern bool, b *budget) (string, bool) {
	if !strings.Contains(s, "${") {
		if pattern && strings.IndexByte(s, '\\') >= 0 {
			return strings.ReplaceAll(s, `\`, `\\`), true
		}
		return s, true
	}
	if strings.Count(s, "${") > maxVarRefs {
		b.fail()
		return "", false
	}
	limit := -1 // unlimited
	if b != nil {
		if b.exceeded {
			return "", false
		}
		limit = b.expand
	}
	var out strings.Builder
	out.Grow(len(s) + 16)
	writeLit := func(v string) {
		if !pattern {
			out.WriteString(v)
			return
		}
		for _, r := range v {
			if r == '\\' || r == '*' || r == '?' {
				out.WriteByte('\\')
			}
			out.WriteRune(r)
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
					// Check before writing: a value can be as large as the
					// request allows, and escaping can double it.
					if limit >= 0 && out.Len()+2*len(v) > limit {
						b.fail()
						return "", false
					}
					writeLit(v)
				}
				i += end + 3
				continue
			}
		}
		if pattern && s[i] == '\\' {
			out.WriteString(`\\`)
		} else {
			out.WriteByte(s[i])
		}
		i++
	}
	if b != nil {
		if b.expand -= out.Len(); b.expand < 0 {
			b.exceeded = true
			return "", false
		}
	}
	return out.String(), true
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
// wildcards and policy variables, charging the work to b.
func matchPattern(pat, subject string, fold bool, lookup varLookup, b *budget) bool {
	if !strings.Contains(pat, "${") {
		return globMatchBudget(pat, subject, fold, false, b)
	}
	exp, ok := expandVars(pat, lookup, true, b)
	if !ok {
		return false
	}
	return globMatchBudget(exp, subject, fold, true, b)
}
