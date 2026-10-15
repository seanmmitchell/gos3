package native

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Where metadata lives
//
// A native object carries as much of its gos3 metadata as the remote
// service can hold natively, so other S3 clients see it:
//
//   - Content headers (Content-Type, -Encoding, -Disposition, -Language,
//     Cache-Control, Expires) and user metadata travel as native headers
//     when they survive an HTTP header round trip unchanged on any S3
//     service (printable ASCII without surrounding spaces, lowercase
//     token names).
//   - Tags travel as x-amz-tagging when they are valid AWS tags and the
//     remote supports object tagging; they are read back with
//     GetObjectTagging (only when HEAD reports a tag count).
//   - On buckets with object lock, retention and legal hold travel as
//     x-amz-object-lock-* headers and are changed with the retention and
//     legal hold APIs, so the remote enforces them. Elsewhere they are
//     kept as gos3 metadata only ("contract" retention, which the S3
//     layer enforces).
//
// The ACL and the website redirect decide who may read an object and
// where browsers are sent, and anybody who can write the remote bucket
// can set any header of its objects (or restore an older version with its
// headers): they are therefore never read from the remote object, but
// kept in an overlay record bound to the exact remote version
// (overlay.go), written once the remote acknowledged the write. An
// object whose overlay could not be written is private, and the write
// fails.
//
// Everything else that cannot or must not be native (checksums, part
// sizes, an ETag that differs from the remote one, SSE and storage class
// settings, which the remote could apply with real effects, and every
// value that cannot survive a header round trip) travels in ONE reserved
// user metadata entry, x-amz-meta-gos3-meta ("ext"), holding
// base64url(JSON(metaPatch)). It is omitted when there is nothing to put
// in it, so objects written with plain metadata look exactly like objects
// written by any other client. What does not fit into the 2 KiB S3 allows
// for user metadata is kept in an overlay record instead, as is every
// change made after the object was written (UpdateObjectMeta never
// rewrites an object). An ACL or redirect found in a gos3-meta header
// (written by the former gateway backing, or by anybody else) is
// ignored.
//
// Reads apply, in order of precedence: overlay > native > ext. Native
// values that ext also holds cannot exist (a field goes to one of them),
// except for tags and object lock settings, which the remote's APIs can
// change after the write: native tags (a tag count on HEAD) win over ext
// tags, and on buckets with object lock the native retention and legal
// hold are the only ones reported. Native retain-until dates have
// millisecond precision; the exact time gos3 was given is kept in ext or
// the overlay ("rp") and reported while it rounds up to the native date.

// metaName is the reserved user metadata name (x-amz-meta-gos3-meta).
const metaName = "gos3-meta"

// maxUserMetaBytes is the limit of S3 services (AWS, gos3) on the user
// metadata of an object: the sum of the names and values of its
// x-amz-meta-* headers.
const maxUserMetaBytes = 2 << 10

// defaultContentType is what S3 services report for objects stored
// without a Content-Type; it is reported as "" (the S3 layer renders ""
// as this value again). An explicit binary/octet-stream travels in ext.
const defaultContentType = "binary/octet-stream"

// Object lock headers.
const (
	hdrLockMode      = "X-Amz-Object-Lock-Mode"
	hdrLockUntil     = "X-Amz-Object-Lock-Retain-Until-Date"
	hdrLegalHold     = "X-Amz-Object-Lock-Legal-Hold"
	hdrTagging       = "X-Amz-Tagging"
	hdrTaggingCount  = "X-Amz-Tagging-Count"
	retainDateLayout = "2006-01-02T15:04:05.000Z"
)

// maxObjectTags is the most tags an S3 object can have.
const maxObjectTags = 10

