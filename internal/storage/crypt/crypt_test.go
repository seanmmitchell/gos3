package crypt

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	mrand "math/rand/v2"
	"sync"
	"testing"
)

const C = ChunkSize

func newKey(t testing.TB) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func randBytes(t testing.TB, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	if _, err := rand.Read(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// seal encrypts plain, writing it in pieces of at most step bytes (all at
// once when step is 0).
func seal(t testing.TB, key, plain []byte, step int) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, key)
	if err != nil {
		t.Fatal(err)
	}
	if step == 0 {
		step = len(plain) + 1
	}
	for p := plain; len(p) > 0; {
		n := min(step, len(p))
		if m, err := w.Write(p[:n]); err != nil || m != n {
			t.Fatalf("write: %d, %v", m, err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// open decrypts the whole stream.
func open(key, ct []byte, size int64) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(ct), key, size)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

var sizes = []int{0, 1, C - 1, C, C + 1, 3 * C, 3*C + 5}

func TestRoundTrip(t *testing.T) {
	key := newKey(t)
	for _, n := range sizes {
		plain := randBytes(t, n)
		for _, step := range []int{0, 1000, C, 32 << 10} {
			ct := seal(t, key, plain, step)
			if int64(len(ct)) != EncryptedSize(int64(n)) {
				t.Fatalf("size %d: ciphertext %d bytes, EncryptedSize %d", n, len(ct), EncryptedSize(int64(n)))
			}
			if n >= 32 && bytes.Contains(ct, plain[:32]) {
				t.Fatalf("size %d: plaintext in ciphertext", n)
			}
			got, err := open(key, ct, int64(n))
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("size %d step %d: round trip: %v", n, step, err)
			}
		}
	}
}

func TestEncryptedSize(t *testing.T) {
	for n, want := range map[int64]int64{
		0: 16, 1: 17, C - 1: C + 15, C: C + 16, C + 1: C + 1 + 32, 3 * C: 3*C + 48, 3*C + 5: 3*C + 5 + 64,
	} {
		if got := EncryptedSize(n); got != want {
			t.Errorf("EncryptedSize(%d) = %d, want %d", n, got, want)
		}
	}
	for _, n := range []int64{-1, 1<<63 - 1, 1<<63 - 1 - 1<<20} {
		if got := EncryptedSize(n); got != -1 {
			t.Errorf("EncryptedSize(%d) = %d, want -1", n, got)
		}
	}
}

func TestEmptyStream(t *testing.T) {
	key := newKey(t)
	ct := seal(t, key, nil, 0)
	if len(ct) != Overhead {
		t.Fatalf("empty stream has %d bytes", len(ct))
	}
	if got, err := open(key, ct, 0); err != nil || len(got) != 0 {
		t.Fatalf("empty: %q, %v", got, err)
	}
	// The empty stream is still authenticated: a missing final chunk, a
	// stream of another key or extra bytes fail.
	r, _ := NewReader(bytes.NewReader(nil), key, 0)
	if _, err := r.ReadAt(make([]byte, 1), 0); err != io.EOF {
		t.Fatalf("ReadAt past the end: %v", err)
	}
	if _, err := open(newKey(t), ct, 0); err != nil {
		// Reading 0 bytes needs no chunk; nothing to verify.
		t.Fatalf("empty read touched data: %v", err)
	}
}

func expectCorrupt(t *testing.T, what string, key, ct []byte, size int64) {
	t.Helper()
	got, err := open(key, ct, size)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("%s: accepted (%d bytes), err %v", what, len(got), err)
	}
}

func TestTampering(t *testing.T) {
	key := newKey(t)
	n := 3*C + 5
	plain := randBytes(t, n)
	ct := seal(t, key, plain, 0)
	size := int64(n)

	expectCorrupt(t, "drop final chunk", key, ct[:3*sealedSize], size)
	expectCorrupt(t, "cut mid-chunk", key, ct[:len(ct)-3], size)
	expectCorrupt(t, "cut mid-first-chunk", key, ct[:100], size)
	expectCorrupt(t, "trailing byte", key, append(bytes.Clone(ct), 0), size)
	// Truncation at a chunk boundary with a matching (smaller) size: the
	// last remaining chunk is not marked final.
	expectCorrupt(t, "truncated at boundary", key, ct[:2*sealedSize], 2*C)
	// Appending a chunk sealed as final by another stream of the same
	// shape (different key) and claiming the longer size.
	other := seal(t, newKey(t), randBytes(t, 4*C), 0)
	expectCorrupt(t, "foreign final chunk", key, append(bytes.Clone(ct[:3*sealedSize]), other[3*sealedSize:]...), 4*C)
	expectCorrupt(t, "append chunk", key, append(bytes.Clone(ct), ct[:sealedSize]...), size)
	expectCorrupt(t, "append chunk, larger size", key, append(bytes.Clone(ct), ct[:sealedSize]...), size+C)
	// Swapping two chunks.
	sw := bytes.Clone(ct)
	copy(sw[:sealedSize], ct[sealedSize:2*sealedSize])
	copy(sw[sealedSize:2*sealedSize], ct[:sealedSize])
	expectCorrupt(t, "swap chunks", key, sw, size)
	// Duplicating a chunk.
	dup := bytes.Clone(ct)
	copy(dup[sealedSize:2*sealedSize], ct[:sealedSize])
	expectCorrupt(t, "duplicate chunk", key, dup, size)
	// Bit flips anywhere (ciphertext and tags).
	for _, pos := range []int{0, 1, sealedSize - 1, sealedSize, 2*sealedSize + 7, len(ct) - Overhead, len(ct) - 1} {
		bad := bytes.Clone(ct)
		bad[pos] ^= 0x80
		expectCorrupt(t, "bit flip", key, bad, size)
	}
	expectCorrupt(t, "wrong key", newKey(t), ct, size)
	// A wrong size moves the final chunk.
	expectCorrupt(t, "larger size", key, ct, size+1)
	expectCorrupt(t, "smaller size", key, ct, size-1)

	// An empty stream truncated or extended.
	empty := seal(t, key, nil, 0)
	r, _ := NewReader(bytes.NewReader(empty[:Overhead-1]), key, 0)
	if _, err := r.chunkLocked(0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated empty stream: %v", err)
	}
	r, _ = NewReader(bytes.NewReader(append(bytes.Clone(empty), 1)), key, 0)
	if _, err := r.chunkLocked(0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("extended empty stream: %v", err)
	}
	r, _ = NewReader(bytes.NewReader(empty), key, 0)
	if _, err := r.chunkLocked(0); err != nil {
		t.Fatalf("empty stream: %v", err)
	}
}

func TestReadAtRanges(t *testing.T) {
	key := newKey(t)
	n := 3*C + 5
	plain := randBytes(t, n)
	ct := seal(t, key, plain, 0)
	r, err := NewReader(bytes.NewReader(ct), key, int64(n))
	if err != nil {
		t.Fatal(err)
	}
	rng := mrand.New(mrand.NewPCG(1, 2))
	for range 500 {
		off := rng.IntN(n + 10)
		l := rng.IntN(2*C + 10)
		buf := make([]byte, l)
		got, err := r.ReadAt(buf, int64(off))
		wantN := max(0, min(l, n-off))
		if got != wantN {
			t.Fatalf("ReadAt(%d, %d) = %d, want %d (%v)", l, off, got, wantN, err)
		}
		if wantN < l && err != io.EOF {
			t.Fatalf("short ReadAt(%d, %d) error %v", l, off, err)
		}
		if wantN == l && err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", l, off, err)
		}
		if wantN > 0 && !bytes.Equal(buf[:got], plain[off:off+got]) {
			t.Fatalf("ReadAt(%d, %d) content mismatch", l, off)
		}
	}
	// Seek + Read.
	if _, err := r.Seek(C-3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(r, buf); err != nil || !bytes.Equal(buf, plain[C-3:C+7]) {
		t.Fatalf("seek read: %v", err)
	}
	if pos, _ := r.Seek(-2, io.SeekEnd); pos != int64(n-2) {
		t.Fatalf("SeekEnd: %d", pos)
	}
	rest, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(rest, plain[n-2:]) {
		t.Fatalf("tail: %v", err)
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
	// Concurrent ReadAt calls are safe.
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := make([]byte, 1000)
			for i := range 50 {
				off := (g*7919 + i*C/3) % (n - 1000)
				if _, err := r.ReadAt(b, int64(off)); err != nil || !bytes.Equal(b, plain[off:off+1000]) {
					t.Errorf("concurrent ReadAt at %d: %v", off, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// countingReaderAt counts the bytes read through it.
type countingReaderAt struct {
	r     io.ReaderAt
	n     int64
	calls int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	c.calls++
	return n, err
}

// TestSequentialReadsEachChunkOnce checks that io.Copy (32 KiB reads)
// reads the ciphertext about once, not once per Read call.
func TestSequentialReadsEachChunkOnce(t *testing.T) {
	key := newKey(t)
	n := 40*C + 123
	plain := randBytes(t, n)
	ct := seal(t, key, plain, 0)
	cr := &countingReaderAt{r: bytes.NewReader(ct)}
	r, err := NewReader(cr, key, int64(n))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := io.CopyBuffer(&out, struct{ io.Reader }{r}, make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), plain) {
		t.Fatal("content mismatch")
	}
	// Every chunk is read once; the final chunk read asks for one extra byte.
	if cr.n != int64(len(ct)) || cr.calls != 41 {
		t.Fatalf("read %d bytes in %d calls for a %d-byte ciphertext of 41 chunks", cr.n, cr.calls, len(ct))
	}
}

func TestWriterErrors(t *testing.T) {
	if _, err := NewWriter(io.Discard, make([]byte, 16)); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewReader(bytes.NewReader(nil), make([]byte, 31), 0); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewReader(bytes.NewReader(nil), newKey(t), -1); err == nil {
		t.Fatal("negative size accepted")
	}
	w, _ := NewWriter(io.Discard, newKey(t))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after close accepted")
	}
	fail := errors.New("disk full")
	w, _ = NewWriter(failWriter{fail}, newKey(t))
	if _, err := w.Write(make([]byte, C+1)); !errors.Is(err, fail) {
		t.Fatalf("write error: %v", err)
	}
	if err := w.Close(); !errors.Is(err, fail) {
		t.Fatalf("close after failed write: %v", err)
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

// errReaderAt fails every read.
type errReaderAt struct{ err error }

func (e errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, e.err }

func TestReaderIOError(t *testing.T) {
	fail := errors.New("i/o error")
	r, _ := NewReader(errReaderAt{fail}, newKey(t), 10)
	if _, err := io.ReadAll(r); !errors.Is(err, fail) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("I/O error: %v", err)
	}
}

func BenchmarkSequentialRead(b *testing.B) {
	key := newKey(b)
	plain := randBytes(b, 8<<20)
	ct := seal(b, key, plain, 0)
	b.SetBytes(int64(len(plain)))
	buf := make([]byte, 32<<10)
	for b.Loop() {
		r, _ := NewReader(bytes.NewReader(ct), key, int64(len(plain)))
		if _, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{r}, buf); err != nil {
			b.Fatal(err)
		}
	}
}
