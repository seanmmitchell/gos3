package fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// recordFormatVersionV1 is the compact binary encoding of versionRecord, the
// record type that is read in bulk by walks and listings (JSON decoding was
// the dominant cost there).
//
// Layout after the format byte, all fields in this fixed order:
//
//	str   VersionID
//	byte  flags (bit 0: DeleteMarker, bit 1: Meta.LegalHold)
//	varint Size
//	time  LastModified
//	str   DataID
//	str   ETag, ContentType, ContentEncoding, ContentDisposition,
//	      ContentLanguage, CacheControl, Expires
//	map   UserMeta
//	map   Tags
//	str   StorageClass, ACL
//	map   Checksums
//	str   ChecksumType
//	ints  PartSizes
//	maps  PartChecksums
//	str   SSE, SSEKMSKeyID, RetentionMode
//	time  RetainUntil
//	str   WebsiteRedirect
//
// where str = uvarint length + bytes (byte-exact, any content); time =
// uvarint 0 for the zero time, else uvarint 1 + varint Unix seconds +
// uvarint nanoseconds (decoded in UTC); map = uvarint 0 for nil, else
// uvarint count+1 followed by count (str key, str value) pairs in sorted key
// order; ints = uvarint 0 for nil, else count+1 varints; maps = uvarint 0
// for nil, else count+1 maps.
const recordFormatVersionV1 byte = 3

// recordFormatVersionV2 is recordFormatVersionV1 followed by
//
//	str   Compression
//	varint StoredSize
//
// It is used only for records with Compression set, so databases without
// compressed objects stay readable by builds that predate it.
const recordFormatVersionV2 byte = 4

// V3 adds actual encryption, so legacy SSE claims remain distinguishable.
const recordFormatVersionV3 byte = 5

var errShortRecord = errors.New("truncated record")

type encoder struct{ b []byte }

func (e *encoder) uvarint(v uint64) { e.b = binary.AppendUvarint(e.b, v) }
func (e *encoder) varint(v int64)   { e.b = binary.AppendVarint(e.b, v) }

func (e *encoder) str(s string) {
	e.uvarint(uint64(len(s)))
	e.b = append(e.b, s...)
}

func (e *encoder) time(t time.Time) {
	if t.IsZero() {
		e.uvarint(0)
		return
	}
	e.uvarint(1)
	e.varint(t.Unix())
	e.uvarint(uint64(t.Nanosecond()))
}

func (e *encoder) strMap(m map[string]string) {
	if m == nil {
		e.uvarint(0)
		return
	}
	e.uvarint(uint64(len(m)) + 1)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		e.str(k)
		e.str(m[k])
	}
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail(errShortRecord)
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.fail(errShortRecord)
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) byte() byte {
	if d.err != nil {
		return 0
	}
	if len(d.b) == 0 {
		d.fail(errShortRecord)
		return 0
	}
	c := d.b[0]
	d.b = d.b[1:]
	return c
}

