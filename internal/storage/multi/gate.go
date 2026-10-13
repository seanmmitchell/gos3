package multi

import (
	"context"
	"hash/fnv"
	"sync"
)

// gate counts the mutating operations in flight on one bucket and lets a
// cutover (the switch of a bucket's primary, or of an active-active
// replica to synchronous writes) wait until none is in flight while new
// ones wait at the gate. Reads never pass the gate.
type gate struct {
	mu     sync.Mutex
	active int
	closed chan struct{} // non-nil while quiesced; closed when the gate reopens
	idle   chan struct{} // non-nil while quiesce waits; closed when active drops to 0
}

// enter waits until the gate is open and counts one operation in. Every
// successful enter must be paired with exactly one leave.
func (g *gate) enter(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.closed == nil {
			g.active++
			g.mu.Unlock()
			return nil
		}
		ch := g.closed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// leave counts one operation out.
func (g *gate) leave() {
	g.mu.Lock()
	g.active--
	if g.active == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
	g.mu.Unlock()
}

// quiesce closes the gate and waits until no operation is in flight. On
// success the caller has the bucket to itself until it calls release. When
// ctx ends first the gate is reopened and ctx's error returned. Only one
// quiesce runs at a time; a second one waits for the first to release.
func (g *gate) quiesce(ctx context.Context) (release func(), err error) {
	for {
		g.mu.Lock()
		if g.closed == nil {
			break
		}
		ch := g.closed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	closed := make(chan struct{})
	g.closed = closed
	release = func() {
		g.mu.Lock()
		if g.closed == closed {
			close(closed)
			g.closed = nil
			g.idle = nil
		}
		g.mu.Unlock()
	}
	if g.active == 0 {
		g.mu.Unlock()
		return release, nil
	}
	idle := make(chan struct{})
	g.idle = idle
	g.mu.Unlock()
	select {
	case <-idle:
		return release, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// keyLockStripes is the number of mutexes keyLocks hashes keys onto.
const keyLockStripes = 512

// keyLocks serializes work on one object key: a synchronous active-active
// write (primary commit plus replica commits) and every copy of the key to
// a replica, so that a replica receives a key's versions in the primary's
// order. Unrelated keys may share a stripe, which only costs concurrency.
type keyLocks struct {
	stripes [keyLockStripes]sync.Mutex
}

// lock locks bucket/key and returns the unlock function.
func (k *keyLocks) lock(bucket, key string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucket))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	m := &k.stripes[h.Sum32()%keyLockStripes]
	m.Lock()
	return m.Unlock
}
