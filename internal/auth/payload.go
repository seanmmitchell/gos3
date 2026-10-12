package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// trailerHolder stores trailing headers once a chunked body has been fully
// decoded. It is safe for concurrent use.
type trailerHolder struct {
	mu sync.Mutex
	h  http.Header
}

func (t *trailerHolder) set(h http.Header) {
	t.mu.Lock()
	t.h = h
	t.mu.Unlock()
}

// get returns a copy of the trailers (never nil).
func (t *trailerHolder) get() http.Header {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.h == nil {
		return http.Header{}
	}
	return t.h.Clone()
}

// emptyTrailer is the Trailer func used for bodies that cannot carry trailers.
func emptyTrailer() http.Header { return http.Header{} }

// mapBodyError converts low-level body read errors into S3 errors where the
// meaning is unambiguous (a truncated body is IncompleteBody); other errors
// (network failures, http.MaxBytesError, ...) are returned unchanged.
func mapBodyError(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return s3err.ErrIncompleteBody
	}
	return err
}

// sha256Reader verifies that the body hashes to the declared hex SHA-256 when
// it reaches EOF. On mismatch the final Read returns
// ErrXAmzContentSHA256Mismatch instead of io.EOF (and no data).
type sha256Reader struct {
	r    io.ReadCloser
	h    hash.Hash
	want string // lowercase hex
	err  error
}

func newSHA256Reader(body io.ReadCloser, wantHex string) *sha256Reader {
	return &sha256Reader{r: body, h: sha256.New(), want: strings.ToLower(wantHex)}
}

func (s *sha256Reader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	if n > 0 {
		s.h.Write(p[:n])
	}
	if err == nil {
		return n, nil
	}
	if err != io.EOF {
		s.err = mapBodyError(err)
		return 0, s.err
	}
	got := hex.EncodeToString(s.h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.want)) != 1 {
		s.err = s3err.ErrXAmzContentSHA256Mismatch.
			With("ClientComputedContentSHA256", s.want).
			With("S3ComputedContentSHA256", got)
		return 0, s.err
	}
	s.err = io.EOF
	return n, io.EOF
}

func (s *sha256Reader) Close() error { return s.r.Close() }

// passthroughBody wraps an unverified body, mapping truncation to
// IncompleteBody.
type passthroughBody struct {
	r io.ReadCloser
}

func (p passthroughBody) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err != nil && err != io.EOF {
		err = mapBodyError(err)
	}
	return n, err
}

func (p passthroughBody) Close() error { return p.r.Close() }

// isHexSHA256 reports whether s is a 64 character hex string.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if _, ok := unhex(s[i]); !ok {
			return false
		}
	}
	return true
}

// isLowerHexSHA256 reports whether s is a 64 character lowercase hex string
// (the format of SigV4 signatures).
func isLowerHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// payloadKind classifies an x-amz-content-sha256 value.
type payloadKind int

const (
	payloadInvalid payloadKind = iota
	payloadHex
	payloadUnsigned
	payloadStreamingSigned
	payloadStreamingSignedTrailer
	payloadStreamingUnsignedTrailer
	payloadSigV4a
)

func classifyPayload(v string) payloadKind {
	switch v {
	case PayloadUnsigned:
		return payloadUnsigned
	case PayloadStreamingSigned:
		return payloadStreamingSigned
	case PayloadStreamingSignedTrailer:
		return payloadStreamingSignedTrailer
	case PayloadStreamingUnsignedTrailer:
		return payloadStreamingUnsignedTrailer
	case "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD", "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD-TRAILER":
		return payloadSigV4a
	}
	if isHexSHA256(v) {
		return payloadHex
	}
	return payloadInvalid
}

// validatePayloadMode returns an S3 error for unusable x-amz-content-sha256
// values.
func validatePayloadMode(v string) (payloadKind, error) {
	k := classifyPayload(v)
	switch k {
	case payloadInvalid:
		return k, s3err.ErrInvalidArgument.
			WithMessage("x-amz-content-sha256 must be UNSIGNED-PAYLOAD, STREAMING-UNSIGNED-PAYLOAD-TRAILER, STREAMING-AWS4-HMAC-SHA256-PAYLOAD, STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER or a valid sha256 value.").
			With("ArgumentName", "x-amz-content-sha256").
			With("ArgumentValue", truncate(v, 128))
	case payloadSigV4a:
		return k, s3err.ErrNotImplemented.WithMessage("Signature Version 4A (AWS4-ECDSA-P256-SHA256) payloads are not supported.")
	}
	return k, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
