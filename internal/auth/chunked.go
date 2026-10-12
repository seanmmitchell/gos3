package auth

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// Limits applied while decoding aws-chunked bodies. Data is streamed through
// (never buffered per chunk), so these only bound framing overhead and guard
// against hostile input.
const (
	// MaxChunkSize is the largest accepted signed aws-chunked chunk
	// (64 MiB). Signing clients use 8 KiB to 128 KiB chunks. Unsigned
	// chunks (STREAMING-UNSIGNED-PAYLOAD-TRAILER) are bounded only by
	// maxUnsignedChunkSize and x-amz-decoded-content-length: the AWS SDK for
	// Go v2 sends a body of known length as a single chunk, so a 5 GiB
	// PutObject over HTTPS is one 5 GiB chunk.
	MaxChunkSize = 64 << 20
	// maxUnsignedChunkSize bounds unsigned chunks at the largest S3 object
	// (5 TiB), which also keeps sizes far from int64 overflow.
	maxUnsignedChunkSize = 5 << 40
	// maxChunkHeaderLen bounds one "<hex-size>;chunk-signature=<sig>" line.
	maxChunkHeaderLen = 4096
	// maxTrailerBytes bounds the whole trailer section.
	maxTrailerBytes = 16 << 10
	// maxTrailers bounds the number of trailing headers.
	maxTrailers = 32
	// chunkBufSize is the bufio size; must exceed every line limit.
	chunkBufSize = 16 << 10
)

const (
	chunkSigAlgorithm   = "AWS4-HMAC-SHA256-PAYLOAD"
	trailerSigAlgorithm = "AWS4-HMAC-SHA256-TRAILER"
	trailerSignatureKey = "x-amz-trailer-signature"
)

var errLineTooLong = errors.New("line too long")

// chunkSigner carries what is needed to verify chunk and trailer signatures.
type chunkSigner struct {
	key     []byte
	amzDate string
	scope   string
	prevSig string // seed signature, then each chunk's signature
}

func (s *chunkSigner) sign(algo, hashHex string) string {
	var sts strings.Builder
	sts.Grow(len(algo) + len(s.amzDate) + len(s.scope) + 64*3 + 5)
	sts.WriteString(algo)
	sts.WriteByte('\n')
	sts.WriteString(s.amzDate)
	sts.WriteByte('\n')
	sts.WriteString(s.scope)
	sts.WriteByte('\n')
	sts.WriteString(s.prevSig)
	sts.WriteByte('\n')
	if algo == chunkSigAlgorithm {
		sts.WriteString(PayloadEmptySHA256)
		sts.WriteByte('\n')
	}
	sts.WriteString(hashHex)
	return hex.EncodeToString(hmacSHA256(s.key, sts.String()))
}

// verify checks a chunk (or trailer) signature and advances the chain.
func (s *chunkSigner) verify(algo, hashHex, provided string) error {
	want := s.sign(algo, hashHex)
	if !hmac.Equal([]byte(want), []byte(provided)) {
		return s3err.ErrSignatureDoesNotMatch.WithMessage("The chunk signature we calculated does not match the signature you provided. Check your key and signing method.")
	}
	s.prevSig = want
	return nil
}

// chunkedReader decodes an aws-chunked body:
//
//	<hex-size>[;chunk-signature=<sig>]\r\n<data>\r\n ... 0[;chunk-signature=<sig>]\r\n
//	[<name>:<value>\r\n ...][x-amz-trailer-signature:<sig>\r\n]\r\n
//
// Chunk data is passed through as it arrives; a chunk's signature is checked
// when its last byte has been read, and a failure is returned instead of
// those final bytes. Decoded length, trailers and trailer signature are
// verified before io.EOF is returned.
type chunkedReader struct {
	src    *bufio.Reader
	closer io.Closer

	signer        *chunkSigner // nil for unsigned chunks
	allowTrailers bool         // trailer modes
	declared      int64        // x-amz-decoded-content-length, -1 unknown
	// expected trailer names (lowercase) from x-amz-trailer; nil = any.
	expected map[string]bool

	trailers *trailerHolder

	h         hash.Hash
	remaining int64 // bytes of data left in the current chunk
	chunkSig  string
	decoded   int64
	err       error // sticky; io.EOF after a successful finish
}

func newChunkedReader(body io.ReadCloser, signer *chunkSigner, allowTrailers bool, declared int64, expected map[string]bool, th *trailerHolder) *chunkedReader {
	c := &chunkedReader{
		src:           bufio.NewReaderSize(body, chunkBufSize),
		closer:        body,
		signer:        signer,
		allowTrailers: allowTrailers,
		declared:      declared,
		expected:      expected,
		trailers:      th,
	}
	if signer != nil {
		c.h = sha256.New()
	}
	return c
}

