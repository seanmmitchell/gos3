package s3store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// The owner lease (P/meta/lease.json).
//
// The owner rewrites the lease every LeaseInterval with If-Match on the
// ETag of its last write; a 412 means somebody else wrote it (fenced). A
// server opening the prefix takes the lease when it is absent or released,
// or once it is older than LeaseTTL. The age is judged by the service's
// clock only (Date - Last-Modified of the same answer), or by watching the
// lease stay unchanged for LeaseTTL of local time; local and remote clocks
// are never compared. Taking it is a conditional PUT on the observed ETag,
// so of two servers opening at once one wins. Each take increments the
// epoch, which names the owner's journal epoch.
//
// The lease alone does not fence a paused owner's journal writes; the
// epoch seals do (see engine.go). It bounds how long a server that lost
// contact keeps serving reads: without a successful renewal for
// LeaseTTL - LeaseInterval it fail-stops.

// leaseDoc is the content of the lease object.
type leaseDoc struct {
	Owner    string    `json:"owner"`
	Epoch    uint64    `json:"epoch"`
	Host     string    `json:"host"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Renewals uint64    `json:"renewals"`
	Released bool      `json:"released,omitempty"`
}

// leaseState is the session's view of its lease.
type leaseState struct {
	doc    leaseDoc
	etag   string
	lastOK time.Time // local time of the last confirmed write
}

// leaseToken identifies the last lease an engine held: when the lease
// still has this owner and ETag, the engine may take it over at once.
type leaseToken struct {
	owner, etag string
}

func (l leaseState) token() leaseToken { return leaseToken{owner: l.doc.Owner, etag: l.etag} }

// errLeaseLost reports a lease that could not be renewed in time.
var errLeaseLost = errors.New("s3store: lease could not be renewed in time")

// maxJournalEpoch returns the highest epoch present in the journal.
func (e *Engine) maxJournalEpoch(ctx context.Context) (uint64, error) {
	var top uint64
	in := s3client.ListInput{Prefix: e.keys.journalRoot(), Delimiter: "/"}
	for {
		out, err := e.client().ListObjectsV2(ctx, e.opts.Bucket, in)
		if err != nil {
			return 0, remoteErr("list journal epochs", err)
		}
		for _, p := range out.CommonPrefixes {
			if ep, ok := parseHex16(p[len(e.keys.journalRoot()) : len(p)-1]); ok {
				top = max(top, ep)
			}
		}
		if !out.IsTruncated {
			return top, nil
		}
		in.ContinuationToken = out.NextContinuationToken
	}
}

// acquire takes the lease and returns a new session for its epoch, with
// the lease keeper running.
func (e *Engine) acquire(ctx context.Context) (*session, error) {
	r := e.remote()
	key := e.keys.lease()
	top, err := e.maxJournalEpoch(ctx)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	tok := e.token
	e.mu.Unlock()
	owner := randomID(16)
	var seenETag string
	var seenAt time.Time
	logged := false
	for {
		data, attrs, err := r.getBytes(ctx, key, maxSmallObject)
		var cond s3client.PutOptions
		var epoch uint64
		switch {
		case s3client.IsNotFound(err):
			cond.IfNoneMatch, epoch = true, top+1
		case err != nil:
			return nil, err
		default:
			var cur leaseDoc
			if json.Unmarshal(data, &cur) != nil {
				// An unreadable lease is judged by its age alone.
				cur = leaseDoc{}
			}
			cond.IfMatch, epoch = attrs.ETag, max(cur.Epoch, top)+1
			takeNow := cur.Released || (tok.etag != "" && tok.etag == attrs.ETag && tok.owner == cur.Owner)
			if !takeNow {
				if seenETag != "" && seenETag != attrs.ETag {
					return nil, inUse(cur)
				}
				if seenETag == "" {
					seenETag, seenAt = attrs.ETag, e.now()
				}
				age := time.Duration(-1)
				if !attrs.Date.IsZero() && !attrs.LastModified.IsZero() {
					// Both have a resolution of one second: count the
					// age conservatively.
					age = attrs.Date.Sub(attrs.LastModified) - time.Second
				}
				if age < e.opts.LeaseTTL && e.now().Sub(seenAt) < e.opts.LeaseTTL {
					if !logged {
						logged = true
						e.logger().Info("s3store: waiting for the lease of another server to expire",
							"owner", cur.Owner, "host", cur.Host, "pid", cur.PID, "age", age)
					}
					if err := sleepCtx(ctx, e.leasePoll); err != nil {
						return nil, err
					}
					continue
				}
				e.logger().Warn("s3store: taking over an expired lease", "owner", cur.Owner, "host", cur.Host,
					"pid", cur.PID, "epoch", cur.Epoch, "age", age)
			}
		}
		host, _ := os.Hostname()
		doc := leaseDoc{Owner: owner, Epoch: epoch, Host: host, PID: os.Getpid(), Started: time.Now().UTC()}
		body, _ := json.Marshal(doc)
		cond.NoRetry = true
		etag, err := r.put(ctx, key, body, cond)
		if err != nil {
			if s3client.IsPreconditionFailed(err) {
				var cur leaseDoc
				_, _ = r.getJSON(ctx, key, &cur)
				return nil, inUse(cur)
			}
			if !s3client.IsUnavailable(err) {
				return nil, remoteErr("take lease", err)
			}
			// Unknown outcome: read it back.
			var got leaseDoc
			getETag, rerr := r.getJSON(ctx, key, &got)
			switch {
			case rerr == nil && got.Owner == owner && got.Epoch == epoch:
				etag = getETag
			case rerr == nil:
				return nil, inUse(got)
			default:
				return nil, remoteErr("take lease", err)
			}
		}
		s := e.newSession(epoch, owner)
		s.lease = leaseState{doc: doc, etag: etag, lastOK: e.now()}
		e.mu.Lock()
		e.token = s.lease.token()
		e.mu.Unlock()
		go s.leaseLoop()
		return s, nil
	}
}

// inUse describes the holder of a lease.
func inUse(cur leaseDoc) error {
	if cur.Owner == "" {
		return ErrInUse
	}
	return fmt.Errorf("%w (host %q, pid %d, epoch %d, since %s)", ErrInUse, cur.Host, cur.PID, cur.Epoch,
		cur.Started.Format(time.RFC3339))
}

// leaseLoop renews the lease until the session ends.
func (s *session) leaseLoop() {
	defer close(s.leaseDone)
	e := s.e
	t := time.NewTicker(e.opts.LeaseInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if s.leasePaused.Load() {
			continue
		}
		if err := s.renew(); err != nil {
			e.failSession(s, err)
			return
		}
		if late := e.now().Sub(s.lease.lastOK); late > e.opts.LeaseTTL-e.opts.LeaseInterval {
			e.failSession(s, fmt.Errorf("%w (last renewal %v ago)", errLeaseLost, late.Round(time.Millisecond)))
			return
		}
	}
}

// renew rewrites the lease once. Only a fencing is returned as an error;
// failures to reach the service are retried at the next tick.
func (s *session) renew() error {
	e := s.e
	r := e.remote()
	key := e.keys.lease()
	ctx, cancel := context.WithTimeout(s.ctx, e.opts.LeaseInterval)
	defer cancel()
	next := s.lease.doc
	next.Renewals++
	body, _ := json.Marshal(next)
	etag, err := r.put(ctx, key, body, s3client.PutOptions{IfMatch: s.lease.etag, NoRetry: true})
	if err == nil {
		s.lease.doc, s.lease.etag, s.lease.lastOK = next, etag, e.now()
		return nil
	}
	if s.ctx.Err() != nil {
		return nil
	}
	// A 412 or an unknown outcome: look at what is there.
	var got leaseDoc
	getETag, rerr := r.getJSON(ctx, key, &got)
	switch {
	case rerr != nil && s3client.IsNotFound(rerr):
		return fmt.Errorf("%w: the lease was deleted", ErrFenced)
	case rerr != nil:
		e.logger().Warn("s3store: lease renewal failed", "err", err, "readErr", rerr)
		return nil
	case got.Owner != s.owner || got.Epoch != s.epoch:
		return fmt.Errorf("%w: lease taken by host %q pid %d (epoch %d)", ErrFenced, got.Host, got.PID, got.Epoch)
	case got.Renewals == next.Renewals:
		// Our write landed.
		s.lease.doc, s.lease.etag, s.lease.lastOK = got, getETag, e.now()
	case getETag != s.lease.etag:
		// Ours, from an earlier write whose outcome we missed.
		s.lease.doc, s.lease.etag = got, getETag
	default:
		e.logger().Warn("s3store: lease renewal failed", "err", err)
	}
	return nil
}

// releaseLease marks the lease released so another server can take over at
// once (best effort; used by Close and failed Opens).
func (s *session) releaseLease() {
	if s.lease.etag == "" {
		return
	}
	e := s.e
	doc := s.lease.doc
	doc.Released = true
	doc.Renewals++
	body, _ := json.Marshal(doc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	etag, err := e.remote().put(ctx, e.keys.lease(), body, s3client.PutOptions{IfMatch: s.lease.etag, NoRetry: true})
	if err != nil {
		e.logger().Warn("s3store: lease release failed", "err", err)
		return
	}
	s.lease.doc, s.lease.etag = doc, etag
}
