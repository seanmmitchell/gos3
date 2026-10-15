// Package crypt implements the authenticated encryption stream format that
// storage backends use for object data stored with SSE-S3 (see
// storage.EncryptionAES256), for encrypted upload staging and for encrypted
// temporary caches.
//
// A stream encrypts n bytes of plaintext with AES-256-GCM under one 256-bit
// key. The plaintext is cut into chunks of ChunkSize bytes (the last chunk
// may be shorter); chunk i (counting from 0) is sealed on its own with
//
//	nonce   3 zero bytes || uint64 big-endian i || final flag
//	        (1 byte: 1 for the last chunk of the stream, 0 otherwise)
//	AAD     "gos3-stream-v1"
//
// and stored as its ciphertext followed by the 16-byte GCM tag. There is no
// header: sealed chunks follow each other back to back, so chunk i starts at
// i*(ChunkSize+Overhead). Every stream ends with exactly one final chunk;
// the empty stream is a single sealed empty final chunk (Overhead bytes).
// A stream of n plaintext bytes therefore takes
//
//	EncryptedSize(n) = n + Overhead*max(1, ceil(n/ChunkSize))
//
// bytes. Readers are given n (stored with the object's metadata), so they
// know where every chunk and the final chunk must be. The chunk index in
// the nonce rejects reordered, dropped or duplicated chunks; the final flag
// rejects a stream truncated at a chunk boundary; and the final chunk must
// end the input, so appended data is rejected too. Any such damage, a wrong
// key or a flipped bit makes reads fail with ErrCorrupt instead of returning
// unauthenticated plaintext.
//
// Nonces depend on chunk positions only, so a key MUST encrypt exactly one
// stream: sealing two streams (or one stream twice) under the same key
// reuses nonces and breaks confidentiality and authenticity of both. Callers
// derive a fresh key for every stream (the fs backend derives a data file's
// key with HKDF from the bucket master key and the file's random data ID,
// see ObjectDataKey, and uses random keys for temporary files).
//
// The package also wraps keys for storage (Wrap, Unwrap, KeyCheck): the s3
// backing keeps bucket master keys in the remote, sealed under a
// key-encryption key.
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

// Format constants.
const (
	// ChunkSize is the plaintext size of every chunk but the last.
	ChunkSize = 64 << 10
	// Overhead is the number of bytes sealing adds to each chunk (the GCM
	// tag).
	Overhead = 16
	// KeySize is the size of a stream key (AES-256).
	KeySize = 32

	sealedSize = ChunkSize + Overhead
	nonceSize  = 12
)

// aad is the additional authenticated data of every chunk: it ties chunks
// to this format and version.
var aad = []byte("gos3-stream-v1")

// ErrCorrupt is wrapped by every error caused by damaged, truncated,
// extended or tampered ciphertext, or by a wrong key.
var ErrCorrupt = errors.New("crypt: corrupt or tampered data")

// errWriterClosed is returned by Write after Close.
var errWriterClosed = errors.New("crypt: write after close")

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

// EncryptedSize returns the size of the stream encrypting n plaintext bytes,
// or -1 when n is negative or the result would overflow an int64.
func EncryptedSize(n int64) int64 {
	if n < 0 {
		return -1
	}
	chunks := n / ChunkSize
	if n%ChunkSize != 0 || n == 0 {
		chunks++
	}
	overhead := chunks * Overhead
	if n > math.MaxInt64-overhead {
		return -1
	}
	return n + overhead
}

// newAEAD returns the AES-256-GCM AEAD for key.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypt: key has %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypt: %w", err)
	}
	return cipher.NewGCM(block)
}

// chunkNonce returns the nonce of chunk i (see the package documentation).
func chunkNonce(i uint64, final bool) [nonceSize]byte {
	var n [nonceSize]byte
	binary.BigEndian.PutUint64(n[3:11], i)
	if final {
		n[11] = 1
	}
	return n
}

// Writer encrypts a stream into an underlying io.Writer. It buffers up to
// one chunk of plaintext; Close seals the final chunk and must be called
// for the stream to be complete (and readable). A Writer is not safe for
// concurrent use.
type Writer struct {
	w     io.Writer
	aead  cipher.AEAD
	buf   []byte // plaintext of the current chunk; capacity sealedSize
	chunk uint64 // index of the current chunk
	err   error  // sticky; errWriterClosed after a successful Close
}

// NewWriter returns a Writer that encrypts into w with key, which must be
// KeySize bytes and must not encrypt any other stream (see the package
// documentation).
func NewWriter(w io.Writer, key []byte) (*Writer, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Writer{w: w, aead: aead, buf: make([]byte, 0, sealedSize)}, nil
}

