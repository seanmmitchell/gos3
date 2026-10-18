package iam

import (
	"strings"
	"testing"
	"time"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		fold       bool
		want       bool
	}{
		{"*", "", false, true},
		{"*", "anything", false, true},
		{"", "", false, true},
		{"", "a", false, false},
		{"a", "", false, false},
		{"abc", "abc", false, true},
		{"abc", "abd", false, false},
		{"a*c", "abbbc", false, true},
		{"a*c", "ac", false, true},
		{"a*c", "acb", false, false},
		{"a?c", "abc", false, true},
		{"a?c", "ac", false, false},
		{"?", "é", false, true},
		{"*.txt", "dir/file.txt", false, true},
		{"*a*a*a*", "aaa", false, true},
		{"*a*a*a*b", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false, false},
		{"s3:get*", "s3:GetObject", true, true},
		{"s3:get*", "s3:GetObject", false, false},
		{"S3:GETOBJECT", "s3:getobject", true, true},
		{"arn:aws:s3:::b/*", "arn:aws:s3:::b/k/x", false, true},
		{"arn:aws:s3:::b/*", "arn:aws:s3:::b", false, false},
		{"**", "x", false, true},
		{"a**b", "ab", false, true},
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.s, tt.fold, false); got != tt.want {
			t.Errorf("globMatch(%q, %q, fold=%v) = %v, want %v", tt.pattern, tt.s, tt.fold, got, tt.want)
		}
	}
}

func TestGlobMatchEscaped(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{`a\*`, "a*", true},
		{`a\*`, "ab", false},
		{`a\?`, "a?", true},
		{`a\?`, "ab", false},
		{`a\\b`, `a\b`, true},
		{`\*x*`, "*xyz", true},
		{`*\*`, "abc*", true},
		{`*\*`, "abc", false},
		{`a\`, `a\`, true}, // trailing backslash is literal
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.s, false, true); got != tt.want {
			t.Errorf("globMatch(%q, %q, escaped) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestGlobMatchHostileInputIsFast(t *testing.T) {
	pattern := strings.Repeat("*a", 500) + "b"
	s := strings.Repeat("a", 5000)
	start := time.Now()
	if globMatch(pattern, s, false, false) {
		t.Fatal("unexpected match")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("glob took %v", d)
	}
}

func TestExpandVars(t *testing.T) {
	vars := map[string]string{"aws:username": "alice", "s3:prefix": "p*q"}
	lookup := func(n string) (string, bool) { v, ok := vars[n]; return v, ok }
	tests := []struct {
		in      string
		pattern bool
		want    string
		ok      bool
	}{
		{"plain", false, "plain", true},
		{"home/${aws:username}/*", true, `home/alice/*`, true},
		{"home/${aws:username}/*", false, `home/alice/*`, true},
		{"${s3:prefix}", true, `p\*q`, true},
		{"${s3:prefix}", false, `p*q`, true},
		{"a${*}b${?}c${$}", true, `a\*b\?c$`, true},
		{"a${*}b${?}c${$}", false, `a*b?c$`, true},
		{"${aws:missing}", true, "", false},
		{"${aws:missing, 'dflt'}/x", true, "dflt/x", true},
		{"${aws:username , 'dflt'}", false, "alice", true},
		{"${unterminated", false, "${unterminated", true},
		{`back\slash${aws:username}`, true, `back\\slashalice`, true},
		{`back\slash`, true, `back\\slash`, true},
	}
	for _, tt := range tests {
		got, ok := expandVars(tt.in, lookup, tt.pattern, nil)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("expandVars(%q, pattern=%v) = %q, %v; want %q, %v", tt.in, tt.pattern, got, ok, tt.want, tt.ok)
		}
	}
	// Literal '*' from a variable must not act as a wildcard.
	if matchPattern("${s3:prefix}", "pXq", false, lookup, nil) {
		t.Fatal("variable value treated as wildcard")
	}
	if !matchPattern("${s3:prefix}", "p*q", false, lookup, nil) {
		t.Fatal("variable value not matched literally")
	}
	if !matchPattern(`a\b*`, `a\bcd`, false, lookup, nil) {
		t.Fatal("plain backslash not literal")
	}
	if !matchPattern(`a\b/${aws:username}`, `a\b/alice`, false, lookup, nil) {
		t.Fatal("backslash with variables not literal")
	}
}
