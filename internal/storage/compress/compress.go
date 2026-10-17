// Package compress implements the seekable compressed data format that
// storage backends use for objects stored with compression (see
// storage.Compression).
//
// Object data is cut into blocks of BlockSize uncompressed bytes (the last
// block may be shorter), and every block is compressed on its own with raw
// DEFLATE (RFC 1951, compress/flate), so that a reader can start anywhere
// by inflating at most one block: range requests on compressed objects cost
// O(BlockSize), not O(offset). A block that DEFLATE does not shrink by at
// least 1/32 is stored as is, so incompressible data costs a copy and a
// few bytes of index rather than inflation work on every read.
//
// Layout of a stream holding n > 0 blocks of total uncompressed size S:
//
//	block 0 .. block n-1     stored bytes of each block, back to back
//	index                    n entries of 12 bytes, one per block:
//	                           uint64 end offset of the block's stored
//	                                  bytes (bit 63 set: stored raw)
//	                           uint32 CRC-32C of the block's
//	                                  uncompressed bytes
//	footer (24 bytes)        magic "gos3blk1", uint32 BlockSize,
//	                         uint32 n, uint64 S
//
// All integers are big-endian. The index stores cumulative end offsets so
// that the position of any block can be read with one ReadAt, without
// loading the whole index (which is 12 bytes per 256 KiB, about 47 MiB
// for a 1 TiB object). Every block is verified against its CRC-32C when it
// is read. An empty input (S = 0) produces an empty stream: no blocks, no
// index and no footer.
package compress

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
)

// BlockSize is the uncompressed size of every block but the last.
const BlockSize = 256 << 10

// Format constants.
const (
	magic      = "gos3blk1"
	footerSize = 24
	entrySize  = 12
	rawFlag    = uint64(1) << 63
	// maxEnd bounds stored end offsets (bit 63 is the raw flag).
	maxEnd = rawFlag - 1
)

// Level bounds: DEFLATE levels 1 (fastest) to 9 (smallest).
const (
	MinLevel     = flate.BestSpeed
	MaxLevel     = flate.BestCompression
	DefaultLevel = flate.BestSpeed
)

// ErrCorrupt is wrapped by every error caused by malformed or damaged
// compressed data.
var ErrCorrupt = errors.New("compress: corrupt data")

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

// ValidLevel reports whether level is a DEFLATE level Writer accepts.
func ValidLevel(level int) bool { return level >= MinLevel && level <= MaxLevel }

// --- pools ---

// blockPool recycles block buffers of BlockSize bytes (one per open Writer
// or Reader).
var blockPool = sync.Pool{New: func() any {
	b := make([]byte, BlockSize)
	return &b
}}

// flateWriters holds one pool of DEFLATE compressors per level (a
// compressor allocates about 1 MiB of state).
var flateWriters [MaxLevel + 1]sync.Pool

func getFlateWriter(dst io.Writer, level int) *flate.Writer {
	if fw, ok := flateWriters[level].Get().(*flate.Writer); ok {
		fw.Reset(dst)
		return fw
	}
	fw, _ := flate.NewWriter(dst, level) // level is valid
	return fw
}

func putFlateWriter(fw *flate.Writer, level int) {
	fw.Reset(io.Discard)
	flateWriters[level].Put(fw)
}

var flateReaders sync.Pool

func getFlateReader(src io.Reader) io.ReadCloser {
	if fr, ok := flateReaders.Get().(io.ReadCloser); ok {
		if err := fr.(flate.Resetter).Reset(src, nil); err == nil {
			return fr
		}
	}
	return flate.NewReader(src)
}

// --- writer ---

