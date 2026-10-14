package s3

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// Checksum algorithm names (as used in storage.ObjectMeta.Checksums and
// the x-amz-checksum-algorithm / x-amz-sdk-checksum-algorithm headers).
const (
	ChecksumCRC32     = "CRC32"
	ChecksumCRC32C    = "CRC32C"
	ChecksumCRC64NVME = "CRC64NVME"
	ChecksumSHA1      = "SHA1"
	ChecksumSHA256    = "SHA256"
)

// Checksum types.
const (
	ChecksumTypeFullObject = "FULL_OBJECT"
	ChecksumTypeComposite  = "COMPOSITE"
)

// checksumAlgorithms lists the supported algorithms in canonical order.
var checksumAlgorithms = []string{ChecksumCRC32, ChecksumCRC32C, ChecksumCRC64NVME, ChecksumSHA1, ChecksumSHA256}

// crc64NVMETable is the CRC-64/NVME table: polynomial 0xad93d23594c93659,
// reflected 0x9a6c9329ac4bc9b5 (check value of "123456789" is
// 0xae8b14860a799888).
var crc64NVMETable = crc64.MakeTable(0x9a6c9329ac4bc9b5)

var crc32CTable = crc32.MakeTable(crc32.Castagnoli)

// normalizeChecksumAlgorithm returns the canonical name of a checksum
// algorithm (case-insensitive) and whether it is supported.
func normalizeChecksumAlgorithm(s string) (string, bool) {
	u := strings.ToUpper(strings.TrimSpace(s))
	for _, a := range checksumAlgorithms {
		if u == a {
			return a, true
		}
	}
	return "", false
}

// checksumHeader returns the x-amz-checksum-<algo> header name.
func checksumHeader(algo string) string { return "x-amz-checksum-" + strings.ToLower(algo) }

// newChecksumHash returns a fresh hash for algo (nil if unsupported).
// CRC hashes produce big-endian sums, as S3 encodes them.
func newChecksumHash(algo string) hash.Hash {
	switch algo {
	case ChecksumCRC32:
		return crc32.NewIEEE()
	case ChecksumCRC32C:
		return crc32.New(crc32CTable)
	case ChecksumCRC64NVME:
		return crc64.New(crc64NVMETable)
	case ChecksumSHA1:
		return sha1.New()
	case ChecksumSHA256:
		return sha256.New()
	}
	return nil
}

// checksumSize returns the raw size in bytes of an algorithm's checksum.
func checksumSize(algo string) int {
	switch algo {
	case ChecksumCRC32, ChecksumCRC32C:
		return 4
	case ChecksumCRC64NVME:
		return 8
	case ChecksumSHA1:
		return sha1.Size
	case ChecksumSHA256:
		return sha256.Size
	}
	return 0
}

// computeChecksum returns the base64 checksum of data.
func computeChecksum(algo string, data []byte) string {
	h := newChecksumHash(algo)
	if h == nil {
		return ""
	}
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// decodeChecksum decodes a base64 checksum value and checks its length.
func decodeChecksum(algo, v string) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) != checksumSize(algo) {
		return nil, false
	}
	return b, true
}

// invalidChecksumValue is returned for malformed checksum header values.
func invalidChecksumValue(header string) error {
	return s3err.ErrInvalidRequest.WithMessage("Value for %s header is invalid.", header)
}

// payloadOptions configures a payloadReader.
type payloadOptions struct {
	// MaxSize bounds the payload (EntityTooLarge beyond). A zero value uses
	// the server's MaxObjectSize unless MaxSizeSet is true.
	MaxSize int64
	// MaxSizeSet allows a valid policy limit of zero bytes.
	MaxSizeSet bool
	// RequireLength makes an unknown payload length an error
	// (MissingContentLength), as for PutObject and UploadPart.
	RequireLength bool
	// Algorithms are computed in addition to those the client requested
	// (e.g. the multipart upload's checksum algorithm, or CRC64NVME to
	// always store a default checksum).
	Algorithms []string
}

