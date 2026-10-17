package compress

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// compressible returns n bytes of repetitive text.
func compressible(n int) []byte {
	const text = "the quick brown fox jumps over the lazy dog; "
	return bytes.Repeat([]byte(text), n/len(text)+1)[:n]
}

// random returns n incompressible bytes.
func random(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func pack(t *testing.T, data []byte, level int, chunk int) []byte {
	t.Helper()
	var out bytes.Buffer
	z, err := NewWriter(&out, level)
	if err != nil {
		t.Fatal(err)
	}
	for p := data; len(p) > 0; {
		c := min(chunk, len(p))
		if n, err := z.Write(p[:c]); err != nil || n != c {
			t.Fatalf("Write = %d, %v", n, err)
		}
		p = p[c:]
	}
	if got := z.Size(); got != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", got, len(data))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if got := z.StoredSize(); got != int64(out.Len()) {
		t.Fatalf("StoredSize = %d, want %d", got, out.Len())
	}
	return out.Bytes()
}

func open(t *testing.T, stream []byte) *Reader {
	t.Helper()
	z, err := NewReader(bytes.NewReader(stream), int64(len(stream)))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func TestRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 100, BlockSize - 1, BlockSize, BlockSize + 1, 3*BlockSize + BlockSize/2}
	for _, size := range sizes {
		for _, kind := range []string{"text", "random", "mixed"} {
			var data []byte
			switch kind {
			case "text":
				data = compressible(size)
			case "random":
				data = random(size, uint64(size))
			default:
				data = append(compressible(size/2), random(size-size/2, 7)...)
			}
			for _, chunk := range []int{1 << 20, 4093} {
				stream := pack(t, data, DefaultLevel, chunk)
				if size == 0 && len(stream) != 0 {
					t.Fatalf("empty input produced %d bytes", len(stream))
				}
				z := open(t, stream)
				if z.Size() != int64(size) {
					t.Fatalf("%s/%d: Size = %d", kind, size, z.Size())
				}
				got, err := io.ReadAll(z)
				if err != nil {
					t.Fatalf("%s/%d: ReadAll: %v", kind, size, err)
				}
				if !bytes.Equal(got, data) {
					t.Fatalf("%s/%d: data differs", kind, size)
				}
				_ = z.Close()
			}
		}
	}
}

func TestCompressesAndStoresIncompressibleRaw(t *testing.T) {
	text := compressible(4 * BlockSize)
	if s := pack(t, text, DefaultLevel, 1<<20); len(s) > len(text)/4 {
		t.Errorf("text compressed to %d of %d bytes", len(s), len(text))
	}
	noise := random(4*BlockSize, 1)
	s := pack(t, noise, MaxLevel, 1<<20)
	if overhead := len(s) - len(noise); overhead != 4*entrySize+footerSize {
		t.Errorf("random data overhead = %d bytes, want index + footer only", overhead)
	}
}

func TestSeek(t *testing.T) {
	data := append(compressible(2*BlockSize+77), random(BlockSize+5, 3)...)
	z := open(t, pack(t, data, 6, 1<<20))
	defer z.Close()
	r := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		off := r.Int64N(int64(len(data)) + 10)
		n := r.IntN(3 * BlockSize)
		if _, err := z.Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, n)
		got, err := io.ReadFull(z, buf)
		want := data[min(off, int64(len(data))):]
		want = want[:min(n, len(want))]
		if got != len(want) || !bytes.Equal(buf[:got], want) {
			t.Fatalf("read %d at %d: got %d bytes (%v), want %d", n, off, got, err, len(want))
		}
	}
	if pos, err := z.Seek(-10, io.SeekEnd); err != nil || pos != int64(len(data))-10 {
		t.Fatalf("SeekEnd = %d, %v", pos, err)
	}
	if pos, err := z.Seek(4, io.SeekCurrent); err != nil || pos != int64(len(data))-6 {
		t.Fatalf("SeekCurrent = %d, %v", pos, err)
	}
	if _, err := z.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek succeeded")
	}
}

func TestCorruption(t *testing.T) {
	data := append(compressible(BlockSize+1000), random(BlockSize, 9)...)
	stream := pack(t, data, DefaultLevel, 1<<20)
	readAll := func(s []byte) error {
		z, err := NewReader(bytes.NewReader(s), int64(len(s)))
		if err != nil {
			return err
		}
		defer z.Close()
		_, err = io.Copy(io.Discard, z)
		return err
	}
	if err := readAll(stream); err != nil {
		t.Fatal(err)
	}
	// A flipped bit anywhere in the stream is detected.
	for _, pos := range []int{0, 10, BlockSize / 8, len(stream) - footerSize - entrySize - 1, len(stream) - footerSize - 3, len(stream) - 1, len(stream) - 9} {
		bad := bytes.Clone(stream)
		bad[pos] ^= 0x20
		if err := readAll(bad); !errors.Is(err, ErrCorrupt) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("flip at %d: err = %v, want corruption", pos, err)
		}
	}
	// Truncation is detected.
	for _, n := range []int{1, footerSize - 1, len(stream) - 1, len(stream) / 2} {
		if err := readAll(stream[:n]); err == nil {
			t.Errorf("truncated to %d bytes: no error", n)
		}
	}
	// The reader reports the error again on later reads.
	bad := bytes.Clone(stream)
	bad[5] ^= 1
	z, err := NewReader(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := z.Read(make([]byte, 10)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("first read: %v", err)
	}
	if _, err := z.Read(make([]byte, 10)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("second read: %v", err)
	}
}

func TestLevels(t *testing.T) {
	for _, l := range []int{0, -1, 10} {
		if _, err := NewWriter(io.Discard, l); err == nil {
			t.Errorf("level %d accepted", l)
		}
	}
	data := compressible(BlockSize * 2)
	for l := MinLevel; l <= MaxLevel; l++ {
		got, err := io.ReadAll(open(t, pack(t, data, l, 1<<20)))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("level %d: %v", l, err)
		}
	}
}

func TestAbandonAndClose(t *testing.T) {
	var out bytes.Buffer
	z, _ := NewWriter(&out, DefaultLevel)
	_, _ = z.Write(compressible(BlockSize + 1))
	z.Abandon()
	if _, err := z.Write([]byte("x")); err == nil {
		t.Fatal("write after Abandon succeeded")
	}
	z.Abandon() // idempotent

	r := open(t, pack(t, []byte("hello"), DefaultLevel, 10))
	_ = r.Close()
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after Close succeeded")
	}
}

func BenchmarkWrite(b *testing.B) {
	data := compressible(8 << 20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		z, _ := NewWriter(io.Discard, DefaultLevel)
		_, _ = z.Write(data)
		_ = z.Close()
	}
}

func BenchmarkRead(b *testing.B) {
	data := compressible(8 << 20)
	var out bytes.Buffer
	z, _ := NewWriter(&out, DefaultLevel)
	_, _ = z.Write(data)
	_ = z.Close()
	stream := out.Bytes()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r, _ := NewReader(bytes.NewReader(stream), int64(len(stream)))
		_, _ = io.Copy(io.Discard, r)
		_ = r.Close()
	}
}
