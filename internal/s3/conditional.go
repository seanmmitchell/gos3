package s3

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// checkConditions evaluates the GET/HEAD conditional headers (If-Match,
// If-None-Match, If-Modified-Since, If-Unmodified-Since) against an object
// with the given (unquoted) ETag and last-modified time. It returns nil,
// s3err.ErrPreconditionFailed (412) or s3err.ErrNotModified (304).
//
// Precedence follows RFC 7232 and AWS: when If-Match is present,
// If-Unmodified-Since is ignored (If-Match true + If-Unmodified-Since false
// => 200); when If-None-Match is present, If-Modified-Since is ignored
// (If-None-Match false + If-Modified-Since true => 304). Dates compare at
// one-second granularity; unparsable dates are ignored.
//
// For a 304 the handler should still set ETag and Last-Modified before
// returning the error (writeError keeps them for 304 responses).
func checkConditions(h http.Header, etag string, lastModified time.Time) error {
	return evalConditions(etag, lastModified,
		h.Get("If-Match"), h.Get("If-None-Match"),
		h.Get("If-Modified-Since"), h.Get("If-Unmodified-Since"),
		s3err.ErrNotModified)
}

// checkCopySourceConditions evaluates the x-amz-copy-source-if-* headers
// against the copy source with the same precedence as checkConditions;
// every failure (including the "not modified" cases) is PreconditionFailed.
func checkCopySourceConditions(h http.Header, etag string, lastModified time.Time) error {
	return evalConditions(etag, lastModified,
		h.Get("X-Amz-Copy-Source-If-Match"), h.Get("X-Amz-Copy-Source-If-None-Match"),
		h.Get("X-Amz-Copy-Source-If-Modified-Since"), h.Get("X-Amz-Copy-Source-If-Unmodified-Since"),
		s3err.ErrPreconditionFailed)
}

func evalConditions(etag string, lastModified time.Time, ifMatch, ifNoneMatch, ifModSince, ifUnmodSince string, notModified *s3err.Error) error {
	lm := lastModified.Truncate(time.Second)
	if strings.TrimSpace(ifMatch) != "" {
		if !etagListMatches(ifMatch, etag) {
			return s3err.ErrPreconditionFailed.With("Condition", "If-Match")
		}
	} else if ifUnmodSince != "" {
		if t, ok := parseHTTPTime(ifUnmodSince); ok && lm.After(t) {
			return s3err.ErrPreconditionFailed.With("Condition", "If-Unmodified-Since")
		}
	}
	if strings.TrimSpace(ifNoneMatch) != "" {
		if etagListMatches(ifNoneMatch, etag) {
			if notModified == s3err.ErrPreconditionFailed {
				return s3err.ErrPreconditionFailed.With("Condition", "If-None-Match")
			}
			return notModified
		}
	} else if ifModSince != "" {
		if t, ok := parseHTTPTime(ifModSince); ok && !lm.After(t) {
			if notModified == s3err.ErrPreconditionFailed {
				return s3err.ErrPreconditionFailed.With("Condition", "If-Modified-Since")
			}
			return notModified
		}
	}
	return nil
}

// etagListMatches reports whether a comma-separated list of entity tags
// (quoted or not, weak or strong, or "*") contains etag.
func etagListMatches(list, etag string) bool {
	etag = trimETag(etag)
	for _, t := range strings.Split(list, ",") {
		t = strings.TrimSpace(t)
		if t == "*" {
			return true
		}
		if t != "" && trimETag(t) == etag {
			return true
		}
	}
	return false
}

// trimETag strips a weak prefix and surrounding quotes from an entity tag.
func trimETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
}

// quoteETag returns the quoted form of an unquoted ETag.
func quoteETag(etag string) string { return `"` + trimETag(etag) + `"` }

// byteRange is a satisfiable byte range of an object.
type byteRange struct {
	Start  int64
	Length int64
}

// End returns the offset of the last byte.
func (b byteRange) End() int64 { return b.Start + b.Length - 1 }

// ContentRange returns the Content-Range header value for an object of
// the given size.
func (b byteRange) ContentRange(size int64) string {
	return "bytes " + strconv.FormatInt(b.Start, 10) + "-" + strconv.FormatInt(b.End(), 10) + "/" + strconv.FormatInt(size, 10)
}