// payloadReader streams a request payload while computing its MD5 (always)
// and every requested checksum. It enforces the size limit and declared
// length and, at EOF, validates Content-MD5, x-amz-checksum-* headers and
// trailer checksums. Any failure is sticky and returned from Read, so the
// storage writer must Abort on a read error.
type payloadReader struct {
	r        io.Reader
	closer   io.Closer
	trailer  func() http.Header
	n        int64
	max      int64
	expected int64 // declared decoded length, -1 unknown

	md5        hash.Hash
	hashes     map[string]hash.Hash
	contentMD5 []byte            // decoded Content-MD5 (nil if absent)
	declared   map[string][]byte // algo -> raw checksum from headers
	trailers   []string          // algos whose checksum arrives as a trailer
	primary    string            // the client's checksum algorithm ("" if none)

	sums map[string]string // computed base64 checksums, set at EOF
	err  error
	done bool
}

// newPayloadReader wraps the request body (rc.body) in a payloadReader.
// It validates the checksum-related headers up front:
//
//   - Content-MD5 must be base64 of 16 bytes (InvalidDigest);
//   - at most one x-amz-checksum-<algo> header with a well-formed value
//     (InvalidRequest);
//   - x-amz-sdk-checksum-algorithm / x-amz-checksum-algorithm must name a
//     supported algorithm consistent with the checksum header or trailer
//     (InvalidRequest);
//   - x-amz-trailer may only declare x-amz-checksum-<algo> trailers;
//   - a declared length above the limit fails with EntityTooLarge, an
//     unknown one with MissingContentLength when RequireLength is set.
func (s *Server) newPayloadReader(r *http.Request, rc *reqContext, opts payloadOptions) (*payloadReader, error) {
	max := opts.MaxSize
	if max <= 0 && !opts.MaxSizeSet {
		max = s.maxObjectSize
	}
	p := &payloadReader{
		max:      max,
		expected: rc.contentLength(),
		md5:      md5.New(),
		hashes:   map[string]hash.Hash{},
		declared: map[string][]byte{},
		trailer:  rc.trailer,
	}
	body := rc.body
	if body == nil {
		body = r.Body
	}
	if body == nil {
		body = http.NoBody
	}
	p.r, p.closer = body, body

	if p.expected < 0 && opts.RequireLength {
		return nil, s3err.ErrMissingContentLength
	}
	if p.expected > max {
		return nil, s3err.ErrEntityTooLarge.With("ProposedSize", strconv.FormatInt(p.expected, 10)).
			With("MaxSizeAllowed", strconv.FormatInt(max, 10))
	}

	h := r.Header
	if v := h.Get("Content-MD5"); v != "" {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil || len(b) != md5.Size {
			return nil, s3err.ErrInvalidDigest
		}
		p.contentMD5 = b
	}

	headerAlgo := ""
	for _, algo := range checksumAlgorithms {
		v := h.Get(checksumHeader(algo))
		if v == "" {
			continue
		}
		if headerAlgo != "" {
			return nil, s3err.ErrInvalidRequest.WithMessage("Expecting a single x-amz-checksum- header. Multiple checksum Types are not allowed.")
		}
		raw, ok := decodeChecksum(algo, v)
		if !ok {
			return nil, invalidChecksumValue(checksumHeader(algo))
		}
		headerAlgo = algo
		p.declared[algo] = raw
	}

	trailerAlgo := ""
	if v := h.Get("X-Amz-Trailer"); v != "" {
		for _, name := range strings.Split(v, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			algo, ok := strings.CutPrefix(name, "x-amz-checksum-")
			if ok {
				algo, ok = normalizeChecksumAlgorithm(algo)
			}
			if !ok {
				return nil, s3err.ErrInvalidRequest.WithMessage("The value specified in the x-amz-trailer header is not supported")
			}
			if trailerAlgo != "" || headerAlgo != "" {
				return nil, s3err.ErrInvalidRequest.WithMessage("Expecting a single x-amz-checksum- header. Multiple checksum Types are not allowed.")
			}
			trailerAlgo = algo
			p.trailers = append(p.trailers, algo)
		}
	}

	p.primary = headerAlgo
	if p.primary == "" {
		p.primary = trailerAlgo
	}
	for _, hn := range []string{"X-Amz-Sdk-Checksum-Algorithm", "X-Amz-Checksum-Algorithm"} {
		v := h.Get(hn)
		if v == "" {
			continue
		}
		algo, ok := normalizeChecksumAlgorithm(v)
		if !ok {
			return nil, invalidChecksumValue(strings.ToLower(hn))
		}
		if p.primary != "" && p.primary != algo {
			return nil, s3err.ErrInvalidRequest.WithMessage("Value for %s header is invalid.", strings.ToLower(hn))
		}
		p.primary = algo
	}

	if p.primary != "" {
		p.hashes[p.primary] = newChecksumHash(p.primary)
	}
	for _, a := range opts.Algorithms {
		algo, ok := normalizeChecksumAlgorithm(a)
		if !ok {
			continue
		}
		if _, dup := p.hashes[algo]; !dup {
			p.hashes[algo] = newChecksumHash(algo)
		}
	}
	return p, nil
}

