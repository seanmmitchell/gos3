package s3store

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// Snapshot format.
//
// A snapshot P/meta/snapshots/<startLSN>-<epoch> is gzip-compressed NDJSON:
//
//	{"v":1,"startLSN":L,"epoch":E,"backingId":...,"created":...,"extra":...}   header
//	{"scope":"...","lsn":N,"payload":...}                                       chunk, any number
//	{"end":true,"chunks":C,"sha256":"..."}                                      trailer
//
// sha256 covers every line before the trailer (with its newline). Every
// record with an LSN <= startLSN is reflected in every chunk; a chunk
// reflects every record of its scope up to its own lsn (>= startLSN).
// Loading replays the journal records after startLSN; the caller applies a
// record to a scope only when its LSN is above that scope's chunk lsn.
// A snapshot without a valid trailer is ignored (the previous one is used).

// SnapshotHeader is the first line of a snapshot.
type SnapshotHeader struct {
	V         int             `json:"v"`
	StartLSN  uint64          `json:"startLSN"`
	Epoch     uint64          `json:"epoch"`
	BackingID string          `json:"backingId"`
	Created   time.Time       `json:"created"`
	Extra     json.RawMessage `json:"extra,omitempty"`
}

// SnapshotWriter receives a snapshot from EngineOptions.Snapshot.
type SnapshotWriter interface {
	// StartLSN is the LSN the snapshot starts at: every record up to it
	// must be reflected in every chunk.
	StartLSN() uint64
	// SetExtra sets the header's extra value (e.g. a stamp counter). It
	// must be called before the first Chunk.
	SetExtra(extra json.RawMessage) error
	// Chunk adds the state of one scope as of lsn (>= StartLSN; typically
	// Engine.LastLSN read under the lock ordering the scope's appends).
	Chunk(scope string, lsn uint64, payload json.RawMessage) error
}

// snapRef names a snapshot object.
type snapRef struct {
	key      string
	startLSN uint64
	epoch    uint64
}

// errSnapshotRefused discards a snapshot during which an append was
// refused.
var errSnapshotRefused = errors.New("s3store: snapshot discarded: a journal append was refused while it was taken")

// errInvalidSnapshot marks a snapshot that is damaged or incomplete.
var errInvalidSnapshot = errors.New("s3store: invalid snapshot")

// listSnapshots returns the snapshots under the prefix, newest first.
func (e *Engine) listSnapshots(ctx context.Context) ([]snapRef, error) {
	var out []snapRef
	err := e.client().Walk(ctx, e.opts.Bucket, e.keys.snapshotRoot(), "", func(o s3client.ObjectAttrs) error {
		if lsn, ep, ok := e.keys.parseSnapshot(o.Key); ok {
			out = append(out, snapRef{key: o.Key, startLSN: lsn, epoch: ep})
		}
		return nil
	})
	if err != nil {
		return nil, remoteErr("list snapshots", err)
	}
	slices.SortFunc(out, func(a, b snapRef) int { return -compareSnap(a, b) })
	return out, nil
}

func compareSnap(a, b snapRef) int {
	if a.startLSN != b.startLSN {
		if a.startLSN < b.startLSN {
			return -1
		}
		return 1
	}
	if a.epoch != b.epoch {
		if a.epoch < b.epoch {
			return -1
		}
		return 1
	}
	return 0
}

// loadSnapshot delivers the newest valid snapshot (nil if there is none)
// and sets ls.fromEpoch, ls.startLSN and ls.last.
func (e *Engine) loadSnapshot(ctx context.Context, ls *loadState) (*snapRef, error) {
	snaps, err := e.listSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, sn := range snaps {
		sp, err := e.downloadSnapshot(ctx, sn.key)
		if err != nil {
			return nil, err
		}
		// Validate completely before delivering anything.
		_, _, err = e.parseSnapshot(sp, sn, nil)
		if err == nil {
			var hdr SnapshotHeader
			var maxLSN uint64
			hdr, maxLSN, err = e.parseSnapshot(sp, sn, ls.emit)
			_ = sp.Close()
			if err != nil {
				return nil, err
			}
			// Records after StartLSN are all replayed: a chunk captured
			// later than StartLSN filters them per scope (see Chunk), and
			// records of other scopes up to the highest chunk LSN must
			// not be skipped.
			ls.fromEpoch, ls.startLSN = hdr.Epoch, hdr.StartLSN
			ls.last, ls.maxChunk = hdr.StartLSN, maxLSN
			return &sn, nil
		}
		_ = sp.Close()
		if !errors.Is(err, errInvalidSnapshot) {
			return nil, err
		}
		e.logger().Warn("s3store: ignoring damaged snapshot", "key", sn.key, "err", err)
	}
	return nil, nil
}

