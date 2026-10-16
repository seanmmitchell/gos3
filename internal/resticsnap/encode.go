package resticsnap

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/maphash"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Object keys become file paths below /objects of a snapshot, one '/'
// separated segment per path component. Each segment is encoded so that it
// is a valid file name on Linux, macOS and Windows (encodeSegment), and the
// names of one directory are made distinct ignoring case (pathAssigner).
// Paths are never decoded: the manifest records each key's path.
//
// Encoding of a segment (percent-encoding a byte gives "%XX", upper-case
// hex):
//
//   - '%', control bytes, 0x7F, \ : * ? " < > | and the bytes of invalid
//     UTF-8 are percent-encoded;
//   - an empty segment (a key with "//" or a trailing '/') is "%"; "." is
//     "%2E" and ".." "%2E%2E";
//   - a leading space and a trailing '.' or space are percent-encoded
//     (Windows drops trailing dots and spaces);
//   - a Windows device name (CON, PRN, AUX, NUL, COM0-9, LPT0-9, CONIN$,
//     CONOUT$, with or without an extension) gets its first character
//     percent-encoded;
//   - a segment longer than 240 bytes once encoded is cut (at a character
//     boundary) to at most 200 bytes and gets the suffix "%~" and 16 hex
//     digits of the SHA-256 of the original segment.
//
// The encoding is injective: '%' is always encoded, so "%" alone, "%~" and
// "%%" never appear in the encoding of another segment.
//
// Within a directory, a name that equals an earlier name ignoring case (as
// case-insensitive file systems compare names), or that names a file where
// a directory is needed or the reverse (an object "a/b" next to an object
// "a/b/c"), gets the suffix "%%" and 8 hex digits of the SHA-256 of its key
// (for a directory: of the key prefix ending in its '/'). Names are claimed
// in key order, so the earlier key keeps the plain name. Unicode
// normalization (macOS) is not considered.

const (
	// maxSegment is the longest encoded segment kept whole.
	maxSegment = 240
	// cutSegment is the length a longer segment is cut to.
	cutSegment = 200
)

// deviceNames are the Windows device names (upper case).
var deviceNames = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for _, p := range []string{"COM", "LPT"} {
		for _, d := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			m[p+d] = true
		}
	}
	return m
}()

// special are the printable ASCII characters that are percent-encoded.
const special = `%\:*?"<>|`

const hexDigits = "0123456789ABCDEF"

// escapeByte appends the percent-encoding of c to b.
func escapeByte(b []byte, c byte) []byte {
	return append(b, '%', hexDigits[c>>4], hexDigits[c&15])
}