// Write buffers p, sealing and writing every chunk that is full when more
// plaintext follows. The last chunk is only sealed by Close.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		if len(w.buf) == ChunkSize {
			// More data follows, so the buffered chunk is not the last.
			if err := w.seal(false); err != nil {
				return written, err
			}
		}
		c := copy(w.buf[len(w.buf):ChunkSize], p)
		w.buf = w.buf[:len(w.buf)+c]
		written += c
		p = p[c:]
	}
	return written, nil
}

// seal encrypts the buffered chunk in place and writes it.
func (w *Writer) seal(final bool) error {
	nonce := chunkNonce(w.chunk, final)
	sealed := w.aead.Seal(w.buf[:0], nonce[:], w.buf, aad)
	if _, err := w.w.Write(sealed); err != nil {
		w.err = err
		return err
	}
	w.chunk++
	w.buf = w.buf[:0]
	return nil
}

// Close seals and writes the final chunk (an empty one for an empty
// stream). It does not close the underlying writer. Calling Close again
// returns nil once the stream was completed, or the error that prevented it.
func (w *Writer) Close() error {
	if w.err == errWriterClosed {
		return nil
	}
	if w.err != nil {
		return w.err
	}
	if err := w.seal(true); err != nil {
		return err
	}
	w.err = errWriterClosed
	return nil
}

// Reader decrypts a stream from an io.ReaderAt. It implements io.Reader,
// io.Seeker and io.ReaderAt over the plaintext; each chunk is authenticated
// before any of its bytes are returned. The most recently decrypted chunk
// is cached, so sequential reads in pieces smaller than ChunkSize read and
// decrypt every chunk once.
//
// ReadAt is safe for concurrent use (concurrent calls are serialized);
// Read and Seek share an offset and are not.
type Reader struct {
	r    io.ReaderAt
	aead cipher.AEAD
	size int64 // plaintext size
	last int64 // index of the final chunk
	off  int64 // Read/Seek position

	mu     sync.Mutex // guards the fields below
	buf    []byte     // read buffer; decrypted in place
	plain  []byte     // plaintext of chunk cached (aliases buf)
	cached int64      // index of the chunk in plain, -1 if none
}

// NewReader returns a Reader over the stream in r that encrypts size
// plaintext bytes with key. Nothing is read until the first Read or ReadAt.
// A ciphertext of any other length than EncryptedSize(size) fails when the
// affected chunk is read (reading the final chunk detects trailing bytes);
// callers that can should also compare the length up front.
func NewReader(r io.ReaderAt, key []byte, size int64) (*Reader, error) {
	if EncryptedSize(size) < 0 {
		return nil, fmt.Errorf("crypt: invalid plaintext size %d", size)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	last := int64(0)
	if size > 0 {
		last = (size - 1) / ChunkSize
	}
	return &Reader{r: r, aead: aead, size: size, last: last, cached: -1}, nil
}

// Size returns the plaintext size of the stream.
func (r *Reader) Size() int64 { return r.size }

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := r.ReadAt(p, r.off)
	r.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// Seek implements io.Seeker. Seeking past the end is allowed; reads there
// return io.EOF.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, errors.New("crypt: invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("crypt: negative position")
	}
	r.off = offset
	return offset, nil
}

// ReadAt implements io.ReaderAt.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("crypt: negative offset")
	}
	if off >= r.size {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) && off < r.size {
		i := off / ChunkSize
		plain, err := r.chunkLocked(i)
		if err != nil {
			return n, err
		}
		c := copy(p[n:], plain[off-i*ChunkSize:])
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// chunkLocked returns the plaintext of chunk i, reading and decrypting it
// unless it is cached. r.mu must be held; the result is valid until the
// next call.
func (r *Reader) chunkLocked(i int64) ([]byte, error) {
	if r.cached == i {
		return r.plain, nil
	}
	r.cached = -1 // buf is about to be overwritten
	if r.buf == nil {
		r.buf = make([]byte, sealedSize+1)
	}
	final := i == r.last
	want := int64(sealedSize)
	read := r.buf[:sealedSize]
	if final {
		want = r.size - i*ChunkSize + Overhead
		// One byte more than the final chunk detects trailing data.
		read = r.buf[:want+1]
	}
	got, err := r.r.ReadAt(read, i*sealedSize)
	switch {
	case int64(got) > want:
		return nil, corrupt("data after the final chunk")
	case int64(got) < want:
		if err != nil && err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("crypt: read chunk %d: %w", i, err)
		}
		return nil, corrupt("chunk %d is truncated", i)
	}
	nonce := chunkNonce(uint64(i), final)
	plain, err := r.aead.Open(r.buf[:0], nonce[:], r.buf[:want], aad)
	if err != nil {
		return nil, corrupt("chunk %d failed authentication", i)
	}
	r.plain, r.cached = plain, i
	return plain, nil
}
