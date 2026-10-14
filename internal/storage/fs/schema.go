package fs

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Metadata schema (bbolt, <root>/.gos3/meta.db)
//
// Top-level buckets:
//
//	"meta"     schema information ("format" -> schemaVersion as one byte).
//	"buckets"  bucket name -> bucketRecord.
//	"uploads"  upload ID   -> bucket ID (global index of in-progress
//	           multipart uploads; used by startup recovery).
//	"b:<id>"   per-bucket data, keyed by the random bucket ID assigned at
//	           CreateBucket (so delete+recreate never collides), with the
//	           nested buckets below. Its bbolt sequence numbers versions.
//
// Nested buckets of "b:<id>":
//
//	"ver"    versionKey(key, inv) -> versionRecord. Every version and delete
//	         marker of every key. versionKey is the order-preserving escaped
//	         object key, a two-byte terminator and the big-endian inverted
//	         per-bucket sequence number, so a cursor visits keys in ascending
//	         byte order and, within a key, versions newest first.
//	"cur"    object key -> versionRecord of the latest version, present only
//	         when that latest version is not a delete marker. WalkObjects
//	         iterates this bucket directly.
//	"null"   object key -> 8-byte inverted sequence of the key's null version.
//	"dat"    data ID -> versionKey of the version owning that data file
//	         (reverse index used by orphan detection).
//	"cfg"    configuration key -> opaque document bytes.
//	"upl"    upload ID -> uploadRecord.
//	"parts"  upload ID + 4-byte big-endian part number -> partRecord.
//	"upk"    uploadOrderKey(key, initiated, upload ID) -> upload ID: the
//	         uploads in listing order (key, initiation time, upload ID).
//	         Created and checked against "upl" at Open, so databases
//	         written before it existed are indexed on first use.
//	"imp"    importedKey(key, version ID) -> 8-byte inverted sequence of the
//	         version: the positions of versions stored with a version ID
//	         taken from another backend (storage.VersionImporter, see
//	         import.go), which unlike generated IDs does not embed the
//	         sequence. Every non-null imported version has an entry; null
//	         imports use "null". Created by the first import into the
//	         bucket (so it is optional: absent means no imported versions,
//	         and databases written before it existed need no migration);
//	         versionPosition consults it before parsing an ID and
//	         removeVersion deletes the entry of a removed version.
//
// Record values are encoded as one format byte followed by the encoded
// record struct:
//
//	recordFormatJSON (1): JSON. Used for every record whose strings are all
//	                      valid UTF-8 (practically always).
//	recordFormatGob  (2): encoding/gob. Used when some string (e.g. a user
//	                      metadata value taken from raw header bytes) is not
//	                      valid UTF-8, which JSON would silently replace
//	                      with U+FFFD; gob stores strings byte-exactly.
//	recordFormatVersionV1 (3): compact binary encoding, always used for
//	                      versionRecord (see codec.go); read in bulk by walks.
//	recordFormatVersionV2 (4): recordFormatVersionV1 plus the compression
//	                      fields, used only for versions stored compressed
//	                      (so other records stay readable by older builds).
//
// The format byte also allows switching encodings later without migrating
// existing values.
const (
	schemaVersion    byte = 1
	recordFormatJSON byte = 1
	recordFormatGob  byte = 2
)

var (
	topMeta    = []byte("meta")
	topBuckets = []byte("buckets")
	topUploads = []byte("uploads")

	subVersions = []byte("ver")
	subCurrent  = []byte("cur")
	subNull     = []byte("null")
	subData     = []byte("dat")
	subConfig   = []byte("cfg")
	subUploads  = []byte("upl")
	subParts    = []byte("parts")
	// subUploadOrder is the listing-order index of uploads (uploadindex.go).
	subUploadOrder = []byte("upk")
	// subImported indexes imported version IDs (import.go). It is created
	// lazily by the first import, so it is not in perBucketSubs.
	subImported = []byte("imp")

	// perBucketSubs are the nested buckets created with every bucket.
	perBucketSubs = [][]byte{subVersions, subCurrent, subNull, subData, subConfig, subUploads, subParts, subUploadOrder}

	keyFormat = []byte("format")
)

// Limits on names the backend accepts. Object keys are limited to 1024 bytes
// by the S3 layer; the backend is more lenient but bounded so that escaped
// keys always fit bbolt's 32 KiB key limit.
const (
	maxKeyLen        = 8 << 10
	maxBucketNameLen = 1024
	maxConfigKeyLen  = 1024
	maxPartNumber    = 10000
)