func (c *chunkedReader) Close() error { return c.closer.Close() }

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if c.remaining == 0 {
		if err := c.nextChunk(); err != nil {
			c.err = err
			return 0, err
		}
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.src.Read(p)
	if n > 0 {
		if c.h != nil {
			c.h.Write(p[:n])
		}
		c.remaining -= int64(n)
		c.decoded += int64(n)
	}
	if c.remaining == 0 {
		if ferr := c.finishChunk(); ferr != nil {
			c.err = ferr
			return 0, ferr
		}
		return n, nil
	}
	if err != nil {
		if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
			c.err = s3err.ErrIncompleteBody
		} else {
			c.err = err
		}
		return 0, c.err
	}
	return n, nil
}

// readLine reads one line terminated by "\n" (an optional preceding "\r" is
// stripped) of at most max bytes excluding the terminator.
func (c *chunkedReader) readLine(max int) (string, error) {
	line, err := c.src.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errLineTooLong
		}
		if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
			if len(line) == 0 {
				return "", io.EOF
			}
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	line = line[:len(line)-1]
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if len(line) > max {
		return "", errLineTooLong
	}
	return string(line), nil
}

func malformedChunk(msg string) error {
	return s3err.ErrInvalidRequest.WithMessage("%s", "Malformed aws-chunked body: "+msg+".")
}

// lineError maps readLine failures.
func lineError(err error, what string) error {
	switch {
	case errors.Is(err, errLineTooLong):
		return malformedChunk(what + " is too long")
	case err == io.EOF, errors.Is(err, io.ErrUnexpectedEOF):
		return s3err.ErrIncompleteBody
	}
	return err
}

// nextChunk reads and validates a chunk header. On the final (zero-size)
// chunk it processes the trailer section and returns io.EOF.
func (c *chunkedReader) nextChunk() error {
	line, err := c.readLine(maxChunkHeaderLen)
	if err != nil {
		return lineError(err, "chunk header")
	}
	sizeStr, ext, _ := strings.Cut(line, ";")
	sizeStr = strings.TrimSpace(sizeStr)
	if sizeStr == "" || len(sizeStr) > 16 {
		return malformedChunk("invalid chunk size")
	}
	size, perr := strconv.ParseUint(sizeStr, 16, 64)
	if perr != nil {
		return malformedChunk("invalid chunk size")
	}
	if c.signer != nil && size > MaxChunkSize {
		return malformedChunk("chunk size exceeds the maximum of " + strconv.Itoa(MaxChunkSize) + " bytes")
	}
	if size > maxUnsignedChunkSize {
		return malformedChunk("chunk size exceeds the maximum object size")
	}
	if c.declared >= 0 && c.decoded+int64(size) > c.declared {
		return s3err.ErrIncompleteBody.WithMessage("The decoded body is longer than the x-amz-decoded-content-length header.")
	}
	if c.signer != nil {
		sig, ok := chunkSignature(ext)
		if !ok {
			return s3err.ErrSignatureDoesNotMatch.WithMessage("The chunk is missing a valid chunk-signature.")
		}
		c.chunkSig = sig
		c.h.Reset()
	}
	if size > 0 {
		c.remaining = int64(size)
		return nil
	}
	// Final chunk.
	if c.signer != nil {
		if err := c.signer.verify(chunkSigAlgorithm, PayloadEmptySHA256, c.chunkSig); err != nil {
			return err
		}
	}
	if err := c.readTrailers(); err != nil {
		return err
	}
	if c.declared >= 0 && c.decoded != c.declared {
		return s3err.ErrIncompleteBody.WithMessage("The decoded body length does not match the x-amz-decoded-content-length header.")
	}
	return io.EOF
}

// chunkSignature extracts chunk-signature=<64 lowercase hex> from chunk
// extensions.
func chunkSignature(ext string) (string, bool) {
	for ext != "" {
		var kv string
		kv, ext, _ = strings.Cut(ext, ";")
		k, v, _ := strings.Cut(kv, "=")
		if strings.TrimSpace(k) == "chunk-signature" {
			v = strings.TrimSpace(v)
			return v, isLowerHexSHA256(v)
		}
	}
	return "", false
}

// finishChunk consumes the CRLF after chunk data and verifies the signature.
func (c *chunkedReader) finishChunk() error {
	var crlf [2]byte
	if _, err := io.ReadFull(c.src, crlf[:]); err != nil {
		if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
			return s3err.ErrIncompleteBody
		}
		return err
	}
	if crlf != [2]byte{'\r', '\n'} {
		return malformedChunk("chunk data is not followed by CRLF")
	}
	if c.signer != nil {
		return c.signer.verify(chunkSigAlgorithm, hex.EncodeToString(c.h.Sum(nil)), c.chunkSig)
	}
	return nil
}

func malformedTrailer(msg string) error {
	return s3err.ErrMalformedTrailer.WithMessage("%s", "The request contained trailing data that was not well-formed: "+msg+".")
}

