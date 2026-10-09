package fs

import (
	"errors"
	"fmt"
	"sync"

	"go.etcd.io/bbolt"
)

// maxCommitBatch bounds the number of calls coalesced into one transaction.
const maxCommitBatch = 512

// errTrySolo tells a caller whose function failed inside a shared
// transaction to re-run it in a transaction of its own.
var errTrySolo = errors.New("fs: retry transaction alone")

// committer implements group commit for bbolt write transactions. Every
// bbolt commit costs an fsync, so committing one object per transaction
// caps throughput at one object per disk flush. Calls are queued; whenever
// no batch is running, a goroutine drains the queue, running all queued
// functions in one transaction. A single writer pays no extra latency (its
// call runs immediately), while concurrent writers share fsyncs.
//
// When a function fails, the shared transaction is rolled back, the failing
// call is told to retry alone (or receives its error directly when it ran
// alone) and the remaining calls are re-run together, mirroring
// bbolt.DB.Batch semantics without its artificial delay.
type committer struct {
	mu      sync.Mutex
	queue   []*commitCall
	running bool
}

type commitCall struct {
	fn  func(*bbolt.Tx) error
	err chan error
}

// do runs fn in a (possibly shared) write transaction of db.
func (c *committer) do(db *bbolt.DB, fn func(*bbolt.Tx) error) error {
	call := &commitCall{fn: fn, err: make(chan error, 1)}
	c.mu.Lock()
	c.queue = append(c.queue, call)
	if !c.running {
		c.running = true
		go c.loop(db)
	}
	c.mu.Unlock()
	err := <-call.err
	if err == errTrySolo {
		err = db.Update(fn)
	}
	var pe *panicError
	if errors.As(err, &pe) {
		// Re-raise in the caller's goroutine, like a direct db.Update would.
		panic(pe.value)
	}
	return err
}

func (c *committer) loop(db *bbolt.DB) {
	for {
		c.mu.Lock()
		n := min(len(c.queue), maxCommitBatch)
		if n == 0 {
			c.running = false
			c.queue = nil
			c.mu.Unlock()
			return
		}
		calls := make([]*commitCall, n)
		copy(calls, c.queue)
		c.queue = c.queue[n:]
		c.mu.Unlock()
		runBatch(db, calls)
	}
}

func runBatch(db *bbolt.DB, calls []*commitCall) {
	for len(calls) > 0 {
		failIdx := -1
		err := db.Update(func(tx *bbolt.Tx) error {
			for i, call := range calls {
				if err := safeCall(call.fn, tx); err != nil {
					failIdx = i
					return err
				}
			}
			return nil
		})
		if failIdx >= 0 {
			failed := calls[failIdx]
			var pe *panicError
			if len(calls) == 1 || errors.As(err, &pe) {
				// It ran alone (or panicked), so the error is its own.
				failed.err <- err
				return
			}
			calls = append(calls[:failIdx:failIdx], calls[failIdx+1:]...)
			failed.err <- errTrySolo
			continue
		}
		for _, call := range calls {
			call.err <- err
		}
		return
	}
}

// panicError carries a panic out of the shared commit goroutine.
type panicError struct{ value any }

func (p *panicError) Error() string { return fmt.Sprintf("fs: panic in transaction: %v", p.value) }

// safeCall runs fn, converting a panic into a *panicError so that one faulty
// function cannot take down the shared commit goroutine; the panic is
// re-raised in the caller's goroutine by committer.do.
func safeCall(fn func(*bbolt.Tx) error, tx *bbolt.Tx) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p}
		}
	}()
	return fn(tx)
}