// bucketPrefix is prepended to a bucket ID to name its top-level bbolt bucket.
const bucketPrefix = "b:"

func bucketDataName(id string) []byte { return []byte(bucketPrefix + id) }

// bucketRecord is the persisted form of a bucket.
type bucketRecord struct {
	ID                string                   `json:"id"`
	CreatedAt         time.Time                `json:"created"`
	Region            string                   `json:"region,omitempty"`
	Versioning        storage.VersioningStatus `json:"versioning,omitempty"`
	ObjectLockEnabled bool                     `json:"objectLock,omitempty"`
	Owner             string                   `json:"owner,omitempty"`
}

func (r *bucketRecord) info(name string) storage.BucketInfo {
	return storage.BucketInfo{
		Name:              name,
		CreatedAt:         r.CreatedAt,
		Region:            r.Region,
		Versioning:        r.Versioning,
		ObjectLockEnabled: r.ObjectLockEnabled,
		Owner:             r.Owner,
	}
}

// versionRecord is the persisted form of one object version or delete marker.
type versionRecord struct {
	VersionID    string             `json:"vid"`
	DeleteMarker bool               `json:"dm,omitempty"`
	Size         int64              `json:"size"`
	LastModified time.Time          `json:"mtime"`
	DataID       string             `json:"data,omitempty"` // "" for delete markers and empty objects
	Meta         storage.ObjectMeta `json:"meta"`
	// Compression is the algorithm the data file is compressed with ("" =
	// stored as is); StoredSize is then the data file's size, while Size
	// stays the size of the content.
	Compression string `json:"cmp,omitempty"`
	StoredSize  int64  `json:"ssize,omitempty"`
}

// fileSize returns the expected size of the version's data file.
func (r *versionRecord) fileSize() int64 {
	if r.Compression != "" {
		return r.StoredSize
	}
	return r.Size
}

func (r *versionRecord) info(bucket, key string, latest bool) storage.ObjectInfo {
	oi := storage.ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		VersionID:    r.VersionID,
		IsLatest:     latest,
		DeleteMarker: r.DeleteMarker,
		Size:         r.Size,
		LastModified: r.LastModified,
	}
	if !r.DeleteMarker {
		oi.ObjectMeta = r.Meta
		oi.Compression, oi.StoredSize = r.Compression, r.StoredSize
	}
	return oi
}

// uploadRecord is the persisted form of an in-progress multipart upload.
type uploadRecord struct {
	Key       string             `json:"key"`
	Initiated time.Time          `json:"initiated"`
	Meta      storage.ObjectMeta `json:"meta"`
	Initiator string             `json:"initiator,omitempty"`
}

func (r *uploadRecord) info(bucket, uploadID string) storage.UploadInfo {
	return storage.UploadInfo{
		UploadID:  uploadID,
		Bucket:    bucket,
		Key:       r.Key,
		Initiated: r.Initiated,
		Meta:      r.Meta,
		Initiator: r.Initiator,
	}
}

// partRecord is the persisted form of an uploaded part.
type partRecord struct {
	File         string            `json:"file"` // file name inside the upload directory
	Size         int64             `json:"size"`
	LastModified time.Time         `json:"mtime"`
	ETag         string            `json:"etag"`
	Checksums    map[string]string `json:"checksums,omitempty"`
}

func (r *partRecord) info(number int) storage.PartInfo {
	return storage.PartInfo{
		PartMeta:     storage.PartMeta{ETag: r.ETag, Checksums: r.Checksums},
		PartNumber:   number,
		Size:         r.Size,
		LastModified: r.LastModified,
	}
}

var errCorrupt = errors.New("fs: corrupt metadata record")

// encodeRecord serialises v as format byte + JSON, or + gob when v holds
// strings that are not valid UTF-8.
func encodeRecord(v any) ([]byte, error) {
	if r, ok := v.(*versionRecord); ok {
		return encodeVersionRecord(r), nil
	}
	var buf bytes.Buffer
	if !recordIsUTF8(v) {
		buf.WriteByte(recordFormatGob)
		if err := gob.NewEncoder(&buf).Encode(v); err != nil {
			return nil, fmt.Errorf("fs: encode record: %w", err)
		}
		return buf.Bytes(), nil
	}
	buf.WriteByte(recordFormatJSON)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("fs: encode record: %w", err)
	}
	b := buf.Bytes()
	// Drop the trailing newline written by Encoder.
	return b[:len(b)-1], nil
}