// Read implements io.Reader.
func (p *payloadReader) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	if p.done {
		return 0, io.EOF
	}
	// Read at most one byte beyond the limit. A zero-byte POST policy must
	// not stream and hash an arbitrary body before denial.
	if len(b) > 0 && p.max-p.n < int64(len(b)) {
		b = b[:int(p.max-p.n)+1]
	}
	n, err := p.r.Read(b)
	if n > 0 {
		p.n += int64(n)
		switch {
		case p.n > p.max:
			p.err = s3err.ErrEntityTooLarge.With("MaxSizeAllowed", strconv.FormatInt(p.max, 10))
			return 0, p.err
		case p.expected >= 0 && p.n > p.expected:
			p.err = s3err.ErrIncompleteBody.WithMessage("The request body is longer than the declared content length.")
			return 0, p.err
		}
		p.md5.Write(b[:n])
		for _, h := range p.hashes {
			h.Write(b[:n])
		}
	}
	switch {
	case err == io.EOF:
		if ferr := p.finish(); ferr != nil {
			p.err = ferr
			return n, ferr
		}
		p.done = true
		return n, io.EOF
	case err != nil:
		p.err = mapStorageError(err)
		return n, p.err
	}
	return n, nil
}

// finish validates the payload at EOF and records the computed checksums.
func (p *payloadReader) finish() error {
	if p.expected >= 0 && p.n != p.expected {
		return s3err.ErrIncompleteBody
	}
	if p.contentMD5 != nil && !bytes.Equal(p.contentMD5, p.md5.Sum(nil)) {
		return s3err.ErrBadDigest.WithMessage("The Content-MD5 you specified did not match what we received.")
	}
	sums := make(map[string]string, len(p.hashes))
	raw := make(map[string][]byte, len(p.hashes))
	for algo, h := range p.hashes {
		raw[algo] = h.Sum(nil)
		sums[algo] = base64.StdEncoding.EncodeToString(raw[algo])
	}
	for algo, want := range p.declared {
		if !bytes.Equal(want, raw[algo]) {
			return s3err.ErrBadDigest.WithMessage("The %s you specified did not match the calculated checksum.", strings.ToUpper(checksumHeader(algo)))
		}
	}
	if len(p.trailers) > 0 {
		th := p.trailer()
		for _, algo := range p.trailers {
			name := checksumHeader(algo)
			v := th.Get(name)
			if v == "" {
				return s3err.ErrMalformedTrailer.WithMessage("The %s trailer declared in x-amz-trailer was not received.", name)
			}
			want, ok := decodeChecksum(algo, v)
			if !ok {
				return s3err.ErrInvalidRequest.WithMessage("Value for %s trailing header is invalid.", name)
			}
			if !bytes.Equal(want, raw[algo]) {
				return s3err.ErrBadDigest.WithMessage("The %s you specified did not match the calculated checksum.", strings.ToUpper(name))
			}
		}
	}
	p.sums = sums
	return nil
}

// Close closes the underlying body.
func (p *payloadReader) Close() error { return p.closer.Close() }

// verified reports whether the payload is checked against a digest the
// client sent: Content-MD5, an x-amz-checksum-* header or a checksum
// trailer (the check itself happens at EOF).
func (p *payloadReader) verified() bool {
	return p.contentMD5 != nil || len(p.declared) > 0 || len(p.trailers) > 0
}