// downloadSnapshot copies a snapshot into a local spool.
func (e *Engine) downloadSnapshot(ctx context.Context, key string) (*s3client.Spool, error) {
	body, _, err := e.client().GetObject(ctx, e.opts.Bucket, key, s3client.GetOptions{})
	if err != nil {
		return nil, remoteErr("get "+key, err)
	}
	defer body.Close()
	sp := s3client.NewSpool(e.opts.TempDir, 0)
	if _, err := io.Copy(sp, body); err != nil {
		_ = sp.Close()
		var se *s3client.Error
		if errors.As(err, &se) {
			return nil, remoteErr("get "+key, err)
		}
		return nil, remoteErr("get "+key, &s3client.Error{Op: "GetObject", Err: err})
	}
	if err := sp.Finish(); err != nil {
		_ = sp.Close()
		return nil, fmt.Errorf("s3store: spool snapshot: %w", err)
	}
	return sp, nil
}

// snapLine is any line of a snapshot after the header.
type snapLine struct {
	Scope   *string         `json:"scope"`
	LSN     uint64          `json:"lsn"`
	Payload json.RawMessage `json:"payload"`
	End     bool            `json:"end"`
	Chunks  int             `json:"chunks"`
	SHA256  string          `json:"sha256"`
}

// parseSnapshot reads a spooled snapshot, verifying it; with emit set it
// delivers the header and chunks. Format errors wrap errInvalidSnapshot.
func (e *Engine) parseSnapshot(sp *s3client.Spool, ref snapRef, emit func(LoadEvent) error) (SnapshotHeader, uint64, error) {
	var hdr SnapshotHeader
	bad := func(format string, args ...any) (SnapshotHeader, uint64, error) {
		return hdr, 0, fmt.Errorf("%w %s: %s", errInvalidSnapshot, ref.key, fmt.Sprintf(format, args...))
	}
	zr, err := gzip.NewReader(io.NewSectionReader(sp, 0, sp.Size()))
	if err != nil {
		return bad("gzip: %v", err)
	}
	br := bufio.NewReaderSize(zr, 64<<10)
	sum := sha256.New()
	line, err := br.ReadBytes('\n')
	if err != nil {
		return bad("header: %v", err)
	}
	sum.Write(line)
	if err := json.Unmarshal(line, &hdr); err != nil {
		return bad("header: %v", err)
	}
	if hdr.V != snapVersion || hdr.StartLSN != ref.startLSN || hdr.Epoch != ref.epoch {
		return bad("header v%d startLSN %d epoch %d does not match the key", hdr.V, hdr.StartLSN, hdr.Epoch)
	}
	if hdr.BackingID != e.marker.BackingID {
		return bad("belongs to backing %q, not %q", hdr.BackingID, e.marker.BackingID)
	}
	if emit != nil {
		if err := emit(LoadEvent{Kind: LoadSnapshot, Header: hdr}); err != nil {
			return hdr, 0, err
		}
	}
	var chunks int
	var maxLSN uint64
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return bad("no trailer (%v)", err)
		}
		var l snapLine
		if err := json.Unmarshal(line, &l); err != nil {
			return bad("line %d: %v", chunks+2, err)
		}
		if l.End {
			if l.Chunks != chunks || l.SHA256 != hex.EncodeToString(sum.Sum(nil)) {
				return bad("trailer does not match the content")
			}
			if _, err := br.ReadByte(); err != io.EOF {
				return bad("data after the trailer")
			}
			if err := zr.Close(); err != nil {
				return bad("gzip: %v", err)
			}
			return hdr, maxLSN, nil
		}
		if l.Scope == nil {
			return bad("line %d is neither a chunk nor a trailer", chunks+2)
		}
		sum.Write(line)
		chunks++
		maxLSN = max(maxLSN, l.LSN)
		if emit != nil {
			if err := emit(LoadEvent{Kind: LoadChunk, Chunk: Chunk{Scope: *l.Scope, LSN: l.LSN, Payload: l.Payload}}); err != nil {
				return hdr, 0, err
			}
		}
	}
}

// snapWriter implements SnapshotWriter over a gzip stream into a spool.
type snapWriter struct {
	hdr       SnapshotHeader
	zw        *gzip.Writer
	sum       hash.Hash
	started   bool
	chunks    int
	maxLSN    uint64
	err       error
	finishing bool
}

func (w *snapWriter) StartLSN() uint64 { return w.hdr.StartLSN }

