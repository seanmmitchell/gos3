package gateway

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Metadata mapping
//
// Content headers (Content-Type, -Encoding, -Disposition, -Language,
// Cache-Control, Expires) and user metadata travel as native S3 headers,
// so other S3 clients see them. Everything gos3-specific that S3 has no
// native header for, and every native value that cannot survive an HTTP
// header round trip unchanged (non-ASCII, empty or space-padded values,
// odd user metadata names), travels in ONE reserved user metadata entry,
// x-amz-meta-gos3-meta, holding base64url(JSON(extMeta)). The entry is
// omitted when there is nothing to put in it, so objects written with
// plain metadata look exactly like objects written by any other client.
//
// Storage class, SSE and object lock settings are never sent as native
// headers: the remote may reject them or apply them with real effects
// (archival classes, KMS keys it does not have).

// metaName is the reserved user metadata name (x-amz-meta-gos3-meta).
const metaName = "gos3-meta"

// maxMetaHeader is the size above which the encoded gos3-meta value drops
// per-part information (S3 limits user metadata to 2 KiB in total).
const maxMetaHeader = 1536

// defaultContentType is what S3 services report for objects stored
// without a Content-Type; it is reported as "" (the S3 layer renders ""
// as this value again).
const defaultContentType = "binary/octet-stream"

// extMeta is the JSON document of the gos3-meta entry. Field names are
// short to keep the header small.
type extMeta struct {
	// ETag overrides the remote ETag (it differs when gos3 uploaded the
	// object in parts, after a metadata rewrite, or when the S3 layer's
	// ETag is not the MD5 of the content).
	ETag *string `json:"e,omitempty"`
	// Content headers that could not be sent natively.
	ContentType        *string `json:"ct,omitempty"`
	ContentEncoding    *string `json:"ce,omitempty"`
	ContentDisposition *string `json:"cd,omitempty"`
	ContentLanguage    *string `json:"cl,omitempty"`
	CacheControl       *string `json:"cc,omitempty"`
	Expires            *string `json:"ex,omitempty"`
	// UserMeta holds user metadata entries that could not be sent natively.
	UserMeta      map[string]string   `json:"um,omitempty"`
	Tags          map[string]string   `json:"t,omitempty"`
	StorageClass  string              `json:"sc,omitempty"`
	ACL           string              `json:"acl,omitempty"`
	Checksums     map[string]string   `json:"cs,omitempty"`
	ChecksumType  string              `json:"cst,omitempty"`
	PartSizes     []int64             `json:"ps,omitempty"` // run-length encoded: size, count, size, count, ...
	PartChecksums []map[string]string `json:"pc,omitempty"`
	SSE           string              `json:"sse,omitempty"`
	SSEKMSKeyID   string              `json:"kms,omitempty"`
	RetentionMode string              `json:"rm,omitempty"`
	RetainUntil   *time.Time          `json:"ru,omitempty"`
	LegalHold     bool                `json:"lh,omitempty"`
	Redirect      string              `json:"wr,omitempty"`
	// LastModified (Unix seconds) preserves the original modification time
	// across metadata rewrites (server-side copies onto the object itself).
	LastModified int64 `json:"lm,omitempty"`
}