// metaPatch is a partial ObjectMeta: the fields it sets (non-nil) replace
// those of the metadata it is applied to. It is the JSON document of the
// gos3-meta header (ext) and of overlay records; field names are short to
// keep the header small, and those of the former gateway backing's header
// are kept, so objects it wrote read the same.
type metaPatch struct {
	// ETag replaces the remote ETag (it differs when gos3 uploaded the
	// object in parts, or when the S3 layer's ETag is not the MD5 of the
	// content).
	ETag               *string `json:"e,omitempty"`
	ContentType        *string `json:"ct,omitempty"`
	ContentEncoding    *string `json:"ce,omitempty"`
	ContentDisposition *string `json:"cd,omitempty"`
	ContentLanguage    *string `json:"cl,omitempty"`
	CacheControl       *string `json:"cc,omitempty"`
	Expires            *string `json:"ex,omitempty"`
	// UserMeta entries are added to the user metadata (ext: the entries
	// that could not be native); AllUserMeta replaces it as a whole
	// (overlays of a metadata update).
	UserMeta      map[string]string    `json:"um,omitempty"`
	AllUserMeta   *map[string]string   `json:"uma,omitempty"`
	Tags          *map[string]string   `json:"t,omitempty"`
	StorageClass  *string              `json:"sc,omitempty"`
	ACL           *string              `json:"acl,omitempty"`
	Checksums     *map[string]string   `json:"cs,omitempty"`
	ChecksumType  *string              `json:"cst,omitempty"`
	PartSizes     *[]int64             `json:"ps,omitempty"` // run-length encoded: size, count, size, count, ...
	PartChecksums *[]map[string]string `json:"pc,omitempty"`
	SSE           *string              `json:"sse,omitempty"`
	SSEKMSKeyID   *string              `json:"kms,omitempty"`
	Redirect      *string              `json:"wr,omitempty"`
	// RetentionMode, RetainUntil and LegalHold are contract-only object
	// lock settings (buckets without object lock).
	RetentionMode *string    `json:"rm,omitempty"`
	RetainUntil   *time.Time `json:"ru,omitempty"`
	LegalHold     *bool      `json:"lh,omitempty"`
	// RetainPrecise is the exact retain-until date of native retention
	// (buckets with object lock), reported while it rounds up to the
	// remote's millisecond date.
	RetainPrecise *time.Time `json:"rp,omitempty"`
	// LastModified (Unix seconds) is read from headers written by the
	// former gateway backing, which rewrote objects onto themselves for
	// metadata changes and kept the original time here. Never written.
	LastModified int64 `json:"lm,omitempty"`
}

// empty reports whether p sets nothing.
func (p *metaPatch) empty() bool {
	if p == nil {
		return true
	}
	data, _ := json.Marshal(p)
	return string(data) == "{}"
}

// apply applies p to m (tags and contract object lock settings included;
// the native ones are applied by the caller afterwards).
func (p *metaPatch) apply(m *storage.ObjectMeta) {
	if p == nil {
		return
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&m.ETag, p.ETag)
	set(&m.ContentType, p.ContentType)
	set(&m.ContentEncoding, p.ContentEncoding)
	set(&m.ContentDisposition, p.ContentDisposition)
	set(&m.ContentLanguage, p.ContentLanguage)
	set(&m.CacheControl, p.CacheControl)
	set(&m.Expires, p.Expires)
	if p.AllUserMeta != nil {
		m.UserMeta = maps.Clone(*p.AllUserMeta)
	}
	if len(p.UserMeta) > 0 {
		if m.UserMeta == nil {
			m.UserMeta = map[string]string{}
		}
		maps.Copy(m.UserMeta, p.UserMeta)
	}
	if p.Tags != nil {
		m.Tags = maps.Clone(*p.Tags)
	}
	set(&m.StorageClass, p.StorageClass)
	set(&m.ACL, p.ACL)
	if p.Checksums != nil {
		m.Checksums = maps.Clone(*p.Checksums)
	}
	set(&m.ChecksumType, p.ChecksumType)
	if p.PartSizes != nil {
		m.PartSizes = rleDecode(*p.PartSizes)
	}
	if p.PartChecksums != nil {
		m.PartChecksums = clonePartChecksums(*p.PartChecksums)
	}
	set(&m.SSE, p.SSE)
	set(&m.SSEKMSKeyID, p.SSEKMSKeyID)
	set(&m.WebsiteRedirect, p.Redirect)
	set(&m.RetentionMode, p.RetentionMode)
	if p.RetainUntil != nil {
		m.RetainUntil = *p.RetainUntil
	}
	if p.LegalHold != nil {
		m.LegalHold = *p.LegalHold
	}
}