func (w *snapWriter) SetExtra(extra json.RawMessage) error {
	if w.started {
		return errors.New("s3store: SetExtra after the first chunk")
	}
	if len(extra) == 0 {
		w.hdr.Extra = nil
		return nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, extra); err != nil {
		return fmt.Errorf("s3store: snapshot extra: %w", err)
	}
	w.hdr.Extra = b.Bytes()
	return nil
}

func (w *snapWriter) writeLine(v any) error {
	if w.err != nil {
		return w.err
	}
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if !w.finishing {
		w.sum.Write(line)
	}
	_, w.err = w.zw.Write(line)
	return w.err
}

func (w *snapWriter) start() error {
	if w.started {
		return w.err
	}
	w.started = true
	return w.writeLine(w.hdr)
}

func (w *snapWriter) Chunk(scope string, lsn uint64, payload json.RawMessage) error {
	if lsn < w.hdr.StartLSN {
		return fmt.Errorf("s3store: snapshot chunk %q has lsn %d below the snapshot's start %d", scope, lsn, w.hdr.StartLSN)
	}
	var b bytes.Buffer
	if err := json.Compact(&b, payload); err != nil {
		return fmt.Errorf("s3store: snapshot chunk %q: %w", scope, err)
	}
	if err := w.start(); err != nil {
		return err
	}
	w.chunks++
	w.maxLSN = max(w.maxLSN, lsn)
	return w.writeLine(struct {
		Scope   string          `json:"scope"`
		LSN     uint64          `json:"lsn"`
		Payload json.RawMessage `json:"payload"`
	}{scope, lsn, b.Bytes()})
}

// finish writes the trailer and closes the gzip stream.
func (w *snapWriter) finish() error {
	if err := w.start(); err != nil {
		return err
	}
	w.finishing = true
	if err := w.writeLine(struct {
		End    bool   `json:"end"`
		Chunks int    `json:"chunks"`
		SHA256 string `json:"sha256"`
	}{true, w.chunks, hex.EncodeToString(w.sum.Sum(nil))}); err != nil {
		return err
	}
	return w.zw.Close()
}

// snapshot writes a snapshot of the caller's state and then runs GC.
func (s *session) snapshot(ctx context.Context) error {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	e := s.e
	e.mu.Lock()
	if s.failed != nil {
		err := s.failed
		e.mu.Unlock()
		return err
	}
	start := s.lastLSN
	var prev uint64
	if n := len(s.journal.snaps); n > 0 {
		prev = s.journal.snaps[n-1].startLSN
	}
	batchesAt, bytesAt := s.journal.batches, s.journal.bytes
	backingID := e.marker.BackingID
	e.mu.Unlock()
	if start == prev {
		return nil // nothing new since the last snapshot
	}

	sp := s3client.NewSpool(e.opts.TempDir, 0)
	defer sp.Close()
	zw, _ := gzip.NewWriterLevel(sp, gzip.DefaultCompression)
	w := &snapWriter{
		hdr: SnapshotHeader{V: snapVersion, StartLSN: start, Epoch: s.epoch, BackingID: backingID, Created: time.Now().UTC()},
		zw:  zw, sum: sha256.New(),
	}
	refusals := e.refusals.Load()
	if err := e.opts.Snapshot(ctx, w); err != nil {
		return fmt.Errorf("s3store: snapshot: %w", err)
	}
	if e.refusals.Load() != refusals {
		// The state written may hold a mutation whose record was refused
		// (e.g. by Close): it must not become durable through the
		// snapshot.
		return errSnapshotRefused
	}
	if err := w.finish(); err != nil {
		return fmt.Errorf("s3store: snapshot: %w", err)
	}
	if err := sp.Finish(); err != nil {
		return fmt.Errorf("s3store: snapshot: %w", err)
	}
	// Only durable state may be published.
	if err := s.waitDurable(ctx, max(start, w.maxLSN)); err != nil {
		return err
	}
	ref := snapRef{key: e.keys.snapshot(start, s.epoch), startLSN: start, epoch: s.epoch}
	if _, _, err := e.client().Upload(ctx, e.opts.Bucket, ref.key, sp, sp.Size(), s3client.UploadOptions{}); err != nil {
		return remoteErr("upload snapshot", err)
	}
	e.mu.Lock()
	s.journal.snaps = append(s.journal.snaps, ref)
	s.journal.batches -= batchesAt
	s.journal.bytes -= bytesAt
	s.journal.since = e.now()
	e.mu.Unlock()
	e.logger().Info("s3store: snapshot written", "key", ref.key, "chunks", w.chunks, "bytes", sp.Size())
	s.gc(ctx)
	return nil
}