// encodeSegment encodes one key segment as a file name (see the top of the
// file).
func encodeSegment(seg string) string {
	switch seg {
	case "":
		return "%"
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	}
	b := make([]byte, 0, len(seg)+8)
	for i := 0; i < len(seg); {
		c := seg[i]
		if c < utf8.RuneSelf {
			if c < 0x20 || c == 0x7F || strings.IndexByte(special, c) >= 0 {
				b = escapeByte(b, c)
			} else {
				b = append(b, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(seg[i:])
		if r == utf8.RuneError && size <= 1 {
			b = escapeByte(b, c)
			i++
			continue
		}
		b = append(b, seg[i:i+size]...)
		i += size
	}
	if b[0] == ' ' {
		b = append([]byte("%20"), b[1:]...)
	}
	if last := b[len(b)-1]; last == '.' || last == ' ' {
		b = escapeByte(b[:len(b)-1], last)
	}
	out := string(b)
	if isDeviceName(out) {
		out = string(escapeByte(nil, out[0])) + out[1:]
	}
	if len(out) > maxSegment {
		sum := sha256.Sum256([]byte(seg))
		out = cutEncoded(out, cutSegment) + "%~" + hex.EncodeToString(sum[:])[:16]
	}
	return out
}

// isDeviceName reports whether the file name name is reserved on Windows:
// its part before the first '.', without trailing spaces, is a device name
// (ignoring case).
func isDeviceName(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	base = strings.TrimRight(base, " ")
	if len(base) < 3 || len(base) > 7 {
		return false
	}
	return deviceNames[strings.ToUpper(base)]
}

// cutEncoded returns the longest prefix of the encoded segment s of at most
// n bytes that ends neither inside a percent-encoding nor inside a UTF-8
// sequence.
func cutEncoded(s string, n int) string {
	i := 0
	for i < len(s) {
		step := 1
		switch {
		case s[i] == '%' && i+2 < len(s):
			step = 3
		case s[i] >= utf8.RuneSelf:
			_, step = utf8.DecodeRuneInString(s[i:])
		}
		if i+step > n {
			break
		}
		i += step
	}
	return s[:i]
}

// foldName returns the case-insensitive form of a file name: each rune
// replaced by the smallest rune of its case folding orbit (so that "K",
// "k" and the Kelvin sign compare equal). It is stricter than any file
// system's case folding.
func foldName(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToUpper(s)
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		low := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < low {
				low = f
			}
		}
		b = utf8.AppendRune(b, low)
	}
	return string(b)
}

// collisionSuffix returns the suffix of a name that collides in its
// directory: "%%" and hex digits of the SHA-256 of id (the key, or the key
// prefix of a directory), more of them on each further attempt.
func collisionSuffix(id string, attempt int) string {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:])
	switch attempt {
	case 0:
		return "%%" + h[:8]
	case 1:
		return "%%" + h[:16]
	case 2:
		return "%%" + h
	}
	return "%%" + h + "-" + strings.Repeat("x", attempt-2)
}

// pathAssigner assigns the paths of keys, which it must be given in
// ascending order (byte-wise, as bucket listings are sorted), each once.
// The directories of the current key are kept on a stack with the names
// claimed in each; a directory is left for good when a key no longer has
// its prefix (the keys with a prefix are contiguous in key order), and so
// is its table of names.
type pathAssigner struct {
	seed  maphash.Seed
	stack []*dirFrame // stack[0] is /objects itself
}

// dirFrame is a directory of the current key.
type dirFrame struct {
	// raw is the key prefix of the directory, ending in '/' ("" for
	// /objects); path its encoded path below /objects ("" for /objects).
	raw, path string
	// claims maps the hash of a folded name to the hash of the name and
	// whether it is a directory (bit 0).
	claims map[uint64]uint64
}

// newPathAssigner returns an assigner for one snapshot.
func newPathAssigner() *pathAssigner {
	return &pathAssigner{seed: maphash.MakeSeed(), stack: []*dirFrame{{claims: map[uint64]uint64{}}}}
}

// assign returns the path of key relative to /objects (slash-separated).
func (a *pathAssigner) assign(key string) string {
	for len(a.stack) > 1 && !strings.HasPrefix(key, a.stack[len(a.stack)-1].raw) {
		a.stack[len(a.stack)-1] = nil
		a.stack = a.stack[:len(a.stack)-1]
	}
	f := a.stack[len(a.stack)-1]
	rest := key[len(f.raw):]
	for {
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			break
		}
		raw := f.raw + rest[:i+1]
		name := a.claim(f, encodeSegment(rest[:i]), raw, true)
		f = &dirFrame{raw: raw, path: joinPath(f.path, name), claims: map[uint64]uint64{}}
		a.stack = append(a.stack, f)
		rest = rest[i+1:]
	}
	return joinPath(f.path, a.claim(f, encodeSegment(rest), key, false))
}

// claim claims name in the directory f for id (a key, or the key prefix of
// a directory) and returns it, with a collision suffix when it is taken.
func (a *pathAssigner) claim(f *dirFrame, name, id string, dir bool) string {
	base := name
	for attempt := 0; ; attempt++ {
		folded := maphash.String(a.seed, foldName(name))
		exact := maphash.String(a.seed, name) &^ 1
		if dir {
			exact |= 1
		}
		prev, taken := f.claims[folded]
		if !taken {
			f.claims[folded] = exact
			return name
		}
		if dir && prev == exact {
			return name // the same directory again
		}
		name = base + collisionSuffix(id, attempt)
	}
}

// joinPath joins a directory path and a name.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