// merge returns p with the fields q sets replaced (q's UserMeta entries
// are added; a q.AllUserMeta replaces p's user metadata entirely).
func (p metaPatch) merge(q *metaPatch) metaPatch {
	if q == nil {
		return p
	}
	if q.AllUserMeta != nil {
		p.UserMeta = nil
	}
	pick := func(dst **string, v *string) {
		if v != nil {
			*dst = v
		}
	}
	pick(&p.ETag, q.ETag)
	pick(&p.ContentType, q.ContentType)
	pick(&p.ContentEncoding, q.ContentEncoding)
	pick(&p.ContentDisposition, q.ContentDisposition)
	pick(&p.ContentLanguage, q.ContentLanguage)
	pick(&p.CacheControl, q.CacheControl)
	pick(&p.Expires, q.Expires)
	if q.AllUserMeta != nil {
		p.AllUserMeta = q.AllUserMeta
	}
	if len(q.UserMeta) > 0 {
		if p.AllUserMeta != nil {
			all := maps.Clone(*p.AllUserMeta)
			maps.Copy(all, q.UserMeta)
			p.AllUserMeta = &all
		} else {
			um := maps.Clone(p.UserMeta)
			if um == nil {
				um = map[string]string{}
			}
			maps.Copy(um, q.UserMeta)
			p.UserMeta = um
		}
	}
	if q.Tags != nil {
		p.Tags = q.Tags
	}
	pick(&p.StorageClass, q.StorageClass)
	pick(&p.ACL, q.ACL)
	if q.Checksums != nil {
		p.Checksums = q.Checksums
	}
	pick(&p.ChecksumType, q.ChecksumType)
	if q.PartSizes != nil {
		p.PartSizes = q.PartSizes
	}
	if q.PartChecksums != nil {
		p.PartChecksums = q.PartChecksums
	}
	pick(&p.SSE, q.SSE)
	pick(&p.SSEKMSKeyID, q.SSEKMSKeyID)
	pick(&p.Redirect, q.Redirect)
	pick(&p.RetentionMode, q.RetentionMode)
	if q.RetainUntil != nil {
		p.RetainUntil = q.RetainUntil
	}
	if q.LegalHold != nil {
		p.LegalHold = q.LegalHold
	}
	if q.RetainPrecise != nil {
		p.RetainPrecise = q.RetainPrecise
	}
	return p
}

// diffMeta returns the patch that turns old into new: every field that
// differs (zero values included) is set.
func diffMeta(old, new storage.ObjectMeta) *metaPatch {
	var p metaPatch
	str := func(dst **string, o, n string) {
		if o != n {
			*dst = &n
		}
	}
	str(&p.ETag, old.ETag, new.ETag)
	str(&p.ContentType, old.ContentType, new.ContentType)
	str(&p.ContentEncoding, old.ContentEncoding, new.ContentEncoding)
	str(&p.ContentDisposition, old.ContentDisposition, new.ContentDisposition)
	str(&p.ContentLanguage, old.ContentLanguage, new.ContentLanguage)
	str(&p.CacheControl, old.CacheControl, new.CacheControl)
	str(&p.Expires, old.Expires, new.Expires)
	if !maps.Equal(old.UserMeta, new.UserMeta) {
		p.AllUserMeta = mapPtr(new.UserMeta)
	}
	if !maps.Equal(old.Tags, new.Tags) {
		p.Tags = mapPtr(new.Tags)
	}
	str(&p.StorageClass, old.StorageClass, new.StorageClass)
	str(&p.ACL, old.ACL, new.ACL)
	if !maps.Equal(old.Checksums, new.Checksums) {
		p.Checksums = mapPtr(new.Checksums)
	}
	str(&p.ChecksumType, old.ChecksumType, new.ChecksumType)
	if !slices.Equal(old.PartSizes, new.PartSizes) {
		ps := rleEncode(new.PartSizes)
		if ps == nil {
			ps = []int64{}
		}
		p.PartSizes = &ps
	}
	if !partChecksumsEqual(old.PartChecksums, new.PartChecksums) {
		pc := clonePartChecksums(new.PartChecksums)
		if pc == nil {
			pc = []map[string]string{}
		}
		p.PartChecksums = &pc
	}
	str(&p.SSE, old.SSE, new.SSE)
	str(&p.SSEKMSKeyID, old.SSEKMSKeyID, new.SSEKMSKeyID)
	str(&p.Redirect, old.WebsiteRedirect, new.WebsiteRedirect)
	str(&p.RetentionMode, old.RetentionMode, new.RetentionMode)
	if !old.RetainUntil.Equal(new.RetainUntil) {
		ru := new.RetainUntil
		p.RetainUntil = &ru
	}
	if old.LegalHold != new.LegalHold {
		lh := new.LegalHold
		p.LegalHold = &lh
	}
	return &p
}