// headerOpts are the parts of the encoded headers that do not come from
// the ObjectMeta itself.
type headerOpts struct {
	// etag stores meta.ETag in gos3-meta (the remote ETag will differ).
	etag bool
	// lastModified, when non-zero, is preserved in gos3-meta.
	lastModified time.Time
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

// encodeHeaders returns the remote object headers for meta.
func encodeHeaders(meta storage.ObjectMeta, o headerOpts) http.Header {
	h := http.Header{}
	var x extMeta
	native := func(name, v string, ext **string) {
		switch {
		case v == "":
		case safeHeaderValue(v):
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
	for k, v := range meta.UserMeta {
		if safeMetaName(k) && safeHeaderValue(v) {
			h.Set("X-Amz-Meta-"+k, v)
			continue
		}
		if x.UserMeta == nil {
			x.UserMeta = map[string]string{}
		}
		x.UserMeta[k] = v
	}
	if o.etag {
		etag := meta.ETag
		x.ETag = &etag
	}
	x.Tags = meta.Tags
	x.StorageClass = meta.StorageClass
	x.ACL = meta.ACL
	x.Checksums = meta.Checksums
	x.ChecksumType = meta.ChecksumType
	x.PartSizes = rleEncode(meta.PartSizes)
	x.PartChecksums = meta.PartChecksums
	x.SSE = meta.SSE
	x.SSEKMSKeyID = meta.SSEKMSKeyID
	x.RetentionMode = meta.RetentionMode
	if !meta.RetainUntil.IsZero() {
		ru := meta.RetainUntil
		x.RetainUntil = &ru
	}
	x.LegalHold = meta.LegalHold
	x.Redirect = meta.WebsiteRedirect
	if !o.lastModified.IsZero() {
		x.LastModified = o.lastModified.Unix()
	}

	v := encodeExt(&x)
	if len(v) > maxMetaHeader && x.PartChecksums != nil {
		x.PartChecksums = nil
		v = encodeExt(&x)
	}
	if len(v) > maxMetaHeader && x.PartSizes != nil {
		x.PartSizes = nil
		v = encodeExt(&x)
	}
	if v != "" {
		h.Set("X-Amz-Meta-"+metaName, v)
	}
	return h
}

// encodeExt returns the gos3-meta header value of x ("" when x is empty).
func encodeExt(x *extMeta) string {
	data, err := json.Marshal(x)
	if err != nil || string(data) == "{}" {
		return "" // json.Marshal of this type cannot fail
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// decodeExt parses a gos3-meta header value.
func decodeExt(v string) (*extMeta, bool) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, false
	}
	var x extMeta
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

// objectInfo builds the gos3 view of a remote object from HEAD/GET
// attributes.
func (s *Store) objectInfo(bucket, key string, a s3client.ObjectAttrs) storage.ObjectInfo {
	info := storage.ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		VersionID:    storage.NullVersionID,
		IsLatest:     true,
		Size:         a.Size,
		LastModified: truncTime(a.LastModified),
	}
	m := &info.ObjectMeta
	m.ETag = a.ETag
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
	var x *extMeta
	for k, vs := range h {
		lk := strings.ToLower(k)
		name, ok := strings.CutPrefix(lk, "x-amz-meta-")
		if !ok || name == "" {
			continue
		}
		v := strings.Join(vs, ",")
		if name == metaName {
			if px, ok := decodeExt(v); ok {
				x = px
			} else {
				s.log.Warn("gateway: ignoring a malformed gos3-meta header", "backing", s.name, "bucket", bucket, "key", key)
			}
			continue
		}
		if m.UserMeta == nil {
			m.UserMeta = map[string]string{}
		}
		m.UserMeta[name] = v
	}
	if x == nil {
		// Written by another client (or by gos3 without anything to add).
		m.StorageClass = normalizeStorageClass(a.StorageClass)
		return info
	}
	if x.ETag != nil {
		m.ETag = *x.ETag
	}
	override := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	override(&m.ContentType, x.ContentType)
	override(&m.ContentEncoding, x.ContentEncoding)
	override(&m.ContentDisposition, x.ContentDisposition)
	override(&m.ContentLanguage, x.ContentLanguage)
	override(&m.CacheControl, x.CacheControl)
	override(&m.Expires, x.Expires)
	if len(x.UserMeta) > 0 {
		if m.UserMeta == nil {
			m.UserMeta = map[string]string{}
		}
		maps.Copy(m.UserMeta, x.UserMeta)
	}
	m.Tags = x.Tags
	m.StorageClass = x.StorageClass
	m.ACL = x.ACL
	m.Checksums = x.Checksums
	m.ChecksumType = x.ChecksumType
	m.PartSizes = rleDecode(x.PartSizes)
	m.PartChecksums = x.PartChecksums
	if m.PartChecksums != nil && len(m.PartChecksums) != len(m.PartSizes) {
		m.PartChecksums = nil
	}
	m.SSE = x.SSE
	m.SSEKMSKeyID = x.SSEKMSKeyID
	m.RetentionMode = x.RetentionMode
	if x.RetainUntil != nil {
		m.RetainUntil = *x.RetainUntil
	}
	m.LegalHold = x.LegalHold
	m.WebsiteRedirect = x.Redirect
	if x.LastModified != 0 {
		info.LastModified = time.Unix(x.LastModified, 0).UTC()
	}
	return info
}

// listedInfo builds the gos3 view of a listing entry. Listings carry no
// headers, so the metadata is limited to what ListObjectsV2 reports: Size,
// LastModified, StorageClass and the REMOTE ETag (which differs from the
// ETag HEAD/GET report for objects whose gos3 ETag is kept in gos3-meta,
// e.g. objects gos3 uploaded in parts; LastModified likewise differs after
// a metadata rewrite). User metadata, tags, checksums etc. are not
// available without a HEAD per object, which listings deliberately avoid.
func listedInfo(bucket, key string, a s3client.ObjectAttrs) storage.ObjectInfo {
	return storage.ObjectInfo{
		ObjectMeta: storage.ObjectMeta{
			ETag:         a.ETag,
			StorageClass: normalizeStorageClass(a.StorageClass),
		},
		Bucket:       bucket,
		Key:          key,
		VersionID:    storage.NullVersionID,
		IsLatest:     true,
		Size:         a.Size,
		LastModified: truncTime(a.LastModified),
	}
}

// cloneMeta deep-copies m.
func cloneMeta(m storage.ObjectMeta) storage.ObjectMeta {
	m.UserMeta = maps.Clone(m.UserMeta)
	m.Tags = maps.Clone(m.Tags)
	m.Checksums = maps.Clone(m.Checksums)
	m.PartSizes = slices.Clone(m.PartSizes)
	if m.PartChecksums != nil {
		pc := make([]map[string]string, len(m.PartChecksums))
		for i, c := range m.PartChecksums {
			pc[i] = maps.Clone(c)
		}
		m.PartChecksums = pc
	}
	return m
}

// sameHeaders reports whether two header sets are identical.
func sameHeaders(a, b http.Header) bool {
	return reflect.DeepEqual(a, b)
}