// Writer compresses a stream into the block format. It is not safe for
// concurrent use. Close must be called to write the index and footer
// (and to release pooled buffers); Abandon releases them without writing.
type Writer struct {
	w     io.Writer
	level int

	buf     *[]byte // current block (BlockSize bytes)
	n       int     // bytes buffered in buf
	scratch bytes.Buffer
	index   []uint64 // per block: end offset | rawFlag
	crcs    []uint32
	size    int64 // uncompressed bytes written
	stored  int64 // bytes written to w
	// skip counts blocks to store without trying to compress after
	// incompressible ones (backoff doubles up to maxSkip).
	skip, backoff int
	err           error
	closed        bool
}

// maxSkip bounds the incompressible-data backoff (in blocks).
const maxSkip = 32

// NewWriter returns a Writer writing the compressed form of its input to
// w with the given DEFLATE level (MinLevel..MaxLevel).
func NewWriter(w io.Writer, level int) (*Writer, error) {
	if !ValidLevel(level) {
		return nil, fmt.Errorf("compress: invalid level %d (expected %d-%d)", level, MinLevel, MaxLevel)
	}
	return &Writer{w: w, level: level, buf: blockPool.Get().(*[]byte)}, nil
}

// Write buffers p, compressing and writing every completed block.
func (z *Writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, errors.New("compress: write after close")
	}
	written := 0
	for len(p) > 0 {
		c := copy((*z.buf)[z.n:], p)
		z.n += c
		written += c
		p = p[c:]
		if z.n == BlockSize {
			if err := z.flushBlock(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// flushBlock compresses and writes the buffered block.
func (z *Writer) flushBlock() error {
	if z.n == 0 {
		return nil
	}
	block := (*z.buf)[:z.n]
	out, raw := block, true
	if z.skip > 0 {
		z.skip--
	} else {
		z.scratch.Reset()
		fw := getFlateWriter(&z.scratch, z.level)
		_, err := fw.Write(block)
		if err == nil {
			err = fw.Close()
		}
		putFlateWriter(fw, z.level)
		if err != nil {
			z.err = err
			return err
		}
		if z.scratch.Len() < len(block)-len(block)/32 {
			out, raw = z.scratch.Bytes(), false
			z.backoff = 0
		} else {
			z.backoff = min(max(2*z.backoff, 1), maxSkip)
			z.skip = z.backoff
		}
	}
	if _, err := z.w.Write(out); err != nil {
		z.err = err
		return err
	}
	z.stored += int64(len(out))
	if uint64(z.stored) > maxEnd {
		z.err = errors.New("compress: stream too large")
		return z.err
	}
	end := uint64(z.stored)
	if raw {
		end |= rawFlag
	}
	z.index = append(z.index, end)
	z.crcs = append(z.crcs, crc32.Checksum(block, castagnoli))
	z.size += int64(z.n)
	z.n = 0
	return nil
}

// Close writes the last block, the index and the footer. It does not
// close the underlying writer. An empty input writes nothing.
func (z *Writer) Close() error {
	if z.closed {
		return z.err
	}
	defer z.release()
	if z.err != nil {
		return z.err
	}
	if err := z.flushBlock(); err != nil {
		return err
	}
	z.closed = true
	if len(z.index) == 0 {
		return nil
	}
	if len(z.index) > 1<<32-1 {
		z.err = errors.New("compress: too many blocks")
		return z.err
	}
	// The index and footer are written in chunks to bound the buffer.
	const chunk = 4096
	tail := make([]byte, 0, min(len(z.index), chunk)*entrySize+footerSize)
	for i, end := range z.index {
		tail = binary.BigEndian.AppendUint64(tail, end)
		tail = binary.BigEndian.AppendUint32(tail, z.crcs[i])
		if len(tail) >= chunk*entrySize {
			if err := z.write(tail); err != nil {
				return err
			}
			tail = tail[:0]
		}
	}
	tail = append(tail, magic...)
	tail = binary.BigEndian.AppendUint32(tail, BlockSize)
	tail = binary.BigEndian.AppendUint32(tail, uint32(len(z.index)))
	tail = binary.BigEndian.AppendUint64(tail, uint64(z.size))
	return z.write(tail)
}

func (z *Writer) write(p []byte) error {
	if _, err := z.w.Write(p); err != nil {
		z.err = err
		return err
	}
	z.stored += int64(len(p))
	return nil
}

// Abandon releases the Writer's buffers without writing anything more.
// Use it instead of Close when the output is discarded.
func (z *Writer) Abandon() {
	if !z.closed {
		z.closed = true
		if z.err == nil {
			z.err = errors.New("compress: writer abandoned")
		}
	}
	z.release()
}

func (z *Writer) release() {
	z.closed = true
	if z.buf != nil {
		blockPool.Put(z.buf)
		z.buf = nil
	}
	z.index, z.crcs = nil, nil
}

// Size returns the number of uncompressed bytes written so far (including
// buffered ones).
func (z *Writer) Size() int64 { return z.size + int64(z.n) }

// StoredSize returns the number of bytes written to the underlying writer
// (after Close: the size of the whole stream).
func (z *Writer) StoredSize() int64 { return z.stored }

// --- reader ---

// Reader decompresses a stream written by Writer with random access. It
// implements io.ReadSeeker; it is not safe for concurrent use.
type Reader struct {
	r      io.ReaderAt
	size   int64 // uncompressed size
	blocks int
	idxOff int64 // offset of the index

	pos int64 // read position (uncompressed)

	// cur is the decoded block cur (-1: none) held in buf[:curLen].
	buf    *[]byte
	cur    int
	curLen int

	// ent caches index entries [entFirst, entFirst+len(ent)/entrySize).
	ent      []byte
	entFirst int

	src   []byte // stored bytes of the block being decoded
	err   error  // sticky corruption or I/O error
	freed bool
}

// indexWindow is the number of index entries read at once.
const indexWindow = 1024

// NewReader returns a Reader over the stream r of storedSize bytes. It
// reads and checks the footer; blocks are checked as they are read.
func NewReader(r io.ReaderAt, storedSize int64) (*Reader, error) {
	z := &Reader{r: r, cur: -1}
	if storedSize == 0 {
		return z, nil
	}
	if storedSize < footerSize {
		return nil, corrupt("stream of %d bytes is shorter than its footer", storedSize)
	}
	var f [footerSize]byte
	if _, err := r.ReadAt(f[:], storedSize-footerSize); err != nil {
		return nil, fmt.Errorf("compress: reading footer: %w", noEOF(err))
	}
	if string(f[:8]) != magic {
		return nil, corrupt("bad magic %q", f[:8])
	}
	if bs := binary.BigEndian.Uint32(f[8:]); bs != BlockSize {
		return nil, corrupt("unsupported block size %d", bs)
	}
	n := int64(binary.BigEndian.Uint32(f[12:]))
	size := binary.BigEndian.Uint64(f[16:])
	if size == 0 || size > uint64(n)*BlockSize || size <= uint64(n-1)*BlockSize {
		return nil, corrupt("size %d does not fit %d blocks", size, n)
	}
	idxOff := storedSize - footerSize - n*entrySize
	if idxOff < 0 {
		return nil, corrupt("index of %d blocks does not fit %d bytes", n, storedSize)
	}
	z.size, z.blocks, z.idxOff = int64(size), int(n), idxOff
	// The last entry's end offset must be where the index starts.
	end, _, _, err := z.entry(z.blocks - 1)
	if err != nil {
		return nil, err
	}
	if end != idxOff {
		return nil, corrupt("blocks end at %d, index starts at %d", end, idxOff)
	}
	return z, nil
}

func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Size returns the uncompressed size of the stream.
func (z *Reader) Size() int64 { return z.size }

// entry returns the end offset, raw flag and CRC of block i.
func (z *Reader) entry(i int) (end int64, raw bool, crc uint32, err error) {
	if i < z.entFirst || i >= z.entFirst+len(z.ent)/entrySize {
		first := i - i%indexWindow
		count := min(indexWindow, z.blocks-first)
		if cap(z.ent) < count*entrySize {
			z.ent = make([]byte, count*entrySize)
		}
		z.ent = z.ent[:count*entrySize]
		if _, err := z.r.ReadAt(z.ent, z.idxOff+int64(first)*entrySize); err != nil {
			z.ent = z.ent[:0]
			return 0, false, 0, fmt.Errorf("compress: reading index: %w", noEOF(err))
		}
		z.entFirst = first
	}
	e := z.ent[(i-z.entFirst)*entrySize:]
	v := binary.BigEndian.Uint64(e)
	return int64(v &^ rawFlag), v&rawFlag != 0, binary.BigEndian.Uint32(e[8:]), nil
}

// load decodes block i into buf.
func (z *Reader) load(i int) error {
	if z.cur == i {
		return nil
	}
	if z.freed {
		return errors.New("compress: read after close")
	}
	start := int64(0)
	if i > 0 {
		s, _, _, err := z.entry(i - 1)
		if err != nil {
			return err
		}
		start = s
	}
	end, raw, crc, err := z.entry(i)
	if err != nil {
		return err
	}
	want := BlockSize
	if i == z.blocks-1 {
		want = int(z.size - int64(i)*BlockSize)
	}
	stored := end - start
	if stored <= 0 || end > z.idxOff || (raw && stored != int64(want)) || stored > 2*BlockSize {
		return corrupt("block %d has invalid extent %d-%d", i, start, end)
	}
	if z.buf == nil {
		z.buf = blockPool.Get().(*[]byte)
	}
	z.cur = -1
	dst := (*z.buf)[:want]
	if raw {
		if _, err := z.r.ReadAt(dst, start); err != nil {
			return fmt.Errorf("compress: reading block %d: %w", i, noEOF(err))
		}
	} else {
		if cap(z.src) < int(stored) {
			z.src = make([]byte, stored)
		}
		z.src = z.src[:stored]
		if _, err := z.r.ReadAt(z.src, start); err != nil {
			return fmt.Errorf("compress: reading block %d: %w", i, noEOF(err))
		}
		fr := getFlateReader(bytes.NewReader(z.src))
		n, err := io.ReadFull(fr, dst)
		if err == nil {
			// The block must end exactly here.
			var one [1]byte
			if m, _ := fr.Read(one[:]); m != 0 {
				err = errors.New("block longer than expected")
			}
		}
		_ = fr.Close()
		flateReaders.Put(fr)
		if err != nil {
			return corrupt("block %d: inflated %d of %d bytes: %v", i, n, want, err)
		}
	}
	if crc32.Checksum(dst, castagnoli) != crc {
		return corrupt("block %d: checksum mismatch", i)
	}
	z.cur, z.curLen = i, want
	return nil
}

// Read reads decompressed data from the current position.
func (z *Reader) Read(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.pos >= z.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := 0
	for n < len(p) && z.pos < z.size {
		i := int(z.pos / BlockSize)
		if err := z.load(i); err != nil {
			z.err = err
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		off := int(z.pos - int64(i)*BlockSize)
		c := copy(p[n:], (*z.buf)[off:z.curLen])
		n += c
		z.pos += int64(c)
	}
	return n, nil
}

// Seek sets the position for the next Read (io.Seeker semantics).
// Seeking beyond the end is allowed; Read then reports io.EOF.
func (z *Reader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = z.pos + offset
	case io.SeekEnd:
		abs = z.size + offset
	default:
		return 0, errors.New("compress: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("compress: negative position")
	}
	z.pos = abs
	return abs, nil
}

// Close releases the Reader's buffers. It does not close the underlying
// ReaderAt. Further reads fail.
func (z *Reader) Close() error {
	if !z.freed {
		z.freed = true
		if z.buf != nil {
			blockPool.Put(z.buf)
			z.buf = nil
		}
		z.cur = -1
		z.src, z.ent = nil, nil
	}
	return nil
}