// decodeRecord parses a value written by encodeRecord. data may point into
// the bbolt mmap; nothing in v aliases it afterwards.
func decodeRecord(data []byte, v any) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: empty record", errCorrupt)
	}
	var err error
	switch data[0] {
	case recordFormatJSON:
		err = json.Unmarshal(data[1:], v)
	case recordFormatGob:
		err = gob.NewDecoder(bytes.NewReader(data[1:])).Decode(v)
	case recordFormatVersionV1, recordFormatVersionV2:
		r, ok := v.(*versionRecord)
		if !ok {
			return fmt.Errorf("%w: version record format for %T", errCorrupt, v)
		}
		err = decodeVersionRecord(data[1:], r, data[0] == recordFormatVersionV2)
	default:
		return fmt.Errorf("%w: unknown record format %d", errCorrupt, data[0])
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errCorrupt, err)
	}
	return nil
}

// recordIsUTF8 reports whether every string in a record is valid UTF-8 (and
// thus survives a JSON round trip unchanged).
func recordIsUTF8(v any) bool {
	switch r := v.(type) {
	case *versionRecord:
		return utf8.ValidString(r.VersionID) && utf8.ValidString(r.DataID) && metaIsUTF8(&r.Meta)
	case *uploadRecord:
		return utf8.ValidString(r.Key) && utf8.ValidString(r.Initiator) && metaIsUTF8(&r.Meta)
	case *partRecord:
		return utf8.ValidString(r.File) && utf8.ValidString(r.ETag) && mapIsUTF8(r.Checksums)
	case *bucketRecord:
		return utf8.ValidString(r.ID) && utf8.ValidString(r.Region) && utf8.ValidString(string(r.Versioning)) && utf8.ValidString(r.Owner)
	}
	return false // unknown types take the exact path
}

func metaIsUTF8(m *storage.ObjectMeta) bool {
	for _, s := range []string{m.ETag, m.ContentType, m.ContentEncoding, m.ContentDisposition,
		m.ContentLanguage, m.CacheControl, m.Expires, m.StorageClass, m.ACL, m.ChecksumType,
		m.SSE, m.SSEKMSKeyID, m.RetentionMode, m.WebsiteRedirect} {
		if !utf8.ValidString(s) {
			return false
		}
	}
	if !mapIsUTF8(m.UserMeta) || !mapIsUTF8(m.Tags) || !mapIsUTF8(m.Checksums) {
		return false
	}
	for _, pc := range m.PartChecksums {
		if !mapIsUTF8(pc) {
			return false
		}
	}
	return true
}

func mapIsUTF8(m map[string]string) bool {
	for k, v := range m {
		if !utf8.ValidString(k) || !utf8.ValidString(v) {
			return false
		}
	}
	return true
}

// --- version keys ---

// escapeKey appends an order-preserving, self-delimiting encoding of key to
// dst: every 0x00 byte becomes 0x00 0xFF. Followed by the terminator 0x00
// 0x01, byte-wise order of encodings equals byte-wise order of keys and no
// encoding of one key is a prefix of another key's terminated encoding.
func escapeKey(dst []byte, key string) []byte {
	for i := 0; i < len(key); i++ {
		c := key[i]
		dst = append(dst, c)
		if c == 0 {
			dst = append(dst, 0xff)
		}
	}
	return dst
}

// keyVersionsPrefix returns the common prefix of all version keys of key.
func keyVersionsPrefix(key string) []byte {
	b := make([]byte, 0, len(key)+12)
	b = escapeKey(b, key)
	return append(b, 0x00, 0x01)
}

// keyVersionsEnd returns a seek position strictly after every version key of
// key and at or before the version keys of every greater object key.
func keyVersionsEnd(key string) []byte {
	b := make([]byte, 0, len(key)+2)
	b = escapeKey(b, key)
	return append(b, 0x00, 0x02)
}

// versionKey returns the "ver" key of the version of key with inverted
// sequence inv.
func versionKey(key string, inv uint64) []byte {
	return binary.BigEndian.AppendUint64(keyVersionsPrefix(key), inv)
}

