package s3

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// Limits shared by the handlers.
const (
	maxKeyLength      = 1024    // bytes of UTF-8
	maxUserMetaSize   = 2 << 10 // bytes: sum of x-amz-meta-* names and values
	maxListKeys       = 1000
	maxDeleteObjects  = 1000
	minPartNumber     = 1
	maxPartNumber     = 10000
	minPartSize       = 5 << 20 // every part but the last
	maxRedirectLength = 2 << 10

	// maxObjectMetaSize bounds all metadata an object returns as response
	// headers: content headers, website redirect and user metadata (see
	// validateStoredMeta). It is AWS's limit for a PUT's request headers.
	maxObjectMetaSize = 8 << 10
)

// validBucketName reports whether name is a valid bucket name for
// CreateBucket: 3-63 characters of lowercase letters, digits, '-' and '.',
// starting and ending with a letter or digit, not an IPv4 address, no "..",
// ".-" or "-.", and none of the prefixes/suffixes AWS reserves.
func validBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '.':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return false
	}
	if looksLikeIPv4(name) {
		return false
	}
	for _, p := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	for _, sfx := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(name, sfx) {
			return false
		}
	}
	return true
}

// validateBucketName returns InvalidBucketName unless validBucketName.
func validateBucketName(name string) error {
	if !validBucketName(name) {
		return s3err.ErrInvalidBucketName.With("BucketName", name)
	}
	return nil
}

// looksLikeIPv4 reports whether s is formatted like a dotted-quad address.
func looksLikeIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// validateObjectKey checks an object key: non-empty, at most 1024 bytes,
// valid UTF-8 (KeyTooLongError / InvalidArgument otherwise).
func validateObjectKey(key string) error {
	switch {
	case key == "":
		return s3err.ErrInvalidArgument.WithMessage("Object key must not be empty.")
	case len(key) > maxKeyLength:
		return s3err.ErrKeyTooLong.With("MaxSizeAllowed", strconv.Itoa(maxKeyLength)).With("Size", strconv.Itoa(len(key)))
	case !utf8.ValidString(key):
		return s3err.ErrInvalidArgument.WithMessage("Object key must be valid UTF-8.")
	}
	return nil
}

// trimHeaderSpace removes the spaces and tabs around a header value (the
// optional whitespace of HTTP, which net/http already strips from HTTP/1
// headers). Other whitespace, such as U+00A0, is kept, so a value carrying
// it fails validation. Header values that become object attributes are
// trimmed with this function both when they are parsed and when they
// become condition keys (see conditionContext): a value is stored exactly
// as the policy saw it, or refused.
func trimHeaderSpace(v string) string { return strings.Trim(v, " \t") }

// Storage classes accepted in x-amz-storage-class (gos3 records them; all
// data is stored identically).
var storageClasses = map[string]bool{
	"STANDARD": true, "REDUCED_REDUNDANCY": true, "STANDARD_IA": true,
	"ONEZONE_IA": true, "INTELLIGENT_TIERING": true, "GLACIER": true,
	"DEEP_ARCHIVE": true, "GLACIER_IR": true, "OUTPOSTS": true, "SNOW": true,
	"EXPRESS_ONEZONE": true,
}

// normalizeStorageClass validates a storage class header value and returns
// the stored form ("" for STANDARD / absent).
func normalizeStorageClass(v string) (string, error) {
	v = trimHeaderSpace(v)
	if v == "" || v == "STANDARD" {
		return "", nil
	}
	if !storageClasses[v] {
		return "", s3err.ErrInvalidStorageClass.With("StorageClassRequested", v)
	}
	return v, nil
}

// storageClassOrStandard returns the class to report in listings.
func storageClassOrStandard(sc string) string {
	if sc == "" {
		return "STANDARD"
	}
	return sc
}

// objectCannedACLs are the canned ACLs valid for objects; bucketCannedACLs
// additionally allow log-delivery-write.
var objectCannedACLs = map[string]bool{
	aclPrivate: true, aclPublicRead: true, aclPublicReadWrite: true,
	aclAuthenticatedRead: true, aclAWSExecRead: true, aclBucketOwnerRead: true,
	aclBucketOwnerFullControl: true,
}

// validateCannedACL validates an x-amz-acl value for an object (bucket =
// false) or bucket and returns the stored form ("" for private / absent).
func validateCannedACL(v string, bucket bool) (string, error) {
	v = trimHeaderSpace(v)
	if v == "" || v == aclPrivate {
		return "", nil
	}
	if objectCannedACLs[v] || (bucket && v == aclLogDeliveryWrite) {
		return v, nil
	}
	return "", s3err.ErrInvalidArgument.With("ArgumentName", "x-amz-acl").With("ArgumentValue", v)
}

// validTagChars reports whether s only has characters allowed in tags:
// Unicode letters, digits, whitespace and _ . : / = + - @.
func validTagChars(s string) bool {
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsSpace(r), unicode.IsMark(r):
		case strings.ContainsRune("_.:/=+-@", r):
		default:
			return false
		}
	}
	return true
}

// parseMaxKeys parses a max-keys style parameter: absent => def; not a
// non-negative integer => InvalidArgument; values above limit are clamped.
func parseMaxKeys(q url.Values, name string, def, limit int) (int, error) {
	if !q.Has(name) {
		return def, nil
	}
	v := q.Get(name)
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, s3err.ErrInvalidArgument.WithMessage("Provided %s not an integer or within integer range", name).
			With("ArgumentName", name).With("ArgumentValue", v)
	}
	if n > limit {
		n = limit
	}
	return n, nil
}

// parsePartNumber parses a partNumber parameter (1..10000).
func parsePartNumber(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < minPartNumber || n > maxPartNumber {
		return 0, s3err.ErrInvalidArgument.WithMessage("Part number must be an integer between 1 and 10000, inclusive").
			With("ArgumentName", "partNumber").With("ArgumentValue", v)
	}
	return n, nil
}