// readTrailers reads the trailer section after the final chunk, up to and
// including the terminating empty line, and verifies the trailer signature
// in signed-trailer mode.
//
// Accepted layouts (AWS SDKs and minio-go):
//
//	name:value\r\n ... [x-amz-trailer-signature:sig\r\n] \r\n
//	name:value\n \r\n [x-amz-trailer-signature:sig\r\n] \r\n   (minio-go)
func (c *chunkedReader) readTrailers() error {
	signedTrailer := c.signer != nil && c.allowTrailers
	hdr := http.Header{}
	var canon bytes.Buffer
	var trailerSig string
	total, count := 0, 0
	sawBlank := false
	for {
		line, err := c.readLine(maxTrailerBytes)
		if err != nil {
			if err == io.EOF && ((count == 0 && trailerSig == "") || sawBlank) {
				// Some clients end the body right after the final chunk
				// header; after a blank line, a missing trailer signature is
				// reported below.
				break
			}
			if errors.Is(err, errLineTooLong) {
				return malformedTrailer("trailer section is too large")
			}
			return lineError(err, "trailer")
		}
		total += len(line) + 2
		if total > maxTrailerBytes {
			return malformedTrailer("trailer section is too large")
		}
		if line == "" {
			// In signed-trailer mode, an empty line between the trailers and
			// the trailer signature is tolerated (minio-go layout).
			if signedTrailer && count > 0 && trailerSig == "" && !sawBlank {
				sawBlank = true
				continue
			}
			break
		}
		if trailerSig != "" {
			return malformedTrailer("data after x-amz-trailer-signature")
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return malformedTrailer("trailer line without ':'")
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if !validHeaderName(name) {
			return malformedTrailer("invalid trailer name")
		}
		if name == trailerSignatureKey {
			if signedTrailer {
				if !isLowerHexSHA256(value) {
					return s3err.ErrSignatureDoesNotMatch.WithMessage("The trailer signature is malformed.")
				}
				trailerSig = value
			}
			// Unsigned modes: a trailer signature carries no meaning; ignore.
			continue
		}
		if !c.allowTrailers {
			return malformedTrailer("trailers are not allowed with " + PayloadStreamingSigned)
		}
		if c.expected != nil && !c.expected[name] {
			return malformedTrailer("trailer " + strconv.Quote(name) + " was not declared in x-amz-trailer")
		}
		key := http.CanonicalHeaderKey(name)
		if _, dup := hdr[key]; dup {
			return malformedTrailer("duplicate trailer " + strconv.Quote(name))
		}
		count++
		if count > maxTrailers {
			return malformedTrailer("too many trailers")
		}
		hdr[key] = []string{value}
		canon.WriteString(name)
		canon.WriteByte(':')
		canon.WriteString(value)
		canon.WriteByte('\n')
	}
	for name := range c.expected {
		if _, ok := hdr[http.CanonicalHeaderKey(name)]; !ok {
			return malformedTrailer("declared trailer " + strconv.Quote(name) + " is missing")
		}
	}
	if signedTrailer && (count > 0 || trailerSig != "") {
		if trailerSig == "" {
			return s3err.ErrSignatureDoesNotMatch.WithMessage("The trailer signature is missing.")
		}
		sum := sha256.Sum256(canon.Bytes())
		if err := c.signer.verify(trailerSigAlgorithm, hex.EncodeToString(sum[:]), trailerSig); err != nil {
			return s3err.ErrSignatureDoesNotMatch.WithMessage("The trailer signature we calculated does not match the signature you provided.")
		}
	}
	c.trailers.set(hdr)
	return nil
}

// validHeaderName reports whether s is a non-empty RFC 7230 token.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// parseTrailerDeclaration parses the x-amz-trailer header (comma separated
// names) into a lowercase set; nil when absent.
func parseTrailerDeclaration(h http.Header) (map[string]bool, error) {
	vals := h.Values("X-Amz-Trailer")
	if len(vals) == 0 {
		return nil, nil
	}
	set := map[string]bool{}
	for _, v := range vals {
		for _, name := range strings.Split(v, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if !validHeaderName(name) || len(set) >= maxTrailers {
				return nil, s3err.ErrInvalidArgument.WithMessage("The x-amz-trailer header is invalid.").
					With("ArgumentName", "x-amz-trailer")
			}
			set[name] = true
		}
	}
	if len(set) == 0 {
		return nil, nil
	}
	return set, nil
}

// parseDecodedContentLength parses x-amz-decoded-content-length; -1 when
// absent.
func parseDecodedContentLength(h http.Header) (int64, error) {
	vals := h.Values("X-Amz-Decoded-Content-Length")
	if len(vals) == 0 {
		return -1, nil
	}
	v := strings.TrimSpace(vals[0])
	n, err := strconv.ParseInt(v, 10, 64)
	if len(vals) > 1 || err != nil || n < 0 || v == "" || v[0] == '+' {
		return 0, s3err.ErrInvalidArgument.WithMessage("The x-amz-decoded-content-length header is invalid.").
			With("ArgumentName", "x-amz-decoded-content-length").
			With("ArgumentValue", truncate(vals[0], 64))
	}
	return n, nil
}