// parseRange parses a Range header for an object of size bytes, the way
// S3 does:
//   - "" or a syntactically invalid header => nil, nil (serve the full object)
//   - multiple ranges => nil, nil (S3 ignores the header and returns 200)
//   - "bytes=a-b", "bytes=a-", "bytes=-n" => the range, with b clamped to
//     the last byte and n larger than the object meaning the whole object
//   - a range starting at or beyond the end (or "-0", or any range of an
//     empty object) => InvalidRange (416) with RangeRequested and
//     ActualObjectSize; writeError adds "Content-Range: bytes */<size>".
func parseRange(v string, size int64) (*byteRange, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	unit, spec, ok := strings.Cut(v, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return nil, nil
	}
	spec = strings.TrimSpace(spec)
	if strings.Contains(spec, ",") {
		return nil, nil
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return nil, nil
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	unsatisfiable := func() (*byteRange, error) {
		return nil, s3err.ErrInvalidRange.With("RangeRequested", v).With("ActualObjectSize", strconv.FormatInt(size, 10))
	}
	if first == "" { // suffix range
		n, ok := parseDigits(last)
		if !ok {
			return nil, nil
		}
		if n == 0 || size == 0 {
			return unsatisfiable()
		}
		if n > size {
			n = size
		}
		return &byteRange{Start: size - n, Length: n}, nil
	}
	start, ok := parseDigits(first)
	if !ok {
		return nil, nil
	}
	end := int64(math.MaxInt64)
	if last != "" {
		if end, ok = parseDigits(last); !ok || end < start {
			return nil, nil
		}
	}
	if start >= size {
		return unsatisfiable()
	}
	if end >= size {
		end = size - 1
	}
	return &byteRange{Start: start, Length: end - start + 1}, nil
}

// parseDigits parses a non-empty run of ASCII digits, saturating at
// math.MaxInt64.
func parseDigits(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (math.MaxInt64-int64(c-'0'))/10 {
			n = math.MaxInt64
			continue
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// copySource is a parsed x-amz-copy-source header.
type copySource struct {
	Bucket    string
	Key       string
	VersionID string // "" = latest
}

// parseCopySource parses x-amz-copy-source: a URL-encoded
// "[/]bucket/key[?versionId=id]". The "?" separating the version must be
// literal (an encoded %3F belongs to the key).
func parseCopySource(v string) (copySource, error) {
	invalid := func(msg string) (copySource, error) {
		return copySource{}, s3err.ErrInvalidArgument.WithMessage("%s", msg).
			With("ArgumentName", "x-amz-copy-source").With("ArgumentValue", v)
	}
	raw := strings.TrimSpace(v)
	if raw == "" {
		return invalid("Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	rawPath, rawQuery, hasQuery := strings.Cut(raw, "?")
	p, err := url.PathUnescape(rawPath)
	if err != nil {
		return invalid("Invalid copy source encoding")
	}
	p = strings.TrimPrefix(p, "/")
	bucket, key, ok := strings.Cut(p, "/")
	if !ok || bucket == "" || key == "" {
		return invalid("Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	if !plausibleBucketName(bucket) {
		return invalid("Invalid copy source bucket name")
	}
	if err := validateObjectKey(key); err != nil {
		return copySource{}, err
	}
	cs := copySource{Bucket: bucket, Key: key}
	if hasQuery {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return invalid("Invalid copy source encoding")
		}
		if q.Has("versionId") {
			cs.VersionID = q.Get("versionId")
			if cs.VersionID == "" {
				return invalid("Version id cannot be the empty string")
			}
		}
	}
	return cs, nil
}

// parseCopySourceRange parses x-amz-copy-source-range ("bytes=first-last",
// both required) for a source of size bytes (UploadPartCopy).
func parseCopySourceRange(v string, size int64) (byteRange, error) {
	bad := s3err.ErrInvalidArgument.WithMessage("The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy").
		With("ArgumentName", "x-amz-copy-source-range").With("ArgumentValue", v)
	spec, ok := strings.CutPrefix(strings.TrimSpace(v), "bytes=")
	if !ok {
		return byteRange{}, bad
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return byteRange{}, bad
	}
	start, ok1 := parseDigits(a)
	end, ok2 := parseDigits(b)
	if !ok1 || !ok2 || end < start {
		return byteRange{}, bad
	}
	if end >= size {
		return byteRange{}, s3err.ErrInvalidArgument.WithMessage("Range specified is not valid for source object of size: %d", size).
			With("ArgumentName", "x-amz-copy-source-range").With("ArgumentValue", v)
	}
	return byteRange{Start: start, Length: end - start + 1}, nil
}
