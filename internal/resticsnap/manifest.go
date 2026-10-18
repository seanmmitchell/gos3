package resticsnap

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// The manifest (/gos3-manifest.jsonl of a snapshot) is JSON lines: a
// header (manifestHeader), then one entry per key in ascending key order:
//
//	{"k":"photos/a.jpg","p":"objects/photos/a.jpg","v":"3HL4kqtJ...","lm":"2024-06-15T12:00:00.123Z",
//	 "s":1234,"e":"9b2cf535f27731c974343645a3985328","mt":1718452800,"fp":"5d41402abc4b2a76",
//	 "m":{"contentType":"image/jpeg","userMeta":{"camera":"x"}}}
//	{"k":"deleted","v":"null","lm":"…","dm":true,"fp":"…"}
//
// k is the key ("kb" holds it in base64 when it is not valid UTF-8), p the
// file's path in the snapshot, v the version ID, lm its LastModified, s its
// size, e its ETag, mt the file's modification time (Unix seconds), fp a
// fingerprint of the version's identity and m its metadata. A key whose
// current version was a delete marker has dm set and no file. The manifest
// is authoritative: restores find each key's file by its path.

// manifestFormat and bucketDocFormat identify the documents of a snapshot.
const (
	manifestFormat  = "gos3-snapshot-manifest"
	bucketDocFormat = "gos3-snapshot-bucket"
	formatVersion   = 1
)

// manifestHeader is the manifest's first line.
type manifestHeader struct {
	Format     string    `json:"gos3"`
	Version    int       `json:"version"`
	Bucket     string    `json:"bucket"`
	BucketID   string    `json:"bucketId,omitempty"`
	AsOf       time.Time `json:"asOf"`
	Prefix     string    `json:"prefix,omitempty"`
	Consistent bool      `json:"consistent"`
	Versioning string    `json:"versioning,omitempty"`
	// Keys and Bytes count the manifest's entries (delete markers
	// included) and their sizes.
	Keys  int64 `json:"keys"`
	Bytes int64 `json:"bytes"`
}

// entry is one key of a snapshot: a line of the manifest, and of the run's
// intermediate files (the listing, the plan), which add internal fields.
type entry struct {
	Key          string    `json:"k"`
	KeyBase64    string    `json:"kb,omitempty"`
	Path         string    `json:"p,omitempty"`
	VersionID    string    `json:"v"`
	LastModified time.Time `json:"lm"`
	Size         int64     `json:"s"`
	ETag         string    `json:"e,omitempty"`
	DeleteMarker bool      `json:"dm,omitempty"`
	MTime        int64     `json:"mt,omitempty"`
	Fingerprint  string    `json:"fp,omitempty"`
	Meta         *meta     `json:"m,omitempty"`

	// Action is the plan's decision (planKeep, planMaterialize,
	// planMarker); never in a manifest.
	Action string `json:"act,omitempty"`
	// ParentMTime is the parent's modification time of the file at the
	// same path (0: none); never in a manifest.
	ParentMTime int64 `json:"pmt,omitempty"`
}

// meta is an entry's object metadata (storage.ObjectMeta without the ETag
// and the transient multipart fields), plus how the version was stored
// compressed.
type meta struct {
	ContentType        string              `json:"contentType,omitempty"`
	ContentEncoding    string              `json:"contentEncoding,omitempty"`
	ContentDisposition string              `json:"contentDisposition,omitempty"`
	ContentLanguage    string              `json:"contentLanguage,omitempty"`
	CacheControl       string              `json:"cacheControl,omitempty"`
	Expires            string              `json:"expires,omitempty"`
	UserMeta           map[string]string   `json:"userMeta,omitempty"`
	Tags               map[string]string   `json:"tags,omitempty"`
	StorageClass       string              `json:"storageClass,omitempty"`
	ACL                string              `json:"acl,omitempty"`
	Checksums          map[string]string   `json:"checksums,omitempty"`
	ChecksumType       string              `json:"checksumType,omitempty"`
	PartSizes          []int64             `json:"partSizes,omitempty"`
	PartChecksums      []map[string]string `json:"partChecksums,omitempty"`
	Encryption         string              `json:"encryption,omitempty"`
	SSE                string              `json:"sse,omitempty"`
	SSEKMSKeyID        string              `json:"sseKmsKeyId,omitempty"`
	RetentionMode      string              `json:"retentionMode,omitempty"`
	RetainUntil        time.Time           `json:"retainUntil,omitzero"`
	LegalHold          bool                `json:"legalHold,omitempty"`
	WebsiteRedirect    string              `json:"websiteRedirect,omitempty"`
	Compression        string              `json:"compression,omitempty"`
}

