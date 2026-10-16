package resticsnap

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEncodeSegment(t *testing.T) {
	for in, want := range map[string]string{
		"":            "%",
		".":           "%2E",
		"..":          "%2E%2E",
		"...":         "..%2E",
		"plain.txt":   "plain.txt",
		"100%":        "100%25",
		"a\\b:c*d?e":  "a%5Cb%3Ac%2Ad%3Fe",
		`"<>|`:        "%22%3C%3E%7C",
		"tab\there":   "tab%09here",
		"del\x7f":     "del%7F",
		"bad\xffutf8": "bad%FFutf8",
		"trail.":      "trail%2E",
		"trail ":      "trail%20",
		" lead":       "%20lead",
		" ":           "%20",
		"CON":         "%43ON",
		"con.txt":     "%63on.txt",
		"Nul.tar.gz":  "%4Eul.tar.gz",
		"COM1":        "%43OM1",
		"lpt9.log":    "%6Cpt9.log",
		"COM¹":        "%43OM¹",
		"CONIN$":      "%43ONIN$",
		"console":     "console",
		"COM10":       "COM10",
		"AUX .x":      "%41UX .x",
		"日本語":         "日本語",
		"é~":          "é~",
	} {
		if got := encodeSegment(in); got != want {
			t.Errorf("encodeSegment(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 200) // 400 bytes
	got := encodeSegment(long)
	if len(got) > maxSegment || !strings.Contains(got, "%~") || !utf8.ValidString(got) {
		t.Fatalf("long segment encoded as %q (%d bytes)", got, len(got))
	}
	if other := encodeSegment(long[:len(long)-2] + "e"); other == got {
		t.Fatal("long segments sharing a prefix collide")
	}
	escapes := strings.Repeat("%", 100) // 300 bytes encoded
	if got := encodeSegment(escapes); len(got) > maxSegment || len(got[:strings.Index(got, "%~")])%3 != 0 {
		t.Fatalf("escapes cut inside an escape: %q", got)
	}
}

// validFileName reports whether name is a valid file name on Windows,
// macOS and Linux.
func validFileName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, `/\:*?"<>|`) ||
		name == "." || name == ".." || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || isDeviceName(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7F {
			return false
		}
	}
	return true
}

// randomKey returns a random key over an alphabet rich in characters the
// encoding handles.
func randomKey(rng *rand.Rand) string {
	alphabet := []string{"a", "A", "b", "B", "/", "/", ".", "..", " ", "%", "%2E", ":", "\\", "\x00", "\xff", "é", "É", "K", "k", "K", "con", "CON", "~", "-"}
	n := 1 + rng.IntN(8)
	var b strings.Builder
	for range n {
		b.WriteString(alphabet[rng.IntN(len(alphabet))])
	}
	if rng.IntN(20) == 0 {
		b.WriteString(strings.Repeat("x", 230+rng.IntN(40)))
	}
	return b.String()
}

// Encoding is injective, and every encoded segment is a valid file name.
func TestEncodeSegmentProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	seen := map[string]string{}
	for range 200000 {
		seg := strings.ReplaceAll(randomKey(rng), "/", "")
		enc := encodeSegment(seg)
		if !validFileName(enc) {
			t.Fatalf("encodeSegment(%q) = %q is not a valid file name", seg, enc)
		}
		if strings.Contains(enc, "%%") {
			t.Fatalf("encodeSegment(%q) = %q holds a collision marker", seg, enc)
		}
		if prev, ok := seen[enc]; ok && prev != seg {
			t.Fatalf("%q and %q both encode as %q", prev, seg, enc)
		}
		seen[enc] = seg
	}
}

// checkPaths checks that paths (of keys, by key) form a tree that a
// case-insensitive file system can hold: distinct paths, no name used for
// a file and a directory, no two names of a directory equal ignoring case.
func checkPaths(t *testing.T, paths map[string]string) {
	t.Helper()
	type node struct {
		exact string
		dir   bool
	}
	names := map[string]node{} // folded path -> node
	claim := func(p string, dir bool) {
		folded := foldName(p)
		n, ok := names[folded]
		switch {
		case !ok:
			names[folded] = node{p, dir}
		case n.exact != p:
			t.Fatalf("paths %q and %q collide ignoring case", n.exact, p)
		case !n.dir || !dir:
			t.Fatalf("path %q is a file and a directory", p)
		}
	}
	for key, p := range paths {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			if !validFileName(s) {
				t.Fatalf("key %q: invalid name %q in %q", key, s, p)
			}
			if len(s) > 255 {
				t.Fatalf("key %q: name %q too long", key, s)
			}
			if i < len(segs)-1 {
				claim(strings.Join(segs[:i+1], "/"), true)
			}
		}
		claim(p, false)
	}
}

// assignAll assigns the paths of keys (sorted and deduplicated first).
func assignAll(keys []string) map[string]string {
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	a := newPathAssigner()
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = a.assign(k)
	}
	return out
}

func TestPathAssignerCollisions(t *testing.T) {
	paths := assignAll([]string{"a", "a/b", "a/c", "A", "A/x", "dir/", "dir", "x//y", "x/", "k", "K", "K", "b/C/d", "b/c/e", "b/c"})
	checkPaths(t, paths)
	for key, want := range map[string]string{
		"A":     "A",                           // first in key order keeps its name
		"a":     "a" + collisionSuffix("a", 0), // a file folding like "A"
		"a/b":   "a" + collisionSuffix("a/", 0) + "/b",
		"A/x":   "A" + collisionSuffix("A/", 0) + "/x", // a directory where the file "A" is
		"dir":   "dir",
		"dir/":  "dir" + collisionSuffix("dir/", 0) + "/%",
		"x//y":  "x/%" + collisionSuffix("x//", 0) + "/y", // a directory where the file "%" is
		"x/":    "x/%",                                    // "x/" sorts before "x//y"
		"K":     "K",
		"k":     "k" + collisionSuffix("k", 0),
		"b/C/d": "b/C/d",
		"b/c":   "b/c" + collisionSuffix("b/c", 0),
		"b/c/e": "b/c" + collisionSuffix("b/c/", 0) + "/e",
	} {
		if got := paths[key]; got != want {
			t.Errorf("path of %q = %q, want %q", key, got, want)
		}
	}
	// The paths depend on the keys only.
	again := assignAll([]string{"a", "a/b", "a/c", "A", "A/x", "dir/", "dir", "x//y", "x/", "k", "K", "K", "b/C/d", "b/c/e", "b/c"})
	for k, p := range paths {
		if again[k] != p {
			t.Fatalf("path of %q changed from %q to %q", k, p, again[k])
		}
	}
}

// Random key sets get collision-free trees.
func TestPathAssignerProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for round := range 200 {
		var keys []string
		for range 1 + rng.IntN(300) {
			keys = append(keys, randomKey(rng))
		}
		if round%2 == 0 {
			// Keys below a few shared prefixes.
			for i := range keys {
				keys[i] = []string{"p/", "P/", "p", "q/r/"}[rng.IntN(4)] + keys[i]
			}
		}
		paths := assignAll(keys)
		checkPaths(t, paths)
		seen := map[string]string{}
		for k, p := range paths {
			if prev, ok := seen[p]; ok {
				t.Fatalf("keys %q and %q share path %q", prev, k, p)
			}
			seen[p] = k
		}
	}
}