// splitVersionKey decodes a "ver" key into object key and inverted sequence.
func splitVersionKey(k []byte) (string, uint64, bool) {
	var sb strings.Builder
	sb.Grow(len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c != 0 {
			sb.WriteByte(c)
			continue
		}
		if i+1 >= len(k) {
			return "", 0, false
		}
		switch k[i+1] {
		case 0xff:
			sb.WriteByte(0)
			i++
		case 0x01:
			rest := k[i+2:]
			if len(rest) != 8 {
				return "", 0, false
			}
			return sb.String(), binary.BigEndian.Uint64(rest), true
		default:
			return "", 0, false
		}
	}
	return "", 0, false
}

// invert maps a sequence number to its newest-first sort key.
func invert(seq uint64) uint64 { return math.MaxUint64 - seq }

func invBytes(inv uint64) []byte { return binary.BigEndian.AppendUint64(nil, inv) }

// --- identifiers ---

// versionIDLen is the length of generated version IDs: 16 bytes (8-byte
// inverted sequence + 8 random bytes) in unpadded base64url.
var versionIDLen = base64.RawURLEncoding.EncodedLen(16)

// newVersionID returns a URL-safe version ID embedding inv, so lookups by
// version ID are a single bbolt Get.
func newVersionID(inv uint64) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], inv)
	_, _ = rand.Read(b[8:]) // crypto/rand.Read never fails (Go >= 1.24)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// parseVersionID extracts the inverted sequence from an ID generated by