// Plan actions (entry.Action).
const (
	planKeep        = "keep"        // unchanged: a placeholder
	planMaterialize = "materialize" // read from the bucket
	planMarker      = "marker"      // a delete marker: no file
)

// entryOf returns the entry of version oi (Path, MTime, Fingerprint unset).
func entryOf(oi storage.ObjectInfo) entry {
	e := entry{
		Key:          oi.Key,
		VersionID:    oi.VersionID,
		LastModified: oi.LastModified.UTC(),
		DeleteMarker: oi.DeleteMarker,
	}
	if !oi.DeleteMarker {
		e.Size, e.ETag = oi.Size, oi.ETag
		if m := metaOf(oi.ObjectMeta, oi.Compression); !isZeroMeta(m) {
			e.Meta = &m
		}
	}
	e.Fingerprint = fingerprint(e)
	return e
}

// metaOf converts object metadata.
func metaOf(m storage.ObjectMeta, compression string) meta {
	out := meta{
		ContentType: m.ContentType, ContentEncoding: m.ContentEncoding, ContentDisposition: m.ContentDisposition,
		ContentLanguage: m.ContentLanguage, CacheControl: m.CacheControl, Expires: m.Expires,
		UserMeta: m.UserMeta, Tags: m.Tags, StorageClass: m.StorageClass, ACL: m.ACL,
		Checksums: m.Checksums, ChecksumType: m.ChecksumType, PartSizes: m.PartSizes, PartChecksums: m.PartChecksums,
		Encryption: m.Encryption, SSE: m.SSE, SSEKMSKeyID: m.SSEKMSKeyID,
		RetentionMode: m.RetentionMode, LegalHold: m.LegalHold, WebsiteRedirect: m.WebsiteRedirect,
		Compression: compression,
	}
	if !m.RetainUntil.IsZero() {
		out.RetainUntil = m.RetainUntil.UTC()
	}
	return out
}

// isZeroMeta reports whether m holds nothing.
func isZeroMeta(m meta) bool {
	return m.ContentType == "" && m.ContentEncoding == "" && m.ContentDisposition == "" && m.ContentLanguage == "" &&
		m.CacheControl == "" && m.Expires == "" && len(m.UserMeta) == 0 && len(m.Tags) == 0 && m.StorageClass == "" &&
		m.ACL == "" && len(m.Checksums) == 0 && m.ChecksumType == "" && len(m.PartSizes) == 0 && len(m.PartChecksums) == 0 &&
		m.Encryption == "" && m.SSE == "" && m.SSEKMSKeyID == "" && m.RetentionMode == "" && m.RetainUntil.IsZero() &&
		!m.LegalHold && m.WebsiteRedirect == "" && m.Compression == ""
}

// objectInfo returns the version e describes, for writing it to a bucket.
func (e *entry) objectInfo() storage.ObjectInfo {
	oi := storage.ObjectInfo{
		Key: e.Key, VersionID: e.VersionID, LastModified: e.LastModified,
		DeleteMarker: e.DeleteMarker, Size: e.Size,
	}
	if e.DeleteMarker {
		return oi
	}
	oi.ETag = e.ETag
	if m := e.Meta; m != nil {
		oi.ContentType, oi.ContentEncoding, oi.ContentDisposition = m.ContentType, m.ContentEncoding, m.ContentDisposition
		oi.ContentLanguage, oi.CacheControl, oi.Expires = m.ContentLanguage, m.CacheControl, m.Expires
		oi.UserMeta, oi.Tags, oi.StorageClass, oi.ACL = m.UserMeta, m.Tags, m.StorageClass, m.ACL
		oi.Checksums, oi.ChecksumType, oi.PartSizes, oi.PartChecksums = m.Checksums, m.ChecksumType, m.PartSizes, m.PartChecksums
		oi.Encryption, oi.SSE, oi.SSEKMSKeyID = m.Encryption, m.SSE, m.SSEKMSKeyID
		oi.RetentionMode, oi.RetainUntil, oi.LegalHold = m.RetentionMode, m.RetainUntil, m.LegalHold
		oi.WebsiteRedirect, oi.Compression = m.WebsiteRedirect, m.Compression
	}
	return oi
}