// Size returns the number of payload bytes read.
func (p *payloadReader) Size() int64 { return p.n }

// MD5 returns the raw MD5 of the payload (complete after EOF).
func (p *payloadReader) MD5() []byte { return p.md5.Sum(nil) }

// ETag returns the hex MD5 of the payload (complete after EOF).
func (p *payloadReader) ETag() string { return hex.EncodeToString(p.md5.Sum(nil)) }

// Algorithm returns the checksum algorithm the client asked for ("" if
// none).
func (p *payloadReader) Algorithm() string { return p.primary }

// Checksums returns the computed base64 checksums by algorithm (nil before
// a successful EOF or when no algorithm was requested).
func (p *payloadReader) Checksums() map[string]string {
	if len(p.sums) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.sums))
	for k, v := range p.sums {
		out[k] = v
	}
	return out
}

// ChecksumType returns FULL_OBJECT when checksums were computed, else "".
func (p *payloadReader) ChecksumType() string {
	if len(p.sums) == 0 {
		return ""
	}
	return ChecksumTypeFullObject
}

// --- Multipart helpers ---

// multipartETag returns the S3 multipart ETag: hex MD5 of the concatenated
// binary part MD5s, "-" and the part count. Part ETags may be quoted.
func multipartETag(partETags []string) (string, error) {
	h := md5.New()
	for _, e := range partETags {
		b, err := hex.DecodeString(trimETag(e))
		if err != nil || len(b) != md5.Size {
			return "", s3err.ErrInvalidPart.WithMessage("Invalid part ETag %q.", e)
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(partETags)), nil
}

// parseChecksumType validates an x-amz-checksum-type header value ("",
// FULL_OBJECT or COMPOSITE, case-insensitive) and returns its canonical
// form.
func parseChecksumType(v string) (string, error) {
	switch u := strings.ToUpper(strings.TrimSpace(v)); u {
	case "":
		return "", nil
	case ChecksumTypeFullObject, ChecksumTypeComposite:
		return u, nil
	}
	return "", s3err.ErrInvalidRequest.WithMessage("Value for x-amz-checksum-type header is invalid.")
}

// supportsFullObject reports whether algo can produce FULL_OBJECT
// multipart checksums (the CRC family).
func supportsFullObject(algo string) bool {
	switch algo {
	case ChecksumCRC32, ChecksumCRC32C, ChecksumCRC64NVME:
		return true
	}
	return false
}

// defaultChecksumType returns the multipart checksum type S3 uses for algo
// when none is specified: FULL_OBJECT for CRC64NVME, COMPOSITE otherwise.
func defaultChecksumType(algo string) string {
	if algo == ChecksumCRC64NVME {
		return ChecksumTypeFullObject
	}
	return ChecksumTypeComposite
}

// validateChecksumType checks an algorithm / type combination for a
// multipart upload and returns the effective type ("" when algo is "").
func validateChecksumType(algo, typ string) (string, error) {
	if algo == "" {
		if typ != "" {
			return "", s3err.ErrInvalidRequest.WithMessage("The x-amz-checksum-type header can only be used with the x-amz-checksum-algorithm header.")
		}
		return "", nil
	}
	if typ == "" {
		return defaultChecksumType(algo), nil
	}
	if typ == ChecksumTypeFullObject && !supportsFullObject(algo) {
		return "", s3err.ErrInvalidRequest.WithMessage("The FULL_OBJECT checksum type cannot be used with the %s checksum algorithm.", strings.ToLower(algo))
	}
	if typ == ChecksumTypeComposite && algo == ChecksumCRC64NVME {
		return "", s3err.ErrInvalidRequest.WithMessage("The COMPOSITE checksum type cannot be used with the crc64nvme checksum algorithm.")
	}
	return typ, nil
}

// compositeChecksum returns the COMPOSITE multipart checksum: the checksum
// of the concatenated raw part checksums, base64, with "-<parts>" appended.
func compositeChecksum(algo string, partChecksums []string) (string, error) {
	h := newChecksumHash(algo)
	if h == nil {
		return "", s3err.ErrInvalidRequest.WithMessage("Unsupported checksum algorithm %q.", algo)
	}
	for i, c := range partChecksums {
		raw, ok := decodeChecksum(algo, c)
		if !ok {
			return "", s3err.ErrInvalidPart.WithMessage("Part %d has no valid %s checksum.", i+1, algo)
		}
		h.Write(raw)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(partChecksums)), nil
}

// checksumPart is one part's checksum and size for FULL_OBJECT combination.
type checksumPart struct {
	Checksum string // base64
	Size     int64
}

// combineFullObjectChecksum combines per-part CRCs into the CRC of the
// whole object (FULL_OBJECT) without rereading data, using the GF(2)
// "CRC combine" construction (as zlib's crc32_combine).
func combineFullObjectChecksum(algo string, parts []checksumPart) (string, error) {
	c, ok := crcCombiners[algo]
	if !ok {
		return "", s3err.ErrInvalidRequest.WithMessage("The FULL_OBJECT checksum type cannot be used with the %s checksum algorithm.", strings.ToLower(algo))
	}
	var crc uint64
	for i, p := range parts {
		raw, ok := decodeChecksum(algo, p.Checksum)
		if !ok || p.Size < 0 {
			return "", s3err.ErrInvalidPart.WithMessage("Part %d has no valid %s checksum.", i+1, algo)
		}
		var v uint64
		if c.width == 32 {
			v = uint64(binary.BigEndian.Uint32(raw))
		} else {
			v = binary.BigEndian.Uint64(raw)
		}
		if i == 0 {
			crc = v
			continue
		}
		crc = c.combine(crc, v, p.Size)
	}
	if len(parts) == 0 {
		crc = 0 // CRC of the empty string for all supported CRCs
	}
	out := make([]byte, c.width/8)
	if c.width == 32 {
		binary.BigEndian.PutUint32(out, uint32(crc))
	} else {
		binary.BigEndian.PutUint64(out, crc)
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// crcCombiner combines reflected CRCs of a given width and polynomial.
type crcCombiner struct {
	width int
	poly  uint64 // reflected polynomial
	// x2n[k] = x^(2^k) mod poly, in reflected representation.
	x2n [64]uint64
}

var crcCombiners = map[string]*crcCombiner{
	ChecksumCRC32:     newCRCCombiner(32, uint64(crc32.IEEE)),
	ChecksumCRC32C:    newCRCCombiner(32, uint64(crc32.Castagnoli)),
	ChecksumCRC64NVME: newCRCCombiner(64, 0x9a6c9329ac4bc9b5),
}

func newCRCCombiner(width int, poly uint64) *crcCombiner {
	c := &crcCombiner{width: width, poly: poly}
	p := c.one() >> 1 // x^1
	for k := range c.x2n {
		c.x2n[k] = p
		p = c.multmodp(p, p)
	}
	return c
}

// one returns the polynomial 1 (x^0) in reflected representation.
func (c *crcCombiner) one() uint64 { return uint64(1) << (c.width - 1) }

// multmodp returns a*b modulo the polynomial (reflected representation).
func (c *crcCombiner) multmodp(a, b uint64) uint64 {
	m := c.one()
	var p uint64
	for m != 0 {
		if a&m != 0 {
			p ^= b
			if a&(m-1) == 0 {
				break
			}
		}
		m >>= 1
		if b&1 != 0 {
			b = b>>1 ^ c.poly
		} else {
			b >>= 1
		}
	}
	return p
}

// x8nmodp returns x^(8n) mod the polynomial.
func (c *crcCombiner) x8nmodp(n int64) uint64 {
	p := c.one()
	k := 3 // 2^3 = 8 bits per byte
	for n > 0 {
		if n&1 != 0 {
			p = c.multmodp(c.x2n[k&63], p)
		}
		n >>= 1
		k++
	}
	return p
}

// combine returns CRC(A||B) from crc1 = CRC(A), crc2 = CRC(B), len2 = |B|.
func (c *crcCombiner) combine(crc1, crc2 uint64, len2 int64) uint64 {
	return c.multmodp(c.x8nmodp(len2), crc1) ^ crc2
}
