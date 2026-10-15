package s3store

import (
	"context"
	"fmt"
	"slices"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// loadState tracks a load in progress (Open only).
type loadState struct {
	emit func(LoadEvent) error
	// fromEpoch is the epoch of the loaded snapshot (0 without one): the
	// first epoch replayed and the lowest epoch that may need a seal.
	fromEpoch uint64
	startLSN  uint64 // the snapshot's StartLSN
	maxChunk  uint64 // the highest chunk LSN of the snapshot
	last      uint64 // highest LSN delivered (or covered by the snapshot)
	sealed    map[uint64]bool
	lastSeq   map[uint64]uint64
	catalog   []batchRef
	bytes     int64 // journal after the snapshot
	batches   int
}

// apply delivers the records of a batch that are not covered yet, checking
// that no LSN is missing.
func (ls *loadState) apply(h batchHeader, recs []Record, size int) error {
	ls.catalog = append(ls.catalog, batchRef{epoch: h.Epoch, seq: h.Seq, last: h.LSN[1]})
	ls.lastSeq[h.Epoch] = h.Seq
	if h.N == 0 || h.LSN[1] <= ls.last {
		return nil
	}
	if h.LSN[0] > ls.last+1 {
		return fmt.Errorf("%w: records %d to %d are missing (batch %d/%d starts at %d)",
			ErrCorruptJournal, ls.last+1, h.LSN[0]-1, h.Epoch, h.Seq, h.LSN[0])
	}
	for _, r := range recs {
		if r.LSN <= ls.last {
			continue
		}
		if err := ls.emit(LoadEvent{Kind: LoadRecord, Record: r}); err != nil {
			return err
		}
		ls.last = r.LSN
	}
	ls.bytes += int64(size)
	ls.batches++
	return nil
}

// load delivers the newest valid snapshot and the journal after it to
// opts.Load and prepares s (LSNs, catalog, counters) and s.load for
// sealing.
func (e *Engine) load(ctx context.Context, s *session) error {
	ls := &loadState{emit: e.opts.Load, sealed: map[uint64]bool{}, lastSeq: map[uint64]uint64{}}
	if err := ls.emit(LoadEvent{Kind: LoadBegin}); err != nil {
		return err
	}
	snap, err := e.loadSnapshot(ctx, ls)
	if err != nil {
		return err
	}
	if err := e.replayJournal(ctx, s, ls); err != nil {
		return err
	}
	if err := s.sealPrevious(ctx, ls); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	last := max(ls.last, ls.maxChunk)
	s.lastLSN, s.durable = last, last
	s.journal = journalState{
		catalog: ls.catalog,
		bytes:   ls.bytes,
		batches: ls.batches,
		since:   e.now(),
	}
	if snap != nil {
		s.journal.snaps = []snapRef{*snap}
	}
	return nil
}

// replayJournal lists the journal and replays the epochs from ls.fromEpoch
// on, in (epoch, seq) order, stopping each epoch at its seal or at its
// first missing slot.
func (e *Engine) replayJournal(ctx context.Context, s *session, ls *loadState) error {
	epochs := map[uint64][]uint64{}
	err := e.client().Walk(ctx, e.opts.Bucket, e.keys.journalRoot(), "", func(o s3client.ObjectAttrs) error {
		if ep, seq, ok := e.keys.parseBatch(o.Key); ok {
			epochs[ep] = append(epochs[ep], seq)
		}
		return nil
	})
	if err != nil {
		return remoteErr("list journal", err)
	}
	order := make([]uint64, 0, len(epochs))
	for ep := range epochs {
		order = append(order, ep)
	}
	slices.Sort(order)
	var toRead []string
	for _, ep := range order {
		seqs := epochs[ep]
		slices.Sort(seqs)
		if ep >= s.epoch {
			return fmt.Errorf("%w: the journal already has epoch %d (ours is %d)", ErrInUse, ep, s.epoch)
		}
		if ep < ls.fromEpoch {
			// Covered by the snapshot and sealed (see the invariant in
			// engine.go): the last object of the epoch is its seal, the
			// others hold records up to the snapshot at most.
			for i, seq := range seqs {
				ls.catalog = append(ls.catalog, batchRef{epoch: ep, seq: seq, last: ls.startLSN, seal: i == len(seqs)-1})
			}
			ls.sealed[ep] = true
			ls.lastSeq[ep] = seqs[len(seqs)-1]
			continue
		}
		for i, seq := range seqs {
			// A gap (batches removed by an interrupted GC, or lost) is
			// read past: the LSN continuity check of apply refuses the
			// journal (ErrCorruptJournal) if records are actually
			// missing, and the seal goes after the highest slot, so no
			// acknowledged batch is ever dropped or overwritten.
			if i > 0 && seq != seqs[i-1]+1 {
				e.logger().Warn("s3store: journal epoch has a gap; reading on",
					"epoch", ep, "missing_from", seqs[i-1]+1, "missing_to", seq-1)
			}
			toRead = append(toRead, e.keys.batch(ep, seq))
		}
	}
	r := e.remote()
	return r.fetchOrdered(ctx, toRead, maxBatchObject, 8, func(key string, data []byte) error {
		ep, seq, _ := e.keys.parseBatch(key)
		if ls.sealed[ep] {
			e.logger().Warn("s3store: journal object after a seal ignored", "key", key)
			return nil
		}
		h, recs, err := decodeBatch(data)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if h.Epoch != ep || h.Seq != seq {
			return fmt.Errorf("%w: %s holds batch %d/%d", ErrCorruptJournal, key, h.Epoch, h.Seq)
		}
		if h.Seal {
			ls.sealed[ep] = true
			ls.lastSeq[ep] = seq
			ls.catalog = append(ls.catalog, batchRef{epoch: ep, seq: seq, last: ls.last, seal: true})
			return nil
		}
		return ls.apply(h, recs, len(data))
	})
}

// sealPrevious seals every earlier epoch that is not sealed yet, in
// ascending order. A slot found taken holds a batch of that epoch's owner
// (written after we listed the journal): it is applied and the next slot is
// tried. A seal by a newer epoch than ours means we were taken over.
func (s *session) sealPrevious(ctx context.Context, ls *loadState) error {
	lo := max(ls.fromEpoch, 1)
	for ep := lo; ep < s.epoch; ep++ {
		if ls.sealed[ep] {
			continue
		}
		seq := ls.lastSeq[ep] + 1
		for {
			h := batchHeader{Epoch: ep, Seq: seq, Owner: s.owner, Seal: true, By: s.epoch, LSN: [2]uint64{ls.last + 1, ls.last}}
			taken, err := s.writeSlot(ctx, h, encodeBatch(&h, nil))
			if err != nil {
				return err
			}
			if taken == nil {
				ls.catalog = append(ls.catalog, batchRef{epoch: ep, seq: seq, last: ls.last, seal: true})
				ls.lastSeq[ep] = seq
				break
			}
			got, recs, err := decodeBatch(taken.data)
			if err != nil {
				return fmt.Errorf("%s: %w", taken.key, err)
			}
			if got.Epoch != ep || got.Seq != seq {
				return fmt.Errorf("%w: %s holds batch %d/%d", ErrCorruptJournal, taken.key, got.Epoch, got.Seq)
			}
			if got.Seal {
				if got.By > s.epoch {
					return fmt.Errorf("%w: epoch %d was sealed by the newer epoch %d", ErrInUse, ep, got.By)
				}
				ls.catalog = append(ls.catalog, batchRef{epoch: ep, seq: seq, last: ls.last, seal: true})
				ls.lastSeq[ep] = seq
				break
			}
			// The previous owner wrote this batch after we listed the
			// journal; it was acknowledged, so it is part of the state.
			if err := ls.apply(got, recs, len(taken.data)); err != nil {
				return err
			}
			seq++
		}
		e := s.e
		e.logger().Info("s3store: sealed journal epoch", "epoch", ep, "slot", seq, "by", s.epoch)
	}
	return nil
}

// writeStartBatch writes the empty first batch of the session's epoch.
func (s *session) writeStartBatch(ctx context.Context) error {
	e := s.e
	e.mu.Lock()
	last := s.lastLSN
	e.mu.Unlock()
	h := batchHeader{Epoch: s.epoch, Seq: 1, Owner: s.owner, LSN: [2]uint64{last + 1, last}}
	taken, err := s.writeSlot(ctx, h, encodeBatch(&h, nil))
	if err != nil {
		return err
	}
	if taken != nil {
		return fmt.Errorf("%w: %w", ErrInUse, fencedBy(taken))
	}
	e.mu.Lock()
	s.nextSeq = 2
	s.journal.catalog = append(s.journal.catalog, batchRef{epoch: s.epoch, seq: 1, last: last})
	e.mu.Unlock()
	return nil
}

// fetchOrdered reads the objects keys (at most limit bytes each), up to
// conc of them concurrently, and calls fn for each in order. fn's error
// stops the fetch.
func (r remote) fetchOrdered(ctx context.Context, keys []string, limit int64, conc int, fn func(key string, data []byte) error) error {
	if len(keys) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		data []byte
		err  error
	}
	results := make([]chan result, len(keys))
	for i := range results {
		results[i] = make(chan result, 1)
	}
	sem := make(chan struct{}, conc)
	go func() {
		for i, key := range keys {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				data, _, err := r.getBytes(ctx, key, limit)
				results[i] <- result{data, err}
			}()
		}
	}()
	for i, key := range keys {
		var res result
		select {
		case res = <-results[i]:
		case <-ctx.Done():
			return ctx.Err()
		}
		<-sem
		if res.err != nil {
			if s3client.IsNotFound(res.err) {
				return fmt.Errorf("%w: %s vanished while loading (is another server deleting it?)", ErrCorruptJournal, key)
			}
			return res.err
		}
		if err := fn(key, res.data); err != nil {
			return err
		}
	}
	return nil
}