// str copies a string out of the (possibly mmap'ed) input.
func (d *decoder) str() string {
	n := d.uvarint()
	if d.err != nil {
		return ""
	}
	if n > uint64(len(d.b)) {
		d.fail(errShortRecord)
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

func (d *decoder) time() time.Time {
	switch d.uvarint() {
	case 0:
		return time.Time{}
	case 1:
		sec := d.varint()
		nsec := d.uvarint()
		if nsec >= 1e9 {
			d.fail(errors.New("invalid nanoseconds"))
			return time.Time{}
		}
		return time.Unix(sec, int64(nsec)).UTC()
	default:
		d.fail(errors.New("invalid time tag"))
		return time.Time{}
	}
}

// count reads a nil-able collection length, rejecting counts that cannot
// fit in the remaining input (each element takes at least minElem bytes) so
// corrupt input cannot trigger huge allocations.
func (d *decoder) count(minElem int) (int, bool) {
	v := d.uvarint()
	if d.err != nil || v == 0 {
		return 0, false
	}
	n := v - 1
	if n > uint64(len(d.b)/minElem) || n > math.MaxInt32 {
		d.fail(errShortRecord)
		return 0, false
	}
	return int(n), true
}

func (d *decoder) strMap() map[string]string {
	n, ok := d.count(2)
	if !ok {
		return nil
	}
	m := make(map[string]string, n)
	for i := 0; i < n && d.err == nil; i++ {
		k := d.str()
		m[k] = d.str()
	}
	return m
}

func encodeVersionRecord(r *versionRecord) []byte {
	m := &r.Meta
	e := encoder{b: make([]byte, 0, 128+len(r.VersionID)+len(m.ETag)+len(m.ContentType))}
	format := recordFormatVersionV1
	if r.Compression != "" {
		format = recordFormatVersionV2
	}
	if m.Encryption != "" {
		format = recordFormatVersionV3
	}
	e.b = append(e.b, format)
	e.str(r.VersionID)
	var flags byte
	if r.DeleteMarker {
		flags |= 1
	}
	if m.LegalHold {
		flags |= 2
	}
	e.b = append(e.b, flags)
	e.varint(r.Size)
	e.time(r.LastModified)
	e.str(r.DataID)
	for _, s := range []string{m.ETag, m.ContentType, m.ContentEncoding, m.ContentDisposition, m.ContentLanguage, m.CacheControl, m.Expires} {
		e.str(s)
	}
	e.strMap(m.UserMeta)
	e.strMap(m.Tags)
	e.str(m.StorageClass)
	e.str(m.ACL)
	e.strMap(m.Checksums)
	e.str(m.ChecksumType)
	if m.PartSizes == nil {
		e.uvarint(0)
	} else {
		e.uvarint(uint64(len(m.PartSizes)) + 1)
		for _, s := range m.PartSizes {
			e.varint(s)
		}
	}
	if m.PartChecksums == nil {
		e.uvarint(0)
	} else {
		e.uvarint(uint64(len(m.PartChecksums)) + 1)
		for _, pc := range m.PartChecksums {
			e.strMap(pc)
		}
	}
	e.str(m.SSE)
	e.str(m.SSEKMSKeyID)
	e.str(m.RetentionMode)
	e.time(m.RetainUntil)
	e.str(m.WebsiteRedirect)
	if format >= recordFormatVersionV2 {
		e.str(r.Compression)
		e.varint(r.StoredSize)
	}
	if format == recordFormatVersionV3 {
		e.str(m.Encryption)
	}
	return e.b
}

// decodeVersionRecord parses the payload (after the format byte) of an
// encodeVersionRecord result; v2 tells whether the format byte was
// recordFormatVersionV2.
func decodeVersionRecord(data []byte, r *versionRecord, v2 bool, encrypted ...bool) error {
	d := decoder{b: data}
	m := &r.Meta
	r.VersionID = d.str()
	flags := d.byte()
	r.DeleteMarker = flags&1 != 0
	m.LegalHold = flags&2 != 0
	r.Size = d.varint()
	r.LastModified = d.time()
	r.DataID = d.str()
	for _, p := range []*string{&m.ETag, &m.ContentType, &m.ContentEncoding, &m.ContentDisposition, &m.ContentLanguage, &m.CacheControl, &m.Expires} {
		*p = d.str()
	}
	m.UserMeta = d.strMap()
	m.Tags = d.strMap()
	m.StorageClass = d.str()
	m.ACL = d.str()
	m.Checksums = d.strMap()
	m.ChecksumType = d.str()
	if n, ok := d.count(1); ok {
		m.PartSizes = make([]int64, n)
		for i := range m.PartSizes {
			m.PartSizes[i] = d.varint()
		}
	} else {
		m.PartSizes = nil
	}
	if n, ok := d.count(1); ok {
		m.PartChecksums = make([]map[string]string, n)
		for i := range m.PartChecksums {
			m.PartChecksums[i] = d.strMap()
		}
	} else {
		m.PartChecksums = nil
	}
	m.SSE = d.str()
	m.SSEKMSKeyID = d.str()
	m.RetentionMode = d.str()
	m.RetainUntil = d.time()
	m.WebsiteRedirect = d.str()
	r.Compression, r.StoredSize = "", 0
	if v2 {
		r.Compression = d.str()
		r.StoredSize = d.varint()
		if d.err == nil && ((r.Compression == "" && len(encrypted) == 0) || r.StoredSize < 0) {
			d.fail(errors.New("invalid compression fields"))
		}
	}
	m.Encryption = ""
	if len(encrypted) > 0 {
		m.Encryption = d.str()
		if m.Encryption != storage.EncryptionAES256 && m.Encryption != storage.EncryptionRestic {
			d.fail(errors.New("invalid encryption provider"))
		}
	}
	if d.err == nil && len(d.b) != 0 {
		d.fail(fmt.Errorf("%d trailing bytes", len(d.b)))
	}
	if d.err != nil {
		*r = versionRecord{}
		return d.err
	}
	if r.Size < 0 {
		return errors.New("negative size")
	}
	return nil
}
