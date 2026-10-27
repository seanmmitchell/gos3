package resticsnap

import (
	"context"
	"sync"
)

// repositoryGate lets concurrent restores read while maintenance waits for
// exclusive access. A waiting writer blocks new readers, and either wait can
// be interrupted by its context.
type repositoryGate struct {
	mu             sync.Mutex
	changed        chan struct{}
	readers        int
	writer         bool
	waitingWriters int
}

func (g *repositoryGate) signal() {
	close(g.changed)
	g.changed = make(chan struct{})
}

func (g *repositoryGate) read(ctx context.Context) (func(), error) {
	return g.acquire(ctx, false)
}

func (g *repositoryGate) write(ctx context.Context) (func(), error) {
	return g.acquire(ctx, true)
}

func (g *repositoryGate) acquire(ctx context.Context, write bool) (func(), error) {
	g.mu.Lock()
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	if write {
		g.waitingWriters++
	}
	for {
		if err := ctx.Err(); err != nil {
			if write {
				g.waitingWriters--
				g.signal()
			}
			g.mu.Unlock()
			return nil, err
		}
		if !g.writer && ((write && g.readers == 0) || (!write && g.waitingWriters == 0)) {
			if write {
				g.waitingWriters--
				g.writer = true
			} else {
				g.readers++
			}
			g.mu.Unlock()
			return func() {
				g.mu.Lock()
				if write {
					g.writer = false
				} else {
					g.readers--
				}
				g.signal()
				g.mu.Unlock()
			}, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		g.mu.Lock()
	}
}