// mapPtr returns a pointer to a non-nil copy of m (so it encodes as {}).
func mapPtr(m map[string]string) *map[string]string {
	c := maps.Clone(m)
	if c == nil {
		c = map[string]string{}
	}
	return &c
}

func partChecksumsEqual(a, b []map[string]string) bool {
	return slices.EqualFunc(a, b, func(x, y map[string]string) bool { return maps.Equal(x, y) })
}

func clonePartChecksums(pc []map[string]string) []map[string]string {
	if pc == nil {
		return nil
	}
	out := make([]map[string]string, len(pc))
	for i, c := range pc {
		out[i] = maps.Clone(c)
	}
	return out
}

// --- encoding ---

// encodeOpts are the parts of the encoding that do not come from the
// ObjectMeta itself.
type encodeOpts struct {
	// keepETag stores meta.ETag in ext (the remote ETag will differ).
	keepETag bool
	// lock: the remote bucket has object lock, so retention and legal
	// hold are sent as object lock headers.
	lock bool
	// tagging: the remote supports native tags.
	tagging bool
}

// encoded is the remote form of an object's metadata.
type encoded struct {
	header http.Header
	// overflow holds what does not travel with the object: the ACL and
	// the website redirect, and what did not fit into the user metadata
	// limit. It is written as an overlay once the object exists.
	overflow *metaPatch
	// tags reports that the tags were sent natively (x-amz-tagging).
	tags bool
	// lock reports that object lock headers were sent.
	lock bool
}

