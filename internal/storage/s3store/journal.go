package s3store

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// Journal batch format.
//
// A batch object P/meta/journal/<epoch>/<seq> is a header line (JSON,
// batchHeader) followed by the records as NDJSON, one {"lsn":N,"r":<payload>}
// per line, gzip-compressed when the header says so. sha256 is over the
// uncompressed record bytes. A batch holds the consecutive LSNs lsn[0]
// through lsn[1] (n = lsn[1]-lsn[0]+1 records); an empty batch (n = 0, the
// first batch of every epoch) has lsn[0] = lsn[1]+1.
//
// A seal is a header-only batch with seal = true and by = the epoch of the
// owner that wrote it; it ends its epoch: no batch follows it.

// batchHeader is the first line of a journal batch.
type batchHeader struct {
	V      int       `json:"v"`
	Epoch  uint64    `json:"epoch"`
	Seq    uint64    `json:"seq"`
	Owner  string    `json:"owner"`
	LSN    [2]uint64 `json:"lsn"`
	N      int       `json:"n"`
	SHA256 string    `json:"sha256"`
	Gzip   bool      `json:"gzip,omitempty"`
	Seal   bool      `json:"seal,omitempty"`
	By     uint64    `json:"by,omitempty"`
}

// gzipThreshold is the record size above which a batch is compressed.
const gzipThreshold = 8 << 10

// maxBatchObject bounds the size of a batch object read back.
const maxBatchObject = 256 << 20

// encodeBatch fills in the checksum (and compression flag) of h and returns
// the batch object for records.
func encodeBatch(h *batchHeader, records []byte) []byte {
	sum := sha256.Sum256(records)
	h.V = journalVersion
	h.SHA256 = hex.EncodeToString(sum[:])
	payload := records
	if len(records) > gzipThreshold {
		var zb bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&zb, gzip.BestSpeed)
		_, _ = zw.Write(records)
		_ = zw.Close()
		if zb.Len() < len(records) {
			payload, h.Gzip = zb.Bytes(), true
		}
	}
	line, _ := json.Marshal(h)
	out := make([]byte, 0, len(line)+1+len(payload))
	out = append(out, line...)
	out = append(out, '\n')
	return append(out, payload...)
}

// parseBatchHeader decodes the header line of a batch object.
func parseBatchHeader(data []byte) (batchHeader, []byte, error) {
	var h batchHeader
	line, rest, ok := bytes.Cut(data, []byte{'\n'})
	if !ok {
		return h, nil, fmt.Errorf("%w: batch without a header line", ErrCorruptJournal)
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return h, nil, fmt.Errorf("%w: batch header: %v", ErrCorruptJournal, err)
	}
	if h.V != journalVersion {
		return h, nil, fmt.Errorf("%w: unsupported batch version %d", ErrCorruptJournal, h.V)
	}
	return h, rest, nil
}

