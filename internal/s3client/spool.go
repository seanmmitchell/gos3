package s3client

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// DefaultSpoolMemory is the default in-memory size of a Spool before it
// spills to a temporary file.
const DefaultSpoolMemory = 8 << 20

// Spool buffers a written stream so it can be uploaded (and retried) from
// an io.ReaderAt of known size: the first MemLimit bytes are kept in memory,
// beyond that everything moves to a temporary file in Dir. Write it, call
// Finish, read it with ReadAt, and Close it to release the file.
// A Spool is not safe for concurrent writes; ReadAt after Finish is.
type Spool struct {
	dir   string
	limit int
	mem   []byte
	f     *os.File
	size  int64
	err   error
	done  bool
}

// NewSpool returns a spool spilling to dir ("" = os.TempDir()) beyond
// memLimit bytes (<= 0: DefaultSpoolMemory).
func NewSpool(dir string, memLimit int) *Spool {
	if memLimit <= 0 {
		memLimit = DefaultSpoolMemory
	}
	return &Spool{dir: dir, limit: memLimit}
}

var errSpoolFinished = errors.New("s3client: write to a finished spool")

// Write appends p.
func (s *Spool) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.done {
		return 0, errSpoolFinished
	}
	if s.f == nil && len(s.mem)+len(p) <= s.limit {
		s.mem = append(s.mem, p...)
		s.size += int64(len(p))
		return len(p), nil
	}
	if s.f == nil {
		f, err := os.CreateTemp(s.dir, "gos3-spool-*")
		if err != nil {
			s.err = fmt.Errorf("s3client: create spool file: %w", err)
			return 0, s.err
		}
		s.f = f
		if _, err := f.Write(s.mem); err != nil {
			s.err = fmt.Errorf("s3client: write spool file: %w", err)
			return 0, s.err
		}
		s.mem = nil
	}
	n, err := s.f.Write(p)
	s.size += int64(n)
	if err != nil {
		s.err = fmt.Errorf("s3client: write spool file: %w", err)
	}
	return n, s.err
}

// Finish ends writing; the spool is readable afterwards.
func (s *Spool) Finish() error {
	s.done = true
	return s.err
}

// Size returns the number of bytes written.
func (s *Spool) Size() int64 { return s.size }

// ReadAt implements io.ReaderAt over the written bytes.
func (s *Spool) ReadAt(p []byte, off int64) (int, error) {
	if s.f != nil {
		return s.f.ReadAt(p, off)
	}
	if off >= int64(len(s.mem)) {
		return 0, io.EOF
	}
	n := copy(p, s.mem[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Close releases the memory and removes the temporary file. It is
// idempotent.
func (s *Spool) Close() error {
	s.mem = nil
	s.done = true
	if s.f == nil {
		return nil
	}
	name := s.f.Name()
	err := s.f.Close()
	if rerr := os.Remove(name); rerr != nil && err == nil && !errors.Is(rerr, os.ErrNotExist) {
		err = rerr
	}
	s.f = nil
	if s.err == nil {
		s.err = errSpoolFinished
	}
	return err
}