// newVersionID.
func parseVersionID(id string) (uint64, bool) {
	if len(id) != versionIDLen {
		return 0, false
	}
	var b [16]byte
	n, err := base64.RawURLEncoding.Decode(b[:], []byte(id))
	if err != nil || n != 16 {
		return 0, false
	}
	return binary.BigEndian.Uint64(b[:8]), true
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newUploadID returns a random URL-safe upload ID (32 characters).
func newUploadID() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// uploadIDLen is the length of IDs returned by newUploadID.
var uploadIDLen = base64.RawURLEncoding.EncodedLen(24)

// validUploadID reports whether id has the shape of a generated upload ID.
// Upload IDs come from clients and are used as directory names, so anything
// else is rejected before touching the filesystem.
func validUploadID(id string) bool {
	if len(id) != uploadIDLen {
		return false
	}
	return isURLSafe(id)
}

// bucketIDLen is the length of bucket IDs (randomHex(8)).
const bucketIDLen = 16

// validBucketID reports whether id has the shape of a generated bucket ID.
func validBucketID(id string) bool { return len(id) == bucketIDLen && validDataID(id) }

// validDataID reports whether id is a well-formed data file ID (lowercase hex,
// at least 3 characters).
func validDataID(id string) bool {
	return len(id) >= 3 && len(id) <= 64 && validHex(id)
}

// validHex reports whether s consists of lowercase hex digits only.
func validHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isURLSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// partKey returns the "parts" key of part n of an upload.
func partKey(uploadID string, n int) []byte {
	b := make([]byte, 0, len(uploadID)+4)
	b = append(b, uploadID...)
	return binary.BigEndian.AppendUint32(b, uint32(n))
}

// validKey reports whether the backend can store an object key: non-empty,
// valid UTF-8 and bounded.
func validKey(key string) error {
	if key == "" || len(key) > maxKeyLen || !utf8.ValidString(key) {
		return fmt.Errorf("%w: object key %.64q", storage.ErrInvalidName, key)
	}
	return nil
}

// lookupKeyOK reports whether key could possibly exist (for reads, invalid
// keys are simply "not found").
func lookupKeyOK(key string) bool { return key != "" && len(key) <= maxKeyLen }

// etagMatches compares an If-Match value with a stored (unquoted) ETag. "*"
// matches any current object; surrounding quotes are tolerated.
func etagMatches(cond, etag string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "*" {
		return true
	}
	if len(cond) >= 2 && cond[0] == '"' && cond[len(cond)-1] == '"' {
		cond = cond[1 : len(cond)-1]
	}
	return cond == etag
}

// --- transaction helpers ---

// loadBucket returns the record and per-bucket bbolt bucket of name.
func loadBucket(tx *bbolt.Tx, name string) (*bucketRecord, *bbolt.Bucket, error) {
	if name == "" || len(name) > maxBucketNameLen {
		return nil, nil, fmt.Errorf("%w: %.64q", storage.ErrBucketNotFound, name)
	}
	v := tx.Bucket(topBuckets).Get([]byte(name))
	if v == nil {
		return nil, nil, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	var rec bucketRecord
	if err := decodeRecord(v, &rec); err != nil {
		return nil, nil, fmt.Errorf("bucket %q: %w", name, err)
	}
	bb := tx.Bucket(bucketDataName(rec.ID))
	if bb == nil {
		return nil, nil, fmt.Errorf("%w: bucket %q has no data bucket", errCorrupt, name)
	}
	return &rec, bb, nil
}

// loadBucketID is loadBucket that additionally requires the bucket to still
// have the given ID (it may have been deleted and recreated meanwhile).
func loadBucketID(tx *bbolt.Tx, name, id string) (*bucketRecord, *bbolt.Bucket, error) {
	rec, bb, err := loadBucket(tx, name)
	if err != nil {
		return nil, nil, err
	}
	if rec.ID != id {
		return nil, nil, fmt.Errorf("%w: %q was deleted", storage.ErrBucketNotFound, name)
	}
	return rec, bb, nil
}

func putRecord(b *bbolt.Bucket, key []byte, v any) error {
	data, err := encodeRecord(v)
	if err != nil {
		return err
	}
	return b.Put(key, data)
}

// latestVersion returns the newest version of key (nil if it has none) and
// its "ver" key.
func latestVersion(bb *bbolt.Bucket, key string) (*versionRecord, []byte, error) {
	p := keyVersionsPrefix(key)
	k, v := bb.Bucket(subVersions).Cursor().Seek(p)
	if k == nil || !bytes.HasPrefix(k, p) {
		return nil, nil, nil
	}
	var rec versionRecord
	if err := decodeRecord(v, &rec); err != nil {
		return nil, nil, err
	}
	return &rec, bytes.Clone(k), nil
}

// importedKey returns the "imp" key of the imported version versionID of
// key. Version IDs never contain a 0x00 byte, so the escaped, terminated key
// prefix keeps entries of different keys apart.
func importedKey(key, versionID string) []byte {
	return append(keyVersionsPrefix(key), versionID...)
}

// versionPosition resolves a version ID of key to its "ver" key without
// checking that the version exists. "null" is resolved via the null index,
// imported IDs via the "imp" index, other IDs by the sequence they embed.
func versionPosition(bb *bbolt.Bucket, key, versionID string) ([]byte, bool) {
	if versionID == storage.NullVersionID {
		inv := bb.Bucket(subNull).Get([]byte(key))
		if len(inv) != 8 {
			return nil, false
		}
		return versionKey(key, binary.BigEndian.Uint64(inv)), true
	}
	if imp := bb.Bucket(subImported); imp != nil {
		if inv := imp.Get(importedKey(key, versionID)); len(inv) == 8 {
			return versionKey(key, binary.BigEndian.Uint64(inv)), true
		}
	}
	inv, ok := parseVersionID(versionID)
	if !ok {
		return nil, false
	}
	return versionKey(key, inv), true
}

// findVersion returns the version of key with versionID ("" = latest), its
// "ver" key and whether it is the latest version.
func findVersion(bb *bbolt.Bucket, bucket, key, versionID string) (*versionRecord, []byte, bool, error) {
	if versionID == "" {
		if !lookupKeyOK(key) {
			return nil, nil, false, fmt.Errorf("%w: %s/%.64s", storage.ErrObjectNotFound, bucket, key)
		}
		rec, vk, err := latestVersion(bb, key)
		if err != nil {
			return nil, nil, false, err
		}
		if rec == nil {
			return nil, nil, false, fmt.Errorf("%w: %s/%s", storage.ErrObjectNotFound, bucket, key)
		}
		return rec, vk, true, nil
	}
	notFound := fmt.Errorf("%w: %s/%.64s version %.80q", storage.ErrVersionNotFound, bucket, key, versionID)
	if !lookupKeyOK(key) {
		return nil, nil, false, notFound
	}
	vk, ok := versionPosition(bb, key, versionID)
	if !ok {
		return nil, nil, false, notFound
	}
	v := bb.Bucket(subVersions).Get(vk)
	if v == nil {
		return nil, nil, false, notFound
	}
	var rec versionRecord
	if err := decodeRecord(v, &rec); err != nil {
		return nil, nil, false, err
	}
	if rec.VersionID != versionID {
		return nil, nil, false, notFound
	}
	_, latestKey, err := latestVersion(bb, key)
	if err != nil {
		return nil, nil, false, err
	}
	return &rec, vk, bytes.Equal(latestKey, vk), nil
}

// refreshCurrent recomputes the "cur" entry of key from its newest version.
func refreshCurrent(bb *bbolt.Bucket, key string) error {
	cur := bb.Bucket(subCurrent)
	p := keyVersionsPrefix(key)
	k, v := bb.Bucket(subVersions).Cursor().Seek(p)
	if k == nil || !bytes.HasPrefix(k, p) {
		return cur.Delete([]byte(key))
	}
	var rec versionRecord
	if err := decodeRecord(v, &rec); err != nil {
		return err
	}
	if rec.DeleteMarker {
		return cur.Delete([]byte(key))
	}
	return cur.Put([]byte(key), bytes.Clone(v))
}

// removeVersion deletes the version stored at vk (with record rec) of key,
// maintaining the null, imported-ID and data indexes. It returns the data ID
// to unlink after commit ("" if none). The caller refreshes "cur".
func removeVersion(bb *bbolt.Bucket, key string, vk []byte, rec *versionRecord) (string, error) {
	if err := bb.Bucket(subVersions).Delete(vk); err != nil {
		return "", err
	}
	if rec.VersionID == storage.NullVersionID {
		if err := bb.Bucket(subNull).Delete([]byte(key)); err != nil {
			return "", err
		}
	} else if imp := bb.Bucket(subImported); imp != nil {
		// Only the entry that points at this version (generated IDs have
		// none).
		ik := importedKey(key, rec.VersionID)
		if inv := imp.Get(ik); len(inv) == 8 && bytes.HasSuffix(vk, inv) {
			if err := imp.Delete(ik); err != nil {
				return "", err
			}
		}
	}
	if rec.DataID != "" {
		if err := bb.Bucket(subData).Delete([]byte(rec.DataID)); err != nil {
			return "", err
		}
	}
	return rec.DataID, nil
}

// addVersion stores rec as the newest version of key, assigning its version
// ID per the bucket's versioning status (a null version replaces any
// existing null version or null delete marker). It returns data IDs to
// unlink after commit.
func addVersion(br *bucketRecord, bb *bbolt.Bucket, key string, rec *versionRecord) ([]string, error) {
	seq, err := bb.NextSequence()
	if err != nil {
		return nil, err
	}
	inv := invert(seq)
	var unlink []string
	if br.Versioning == storage.VersioningEnabled {
		rec.VersionID = newVersionID(inv)
	} else {
		rec.VersionID = storage.NullVersionID
		if old := bb.Bucket(subNull).Get([]byte(key)); len(old) == 8 {
			ovk := versionKey(key, binary.BigEndian.Uint64(old))
			if v := bb.Bucket(subVersions).Get(ovk); v != nil {
				var orec versionRecord
				if err := decodeRecord(v, &orec); err != nil {
					return nil, err
				}
				id, err := removeVersion(bb, key, ovk, &orec)
				if err != nil {
					return nil, err
				}
				if id != "" {
					unlink = append(unlink, id)
				}
			}
		}
		if err := bb.Bucket(subNull).Put([]byte(key), invBytes(inv)); err != nil {
			return nil, err
		}
	}
	vk := versionKey(key, inv)
	if err := putRecord(bb.Bucket(subVersions), vk, rec); err != nil {
		return nil, err
	}
	if rec.DataID != "" {
		if err := bb.Bucket(subData).Put([]byte(rec.DataID), vk); err != nil {
			return nil, err
		}
	}
	if err := refreshCurrent(bb, key); err != nil {
		return nil, err
	}
	return unlink, nil
}

// checkPreconditions evaluates conditional-write options against the
// current latest version of key.
func checkPreconditions(bb *bbolt.Bucket, key string, opts storage.CommitOptions) error {
	if !opts.IfNoneMatch && opts.IfMatch == "" {
		return nil
	}
	latest, _, err := latestVersion(bb, key)
	if err != nil {
		return err
	}
	if latest != nil && latest.DeleteMarker {
		latest = nil
	}
	if opts.IfNoneMatch && latest != nil {
		return fmt.Errorf("%w: object exists (If-None-Match)", storage.ErrPreconditionFailed)
	}
	if opts.IfMatch != "" && (latest == nil || !etagMatches(opts.IfMatch, latest.Meta.ETag)) {
		return fmt.Errorf("%w: ETag mismatch (If-Match)", storage.ErrPreconditionFailed)
	}
	return nil
}

// --- clone helpers ---

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

func cloneInfo(oi storage.ObjectInfo) storage.ObjectInfo {
	oi.ObjectMeta = cloneMeta(oi.ObjectMeta)
	return oi
}