// gc keeps the two newest valid snapshots and deletes older snapshots and
// the journal batches the older kept snapshot covers. Seals and the last
// object of every epoch are never deleted.
func (s *session) gc(ctx context.Context) {
	e := s.e
	e.mu.Lock()
	snaps := slices.Clone(s.journal.snaps)
	e.mu.Unlock()
	if len(snaps) == 0 {
		return
	}
	newest := snaps[len(snaps)-1]
	kept := []snapRef{newest}
	if len(snaps) > 1 {
		kept = append(kept, snaps[len(snaps)-2])
	}
	all, err := e.listSnapshots(ctx)
	if err != nil {
		e.logger().Warn("s3store: snapshot GC", "err", err)
		return
	}
	var victims []string
	for _, sn := range all {
		if compareSnap(sn, newest) < 0 && !slices.ContainsFunc(kept, func(k snapRef) bool { return k.key == sn.key }) {
			victims = append(victims, sn.key)
		}
	}
	var batches []batchRef
	if len(kept) == 2 {
		cutoff := kept[1].startLSN
		e.mu.Lock()
		maxSeq := map[uint64]uint64{}
		for _, b := range s.journal.catalog {
			maxSeq[b.epoch] = max(maxSeq[b.epoch], b.seq)
		}
		for _, b := range s.journal.catalog {
			if !b.seal && b.seq != maxSeq[b.epoch] && b.last <= cutoff {
				batches = append(batches, b)
			}
		}
		e.mu.Unlock()
	}
	// Journal batches go oldest first, and GC stops at the first failure,
	// so what remains of the journal stays contiguous (replay would also
	// read past a gap of covered batches).
	slices.SortFunc(batches, func(a, c batchRef) int {
		if a.epoch != c.epoch {
			return cmp.Compare(a.epoch, c.epoch)
		}
		return cmp.Compare(a.seq, c.seq)
	})
	deleted := map[string]bool{}
	if len(victims) > 0 {
		if err := e.client().DeleteObjects(ctx, e.opts.Bucket, victims); err != nil {
			e.logger().Warn("s3store: snapshot GC", "err", err)
		}
	}
	for len(batches) > 0 {
		n := min(len(batches), gcChunk)
		keys := make([]string, n)
		for i, b := range batches[:n] {
			keys[i] = e.keys.batch(b.epoch, b.seq)
		}
		batches = batches[n:]
		err := e.client().DeleteObjects(ctx, e.opts.Bucket, keys)
		if err == nil {
			for _, k := range keys {
				deleted[k] = true
			}
			continue
		}
		e.logger().Warn("s3store: journal GC stopped", "err", err)
		var failed *s3client.DeleteObjectsError
		if errors.As(err, &failed) {
			notDeleted := map[string]bool{}
			for _, f := range failed.Failed {
				notDeleted[f.Key] = true
			}
			for _, k := range keys {
				deleted[k] = !notDeleted[k]
			}
		}
		break
	}
	e.mu.Lock()
	s.journal.catalog = slices.DeleteFunc(s.journal.catalog, func(b batchRef) bool {
		return deleted[e.keys.batch(b.epoch, b.seq)]
	})
	s.journal.snaps = slices.DeleteFunc(s.journal.snaps, func(sn snapRef) bool {
		return !slices.ContainsFunc(kept, func(k snapRef) bool { return k.key == sn.key })
	})
	e.mu.Unlock()
}

// gcChunk is the number of journal batches one GC request deletes.
var gcChunk = 1000

// maybeSnapshot starts a background snapshot when the policy says one is
// due (called by the writer after each batch).
func (s *session) maybeSnapshot() {
	e := s.e
	if e.opts.Snapshot == nil {
		return
	}
	p := e.policy
	now := e.now()
	e.mu.Lock()
	j := &s.journal
	due := (p.MaxJournalBytes >= 0 && j.bytes > p.MaxJournalBytes) ||
		(p.MaxBatches >= 0 && j.batches > p.MaxBatches) ||
		(p.MaxAge >= 0 && j.batches > 0 && now.Sub(j.since) > p.MaxAge)
	if !due || j.snapRunning || now.Before(j.nextTry) || s.closing || s.failed != nil {
		e.mu.Unlock()
		return
	}
	j.snapRunning = true
	s.snapBg.Add(1)
	e.mu.Unlock()
	go func() {
		defer s.snapBg.Done()
		err := s.snapshot(s.ctx)
		e.mu.Lock()
		j.snapRunning = false
		if err != nil {
			j.nextTry = e.now().Add(time.Minute)
		}
		e.mu.Unlock()
		if err != nil && s.ctx.Err() == nil {
			e.logger().Warn("s3store: background snapshot failed", "err", err)
		}
	}()
}