// fingerprint returns the fingerprint of the identity of e's version: its
// ID, LastModified, size, ETag and delete-marker flag. A file is staged
// unchanged (a placeholder) only when its fingerprint and path equal the
// parent's.
func fingerprint(e entry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n%d\n%s\n%t", e.VersionID, e.LastModified.UnixNano(), e.Size, e.ETag, e.DeleteMarker)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// mtimeFor returns the modification time (Unix seconds) of the staged file
// of a version last modified at lm: lm to the second, plus a second when
// that equals parent, the parent's time of the file at the same path, so
// that restic reads the new content.
func mtimeFor(lm time.Time, parent int64) int64 {
	mt := lm.Unix()
	if parent != 0 && mt == parent {
		mt++
	}
	return mt
}

// prepareWrite readies e for encoding (a key that is not valid UTF-8,
// which JSON cannot hold, goes to KeyBase64).
func (e *entry) prepareWrite() {
	if !utf8.ValidString(e.Key) {
		e.KeyBase64 = base64.StdEncoding.EncodeToString([]byte(e.Key))
	}
}

// finishRead restores a key written in base64.
func (e *entry) finishRead() error {
	if e.KeyBase64 == "" {
		return nil
	}
	k, err := base64.StdEncoding.DecodeString(e.KeyBase64)
	if err != nil {
		return fmt.Errorf("invalid key %q: %w", e.KeyBase64, err)
	}
	e.Key = string(k)
	return nil
}

// manifestEntry returns e as a manifest line (without the plan's fields).
func (e entry) manifestEntry() entry {
	e.Action, e.ParentMTime = "", 0
	return e
}

// jsonlWriter writes JSON lines to a file.
type jsonlWriter struct {
	f      *os.File
	w      *bufio.Writer
	enc    *json.Encoder
	closed bool
}

// createJSONL creates (or truncates) the file path for writing.
func createJSONL(path string) (*jsonlWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &jsonlWriter{f: f, w: w, enc: enc}, nil
}

// write writes v as one line.
func (j *jsonlWriter) write(v any) error {
	if e, ok := v.(*entry); ok {
		e.prepareWrite()
	}
	return j.enc.Encode(v)
}

// flush writes the buffered lines to the file.
func (j *jsonlWriter) flush() error { return j.w.Flush() }

// close flushes and closes the file; closing again does nothing.
func (j *jsonlWriter) close() error {
	if j == nil || j.closed {
		return nil
	}
	j.closed = true
	err := j.w.Flush()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// jsonlReader reads JSON lines.
type jsonlReader struct {
	c    io.Closer
	r    *bufio.Reader
	line int
}

// openJSONL opens the file path for reading.
func openJSONL(path string) (*jsonlReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &jsonlReader{c: f, r: bufio.NewReaderSize(f, 256<<10)}, nil
}

// next decodes the next non-blank line into v; it reports false at the end.
func (j *jsonlReader) next(v any) (bool, error) {
	for {
		line, err := j.r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			j.line++
			if derr := json.Unmarshal(line, v); derr != nil {
				return false, fmt.Errorf("line %d: %w", j.line, derr)
			}
			if e, ok := v.(*entry); ok {
				if err := e.finishRead(); err != nil {
					return false, fmt.Errorf("line %d: %w", j.line, err)
				}
			}
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// close closes the file.
func (j *jsonlReader) close() error { return j.c.Close() }

// errBadManifest is wrapped by the errors for manifests that cannot be
// used.
var errBadManifest = errors.New("resticsnap: unusable snapshot manifest")

// readHeader reads and checks a manifest's header.
func readHeader(r *jsonlReader) (manifestHeader, error) {
	var h manifestHeader
	ok, err := r.next(&h)
	switch {
	case err != nil:
		return h, fmt.Errorf("%w: %v", errBadManifest, err)
	case !ok:
		return h, fmt.Errorf("%w: empty", errBadManifest)
	case h.Format != manifestFormat:
		return h, fmt.Errorf("%w: not a gos3 manifest", errBadManifest)
	case h.Version != formatVersion:
		return h, fmt.Errorf("%w: format version %d (this build reads %d)", errBadManifest, h.Version, formatVersion)
	}
	return h, nil
}

// checkManifest checks that the manifest file path has a valid header and
// strictly ascending keys, with a path for every entry but delete markers.
func checkManifest(path string) error {
	r, err := openJSONL(path)
	if err != nil {
		return err
	}
	defer r.close()
	if _, err := readHeader(r); err != nil {
		return err
	}
	var prev string
	for n := 0; ; n++ {
		var e entry
		ok, err := r.next(&e)
		if err != nil {
			return fmt.Errorf("%w: %v", errBadManifest, err)
		}
		if !ok {
			return nil
		}
		if n > 0 && e.Key <= prev {
			return fmt.Errorf("%w: keys out of order at line %d", errBadManifest, r.line)
		}
		if !e.DeleteMarker && !validEntryPath(e.Path) {
			return fmt.Errorf("%w: invalid path %q at line %d", errBadManifest, e.Path, r.line)
		}
		prev = e.Key
	}
}

// validEntryPath reports whether p is a clean relative path below
// ObjectsDir.
func validEntryPath(p string) bool {
	if len(p) <= len(ObjectsDir)+1 || p[:len(ObjectsDir)+1] != ObjectsDir+"/" {
		return false
	}
	for _, seg := range bytes.Split([]byte(p), []byte("/")) {
		if len(seg) == 0 || string(seg) == "." || string(seg) == ".." || bytes.IndexByte(seg, '\\') >= 0 {
			return false
		}
	}
	return true
}

// bucketDoc is /gos3-bucket.json: the bucket's description and its
// configuration documents when the snapshot was taken.
type bucketDoc struct {
	Format  string               `json:"gos3"`
	Version int                  `json:"version"`
	Bucket  bucketInfo           `json:"bucket"`
	AsOf    time.Time            `json:"asOf"`
	Configs map[string]configDoc `json:"configs"`
}

// bucketInfo describes the bucket (storage.BucketInfo).
type bucketInfo struct {
	Name              string    `json:"name"`
	ID                string    `json:"id,omitempty"`
	CreatedAt         time.Time `json:"createdAt,omitzero"`
	Region            string    `json:"region,omitempty"`
	Versioning        string    `json:"versioning,omitempty"`
	ObjectLockEnabled bool      `json:"objectLockEnabled,omitempty"`
	Owner             string    `json:"owner,omitempty"`
	Backing           string    `json:"backing,omitempty"`
	Kind              string    `json:"kind,omitempty"`
	Location          string    `json:"location,omitempty"`
}

// configDoc is a configuration document: its text, or its bytes in base64
// (Encoding "base64") when it is not valid UTF-8.
type configDoc struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
}

// newConfigDoc returns the document of data.
func newConfigDoc(data []byte) configDoc {
	if utf8.Valid(data) {
		return configDoc{Data: string(data)}
	}
	return configDoc{Data: base64.StdEncoding.EncodeToString(data), Encoding: "base64"}
}

// bytes returns the document's content.
func (d configDoc) bytes() ([]byte, error) {
	switch d.Encoding {
	case "":
		return []byte(d.Data), nil
	case "base64":
		return base64.StdEncoding.DecodeString(d.Data)
	}
	return nil, fmt.Errorf("unknown encoding %q", d.Encoding)
}