// safeHeaderValue reports whether v survives an HTTP header round trip
// unchanged on any S3 service: non-empty printable ASCII without
// surrounding spaces.
func safeHeaderValue(v string) bool {
	if v == "" || v[0] == ' ' || v[len(v)-1] == ' ' {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// safeMetaName reports whether name can be sent as x-amz-meta-<name> and
// is read back identically (lowercase HTTP token characters).
func safeMetaName(name string) bool {
	if name == "" || name == metaName {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// nativeTags reports whether tags can be stored as the remote object's
// tags: at most 10 valid AWS tags (keys of 1-128 and values of 0-256
// characters from letters, digits, spaces and _ . : / = + - @, no "aws:"
// key prefix). Other whitespace is not native: XML documents (tagging
// requests and answers) do not keep carriage returns.
func nativeTags(tags map[string]string) bool {
	if len(tags) == 0 || len(tags) > maxObjectTags {
		return false
	}
	for k, v := range tags {
		if n := utf8.RuneCountInString(k); n == 0 || n > 128 || utf8.RuneCountInString(v) > 256 {
			return false
		}
		if !validTagChars(k) || !validTagChars(v) || strings.HasPrefix(strings.ToLower(k), "aws:") {
			return false
		}
	}
	return true
}

func validTagChars(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == ' ', unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r):
		case strings.ContainsRune("_.:/=+-@", r):
		default:
			return false
		}
	}
	return true
}

// ceilMillis rounds t up to the millisecond, in UTC: the precision of
// object lock dates.
func ceilMillis(t time.Time) time.Time {
	t = t.UTC()
	if r := t.Truncate(time.Millisecond); r.Before(t) {
		return r.Add(time.Millisecond)
	}
	return t
}

// encodeMeta returns the remote form of meta (see "Where metadata lives").
func encodeMeta(meta storage.ObjectMeta, o encodeOpts) encoded {
	h := http.Header{}
	var x metaPatch
	native := func(name, v string, ext **string) {
		switch {
		case v == "":
		case safeHeaderValue(v) && !(name == "Content-Type" && v == defaultContentType):
			h.Set(name, v)
		default:
			*ext = &v
		}
	}
	native("Content-Type", meta.ContentType, &x.ContentType)
	native("Content-Encoding", meta.ContentEncoding, &x.ContentEncoding)
	native("Content-Disposition", meta.ContentDisposition, &x.ContentDisposition)
	native("Content-Language", meta.ContentLanguage, &x.ContentLanguage)
	native("Cache-Control", meta.CacheControl, &x.CacheControl)
	native("Expires", meta.Expires, &x.Expires)
	userBytes := 0
	for k, v := range meta.UserMeta {
		if safeMetaName(k) && safeHeaderValue(v) {
			h.Set("X-Amz-Meta-"+k, v)
			userBytes += len(k) + len(v)
			continue
		}
		if x.UserMeta == nil {
			x.UserMeta = map[string]string{}
		}
		x.UserMeta[k] = v
	}
	var e encoded
	if len(meta.Tags) > 0 {
		if o.tagging && nativeTags(meta.Tags) {
			h.Set(hdrTagging, s3client.EncodeTagging(meta.Tags))
			e.tags = true
		} else {
			x.Tags = mapPtr(meta.Tags)
		}
	}
	if o.keepETag {
		etag := meta.ETag
		x.ETag = &etag
	}
	opt := func(dst **string, v string) {
		if v != "" {
			*dst = &v
		}
	}
	opt(&x.StorageClass, meta.StorageClass)
	if len(meta.Checksums) > 0 {
		x.Checksums = mapPtr(meta.Checksums)
	}
	opt(&x.ChecksumType, meta.ChecksumType)
	if ps := rleEncode(meta.PartSizes); ps != nil {
		x.PartSizes = &ps
	}
	if len(meta.PartChecksums) > 0 {
		pc := clonePartChecksums(meta.PartChecksums)
		x.PartChecksums = &pc
	}
	opt(&x.SSE, meta.SSE)
	opt(&x.SSEKMSKeyID, meta.SSEKMSKeyID)
	if o.lock {
		if meta.RetentionMode != "" && !meta.RetainUntil.IsZero() {
			h.Set(hdrLockMode, meta.RetentionMode)
			h.Set(hdrLockUntil, ceilMillis(meta.RetainUntil).Format(retainDateLayout))
			if ru := meta.RetainUntil; !ceilMillis(ru).Equal(ru) {
				x.RetainPrecise = &ru
			}
			e.lock = true
		}
		if meta.LegalHold {
			h.Set(hdrLegalHold, "ON")
			e.lock = true
		}
	} else {
		opt(&x.RetentionMode, meta.RetentionMode)
		if !meta.RetainUntil.IsZero() {
			ru := meta.RetainUntil
			x.RetainUntil = &ru
		}
		if meta.LegalHold {
			lh := true
			x.LegalHold = &lh
		}
	}

	// The user metadata limit applies to the native entries and gos3-meta
	// together. What does not fit moves to the overflow, per-part
	// information first.
	limit := maxUserMetaBytes - userBytes - len(metaName)
	v := encodeExt(&x)
	var over metaPatch
	opt(&over.ACL, meta.ACL)
	opt(&over.Redirect, meta.WebsiteRedirect)
	if len(v) > limit && x.PartChecksums != nil {
		over.PartChecksums, x.PartChecksums = x.PartChecksums, nil
		v = encodeExt(&x)
	}
	if len(v) > limit && x.PartSizes != nil {
		over.PartSizes, x.PartSizes = x.PartSizes, nil
		v = encodeExt(&x)
	}
	if len(v) > limit {
		over = over.merge(&x)
		v = ""
	}
	if !over.empty() {
		e.overflow = &over
	}
	if v != "" {
		h.Set("X-Amz-Meta-"+metaName, v)
	}
	e.header = h
	return e
}

// encodeExt returns the gos3-meta header value of x ("" when x is empty).
func encodeExt(x *metaPatch) string {
	data, err := json.Marshal(x)
	if err != nil || string(data) == "{}" {
		return "" // json.Marshal of this type cannot fail
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// decodeExt parses a gos3-meta header value.
func decodeExt(v string) (*metaPatch, bool) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, false
	}
	var x metaPatch
	if err := json.Unmarshal(data, &x); err != nil {
		return nil, false
	}
	return &x, true
}

// rleEncode run-length encodes part sizes as (size, count) pairs, so the
// sizes of an upload of equal parts take a few bytes.
func rleEncode(sizes []int64) []int64 {
	if len(sizes) == 0 {
		return nil
	}
	var out []int64
	for i := 0; i < len(sizes); {
		j := i
		for j < len(sizes) && sizes[j] == sizes[i] {
			j++
		}
		out = append(out, sizes[i], int64(j-i))
		i = j
	}
	return out
}

// rleDecode reverses rleEncode (nil for malformed input).
func rleDecode(rle []int64) []int64 {
	if len(rle) == 0 || len(rle)%2 != 0 {
		return nil
	}
	var out []int64
	for i := 0; i < len(rle); i += 2 {
		if rle[i+1] <= 0 || rle[i+1] > s3client.MaxParts || len(out)+int(rle[i+1]) > s3client.MaxParts {
			return nil
		}
		for range rle[i+1] {
			out = append(out, rle[i])
		}
	}
	return out
}

// --- decoding ---

// headerMeta is what a HEAD/GET answer says about an object's metadata.
type headerMeta struct {
	// meta holds the native metadata: content headers, user metadata,
	// the remote ETag and storage class.
	meta storage.ObjectMeta
	// ext is the decoded gos3-meta header (nil without one).
	ext *metaPatch
	// tagCount is x-amz-tagging-count.
	tagCount int
	// Native object lock settings, when the answer had them.
	lockMode     string
	lockUntil    time.Time
	hasRetention bool
	legalHold    bool
	hasLegalHold bool
}

// decodeHeaders reads the metadata of a HEAD/GET answer. A malformed
// gos3-meta header is reported by ok = false (and otherwise ignored).
func decodeHeaders(a s3client.ObjectAttrs) (hm headerMeta, ok bool) {
	ok = true
	m := &hm.meta
	m.ETag = a.ETag
	m.StorageClass = normalizeStorageClass(a.StorageClass)
	h := a.Header
	m.ContentType = h.Get("Content-Type")
	if m.ContentType == defaultContentType {
		m.ContentType = ""
	}
	m.ContentEncoding = h.Get("Content-Encoding")
	m.ContentDisposition = h.Get("Content-Disposition")
	m.ContentLanguage = h.Get("Content-Language")
	m.CacheControl = h.Get("Cache-Control")
	m.Expires = h.Get("Expires")
	for k, vs := range h {
		name, found := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-")
		if !found || name == "" {
			continue
		}
		v := strings.Join(vs, ",")
		if name == metaName {
			if x, good := decodeExt(v); good {
				hm.ext = x
			} else {
				ok = false
			}
			continue
		}
		if m.UserMeta == nil {
			m.UserMeta = map[string]string{}
		}
		m.UserMeta[name] = v
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h.Get(hdrTaggingCount))); err == nil && n > 0 {
		hm.tagCount = n
	}
	if mode := strings.TrimSpace(h.Get(hdrLockMode)); mode != "" {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(h.Get(hdrLockUntil))); err == nil {
			hm.lockMode, hm.lockUntil, hm.hasRetention = strings.ToUpper(mode), t.UTC(), true
		}
	}
	if lh := strings.TrimSpace(h.Get(hdrLegalHold)); lh != "" {
		hm.legalHold, hm.hasLegalHold = strings.EqualFold(lh, "ON"), true
	}
	return hm, ok
}

// truncTime returns t in UTC truncated to whole seconds, the precision of
// HTTP dates: HEAD (Last-Modified) and listings (ISO 8601 with
// milliseconds) then report the same instant.
func truncTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Second)
}

// normalizeStorageClass maps the remote's STANDARD to "".
func normalizeStorageClass(sc string) string {
	if sc == "STANDARD" {
		return ""
	}
	return sc
}

// cloneMeta deep-copies m.
func cloneMeta(m storage.ObjectMeta) storage.ObjectMeta {
	m.UserMeta = maps.Clone(m.UserMeta)
	m.Tags = maps.Clone(m.Tags)
	m.Checksums = maps.Clone(m.Checksums)
	m.PartSizes = slices.Clone(m.PartSizes)
	m.PartChecksums = clonePartChecksums(m.PartChecksums)
	return m
}

// versionOf returns the gos3 version ID of a remote version ID: the
// remote's, or "null" when it reported none.
func versionOf(remote string) string {
	if remote == "" {
		return storage.NullVersionID
	}
	return remote
}