// decodeBatch decodes and verifies a whole batch object.
func decodeBatch(data []byte) (batchHeader, []Record, error) {
	h, payload, err := parseBatchHeader(data)
	if err != nil {
		return h, nil, err
	}
	bad := func(format string, args ...any) (batchHeader, []Record, error) {
		return h, nil, fmt.Errorf("%w: batch %d/%d: %s", ErrCorruptJournal, h.Epoch, h.Seq, fmt.Sprintf(format, args...))
	}
	if len(data) > maxBatchObject {
		return bad("object exceeds size limit")
	}
	if h.N < 0 || h.N > maxBatchRecords {
		return bad("record count %d exceeds batch limits", h.N)
	}
	if h.Gzip {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return bad("gzip: %v", err)
		}
		defer zr.Close()
		if payload, err = io.ReadAll(io.LimitReader(zr, maxBatchObject+1)); err != nil {
			return bad("gzip: %v", err)
		}
	}
	if len(payload) > maxBatchObject {
		return bad("payload exceeds size limit")
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != h.SHA256 {
		return bad("checksum mismatch")
	}
	if h.Seal {
		if h.N != 0 || len(payload) != 0 {
			return bad("seal with records")
		}
		return h, nil, nil
	}
	if h.LSN[0] == 0 ||
		(h.N == 0 && h.LSN[1] != h.LSN[0]-1) ||
		(h.N > 0 && (h.LSN[1] < h.LSN[0] || h.LSN[1]-h.LSN[0] != uint64(h.N-1))) {
		return bad("inconsistent lsn range %v for %d records", h.LSN, h.N)
	}
	// Count actual record lines before allocating; N is remote input. Even
	// a tiny, checksummed object can otherwise request an enormous slice.
	lines := bytes.Count(payload, []byte{'\n'})
	if len(payload) > 0 && payload[len(payload)-1] != '\n' {
		lines++
	}
	if h.N != lines {
		return bad("%d record lines, header says %d", lines, h.N)
	}
	var recs []Record
	for len(payload) > 0 {
		line, rest, _ := bytes.Cut(payload, []byte{'\n'})
		payload = rest
		var r struct {
			LSN uint64          `json:"lsn"`
			R   json.RawMessage `json:"r"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return bad("record: %v", err)
		}
		if want := h.LSN[0] + uint64(len(recs)); r.LSN != want {
			return bad("record lsn %d, want %d", r.LSN, want)
		}
		recs = append(recs, Record{LSN: r.LSN, Payload: r.R})
	}
	if len(recs) != h.N {
		return bad("%d records, header says %d", len(recs), h.N)
	}
	return h, recs, nil
}

// isOurs reports whether the slot holds exactly the batch h describes.
func (h batchHeader) isOurs(got batchHeader) bool {
	return got.Epoch == h.Epoch && got.Seq == h.Seq && got.Owner == h.Owner &&
		got.SHA256 == h.SHA256 && got.Seal == h.Seal
}

// errJournalStalled reports a batch that could not be written within the
// stall timeout.
var errJournalStalled = errors.New("s3store: journal write stalled")

// slotContent is what another writer stored in a slot we tried to write.
type slotContent struct {
	key  string
	data []byte
}

// writeSlot writes body (described by h) into journal slot (h.Epoch,
// h.Seq) with If-None-Match, resolving ambiguous outcomes by reading the
// slot back:
//
//   - the slot holds our batch: success (nil, nil), also after a 412, a 5xx
//     or a transport error whose write did land;
//   - the slot holds something else: (content, nil);
//   - the slot is empty after a failed PUT: the PUT is retried with backoff;
//   - the slot cannot be read: reading is retried with backoff.
//
// It gives up with errJournalStalled once the stall timeout passed, or
// with ctx's error.
func (s *session) writeSlot(ctx context.Context, h batchHeader, body []byte) (*slotContent, error) {
	e := s.e
	r := e.remote()
	key := e.keys.batch(h.Epoch, h.Seq)
	start := e.now()
	delay := e.retryBase
	needRead := false
	var lastErr error
	for {
		if !needRead {
			_, err := r.put(ctx, key, body, s3client.PutOptions{IfNoneMatch: true, NoRetry: true})
			if err == nil {
				return nil, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr, needRead = err, true
		}
		data, _, err := r.getBytes(ctx, key, maxBatchObject)
		switch {
		case err == nil:
			got, _, perr := parseBatchHeader(data)
			if perr == nil && h.isOurs(got) {
				return nil, nil
			}
			return &slotContent{key: key, data: data}, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case s3client.IsNotFound(err):
			// Our write did not land: send it again.
			needRead = false
		default:
			lastErr = err
		}
		if waited := e.now().Sub(start); waited > e.opts.StallTimeout {
			return nil, fmt.Errorf("%w: slot %s unwritten for %v: %w", errJournalStalled, key, waited.Round(time.Millisecond), lastErr)
		}
		e.logger().Debug("s3store: journal write retry", "key", key, "err", lastErr)
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
		delay = min(2*delay, e.retryMax)
	}
}

// fencedBy describes the content of a slot that fenced us.
func fencedBy(c *slotContent) error {
	h, _, err := parseBatchHeader(c.data)
	switch {
	case err != nil:
		return fmt.Errorf("%w: journal slot %s holds an unreadable object", ErrFenced, c.key)
	case h.Seal:
		return fmt.Errorf("%w: journal epoch %d was sealed by epoch %d", ErrFenced, h.Epoch, h.By)
	}
	return fmt.Errorf("%w: journal slot %s was written by owner %s", ErrFenced, c.key, h.Owner)
}

// journalState is the session's knowledge of the remote journal and the
// snapshot cadence counters (guarded by e.mu).
type journalState struct {
	// catalog lists the journal objects known to exist, for GC.
	catalog []batchRef
	// bytes and batches written since the last snapshot; since is when the
	// last snapshot was taken (or the session loaded).
	bytes   int64
	batches int
	since   time.Time
	// snaps are the known valid snapshots, oldest first.
	snaps       []snapRef
	snapRunning bool
	nextTry     time.Time
}

// batchRef is a known journal object. last is the highest LSN it holds
// (for objects not read, an upper bound).
type batchRef struct {
	epoch, seq, last uint64
	seal             bool
}

// writer is the journal goroutine: it writes everything queued as one
// batch whenever no batch is in flight (group commit, depth 1).
func (s *session) writer() {
	defer close(s.writerEnd)
	e := s.e
	for {
		e.mu.Lock()
		for len(s.queue) == 0 && s.failed == nil && !s.closing {
			e.mu.Unlock()
			select {
			case <-s.wake:
			case <-s.ctx.Done():
			}
			e.mu.Lock()
			if s.ctx.Err() != nil && s.failed == nil && len(s.queue) == 0 {
				break
			}
		}
		if s.failed != nil || len(s.queue) == 0 || s.ctx.Err() != nil {
			e.mu.Unlock()
			return
		}
		n, size := 0, 0
		for n < len(s.queue) && n < maxBatchRecords && (n == 0 || size+len(s.queue[n].line) <= maxBatchBytes) {
			size += len(s.queue[n].line)
			n++
		}
		batch := s.queue[:n:n]
		s.queue = s.queue[n:]
		seq := s.nextSeq
		e.mu.Unlock()

		records := make([]byte, 0, size)
		for _, p := range batch {
			records = append(records, p.line...)
		}
		h := batchHeader{
			Epoch: s.epoch, Seq: seq, Owner: s.owner, N: len(batch),
			LSN: [2]uint64{batch[0].lsn, batch[len(batch)-1].lsn},
		}
		body := encodeBatch(&h, records)
		taken, err := s.writeSlot(s.ctx, h, body)
		if err == nil && taken != nil {
			err = fencedBy(taken)
		}
		if err != nil {
			e.failSession(s, err)
			ferr := s.failure()
			for _, p := range batch {
				p.finish(ferr)
			}
			return
		}

		e.mu.Lock()
		s.nextSeq++
		s.durable = h.LSN[1]
		s.journal.catalog = append(s.journal.catalog, batchRef{epoch: s.epoch, seq: seq, last: h.LSN[1]})
		s.journal.bytes += int64(len(body))
		s.journal.batches++
		if s.failed == nil {
			close(s.durableCh)
			s.durableCh = make(chan struct{})
		}
		failed := s.failed != nil
		e.mu.Unlock()
		// The batch is durable even if the session failed meanwhile (a
		// successor seals after it), so its records are acknowledged.
		for _, p := range batch {
			if p.onDurable != nil {
				p.onDurable()
			}
			p.finish(nil)
		}
		if failed {
			return
		}
		s.maybeSnapshot()
	}
}

// waitDurable waits until lsn is durable in this session.
func (s *session) waitDurable(ctx context.Context, lsn uint64) error {
	e := s.e
	for {
		e.mu.Lock()
		if s.durable >= lsn {
			e.mu.Unlock()
			return nil
		}
		if s.failed != nil {
			err := s.failed
			e.mu.Unlock()
			return err
		}
		ch := s.durableCh
		e.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
