package multi

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Member roles.
const (
	RolePrimary    = "primary"
	RoleReplica    = "replica"
	RoleMoveTarget = "move-target"
)

// Member states (see MemberStatus.State).
const (
	StateInSync   = "in-sync"
	StateSyncing  = "syncing"
	StateLagging  = "lagging"
	StateDegraded = "degraded"
	StateOffline  = "offline"
)

// BackingStatus describes a backing (GET /_gos3/admin/v1/backings).
type BackingStatus struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Location string `json:"location,omitempty"` // s3 backings: e.g. "s3://bucket/prefix/"
	// Kinds are the bucket kinds the backing offers and DefaultKind the
	// one new buckets get, for backings with several (s3 backings, see
	// storage.KindReporter); empty otherwise.
	Kinds       []string  `json:"kinds,omitempty"`
	DefaultKind string    `json:"defaultKind,omitempty"`
	Path        string    `json:"path"`
	Default     bool      `json:"default"`
	Healthy     bool      `json:"healthy"`
	LastError   string    `json:"lastError"`
	CheckedAt   time.Time `json:"checkedAt,omitzero"`
	Buckets     int       `json:"buckets"`  // buckets whose primary copy is here
	Replicas    int       `json:"replicas"` // replica copies and move targets here
}

// PlacementStatus describes a bucket's placement and the state of every
// copy.
type PlacementStatus struct {
	Bucket  string         `json:"bucket"`
	Mode    Mode           `json:"mode"`
	Primary string         `json:"primary"`
	Members []MemberStatus `json:"members"`
	Move    *MoveStatus    `json:"move"`
}

// MemberStatus is the state of one copy of a bucket.
type MemberStatus struct {
	Backing    string    `json:"backing"`
	Role       string    `json:"role"`
	State      string    `json:"state"`
	Healthy    bool      `json:"healthy"`
	Pending    int       `json:"pending"`
	Progress   *Progress `json:"progress"`
	LastSyncAt time.Time `json:"lastSyncAt,omitzero"`
	LastError  string    `json:"lastError"`
}

// MoveStatus describes a move in progress.
type MoveStatus struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"startedAt"`
	Phase     string    `json:"phase"`
	// Target is the kind and location the move creates the bucket with on
	// To (see Router.Move); nil for the backing's choice.
	Target *storage.BucketTarget `json:"target,omitempty"`
}

// PlacementSummary is the short form of a placement (bucket lists).
type PlacementSummary struct {
	Mode     Mode     `json:"mode"`
	Primary  string   `json:"primary"`
	Replicas []string `json:"replicas"`
	Moving   bool     `json:"moving"`
}

// Backings describes every configured backing, in configuration order.
func (r *Router) Backings() []BackingStatus {
	primaries := map[string]int{}
	replicas := map[string]int{}
	r.mu.RLock()
	for _, bs := range r.buckets {
		for i, m := range bs.rec.Members {
			if i == 0 {
				primaries[m.Backing]++
			} else {
				replicas[m.Backing]++
			}
		}
		if bs.rec.Move != nil {
			replicas[bs.rec.Move.To]++
		}
	}
	r.mu.RUnlock()
	topology := r.topology.Load()
	out := make([]BackingStatus, 0, len(topology.order))
	for _, b := range topology.order {
		b.mu.Lock()
		st := BackingStatus{
			Name: b.Name, Kind: b.Kind, Location: b.Location, Path: b.Path, Default: b == topology.def,
			Healthy: b.healthy.Load(), LastError: b.lastErr, CheckedAt: b.checkedAt,
			Buckets: primaries[b.Name], Replicas: replicas[b.Name],
		}
		b.mu.Unlock()
		if st.Healthy {
			st.LastError = ""
		}
		if kr, ok := b.Backend.(storage.KindReporter); ok {
			kinds, def := kr.BucketKinds()
			for _, k := range kinds {
				st.Kinds = append(st.Kinds, string(k))
			}
			st.DefaultKind = string(def)
		}
		out = append(out, st)
	}
	return out
}

// BackingBackend returns the backend of the backing name (for the admin
// API's view of an s3 backing's remote buckets), or false for an unknown
// name. Live backing configuration may replace or remove the backing
// later, closing that backend: callers use it for the request at hand
// only.
func (r *Router) BackingBackend(name string) (storage.Backend, bool) {
	b := r.topology.Load().backings[name]
	if b == nil {
		return nil, false
	}
	return b.Backend, true
}

// Summary returns the short placement of bucket.
func (r *Router) Summary(bucket string) (PlacementSummary, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return PlacementSummary{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec := bs.rec
	s := PlacementSummary{Mode: rec.Mode, Primary: rec.primary(), Replicas: []string{}, Moving: rec.Move != nil}
	for _, m := range rec.Members[1:] {
		s.Replicas = append(s.Replicas, m.Backing)
	}
	return s, nil
}

// Placement describes the placement of bucket and the state of its copies.
func (r *Router) Placement(bucket string) (PlacementStatus, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return PlacementStatus{}, err
	}
	return r.status(bs), nil
}

func (r *Router) status(bs *bucketState) PlacementStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec := bs.rec
	st := PlacementStatus{Bucket: bs.name, Mode: rec.Mode, Primary: rec.primary(), Members: []MemberStatus{}}
	for i, m := range rec.Members {
		role := RoleReplica
		if i == 0 {
			role = RolePrimary
		}
		st.Members = append(st.Members, r.memberStatus(rec, bs.members[m.Backing], role, m.Synced))
	}
	if mv := rec.Move; mv != nil {
		st.Members = append(st.Members, r.memberStatus(rec, bs.members[mv.To], RoleMoveTarget, false))
		st.Move = &MoveStatus{From: mv.From, To: mv.To, StartedAt: mv.StartedAt, Phase: bs.movePh, Target: cloneTarget(mv.Target)}
	}
	return st
}

// memberStatus builds a member's status. Router.mu must be held.
func (r *Router) memberStatus(rec *placementRecord, ms *memberState, role string, synced bool) MemberStatus {
	ms.mu.Lock()
	out := MemberStatus{
		Backing:    ms.backing.Name,
		Role:       role,
		Healthy:    ms.backing.healthy.Load(),
		Pending:    len(ms.dirty),
		LastSyncAt: ms.lastSyncAt,
		LastError:  ms.lastErr,
	}
	if ms.progress != nil {
		p := *ms.progress
		out.Progress = &p
	}
	needFull, failures, fullDone := ms.needFull, ms.failures, ms.fullDone
	ms.mu.Unlock()
	switch {
	case !out.Healthy:
		out.State = StateOffline
	case role == RolePrimary:
		out.State = StateInSync
	case failures > 0:
		out.State = StateDegraded
	case out.Progress != nil || needFull || (role == RoleReplica && !synced) || (role == RoleMoveTarget && !fullDone):
		out.State = StateSyncing
	case rec.Mode == ModeActiveActive && role == RoleReplica:
		if ms.syncMode {
			out.State = StateInSync
		} else {
			out.State = StateLagging
		}
	case out.Pending > 0:
		out.State = StateLagging
	default:
		out.State = StateInSync
	}
	if role == RolePrimary {
		out.Pending, out.LastError = 0, ""
	}
	return out
}

// backingByName validates a backing name from a request.
func (r *Router) backingByName(name string) (*backing, error) {
	b := r.topology.Load().backings[name]
	if b == nil {
		return nil, fmt.Errorf("%w %q", ErrUnknownBacking, name)
	}
	return b, nil
}

// SetReplication sets the bucket's replication mode and replica backings
// (the primary stays). New replicas start with a full copy, in a bucket
// their backing creates to receive copies (storage.CreateBucketOptions
// Import, with the backing's default location); removed ones have their
// copy deleted in the background. A bucket whose versions cannot be
// copied (a native bucket) gets no replicas.
func (r *Router) SetReplication(ctx context.Context, bucket string, mode Mode, replicas []string) (PlacementStatus, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	if !mode.Valid() {
		return PlacementStatus{}, fmt.Errorf("%w: unknown mode %q (expected single, active-passive or active-active)", ErrInvalidPlacement, mode)
	}
	if mode == ModeSingle && len(replicas) > 0 {
		return PlacementStatus{}, fmt.Errorf("%w: mode single takes no replicas", ErrInvalidPlacement)
	}
	if mode != ModeSingle && len(replicas) == 0 {
		return PlacementStatus{}, fmt.Errorf("%w: mode %s needs at least one replica", ErrInvalidPlacement, mode)
	}
	for i, name := range replicas {
		if _, err := r.backingByName(name); err != nil {
			return PlacementStatus{}, err
		}
		if slices.Contains(replicas[:i], name) {
			return PlacementStatus{}, fmt.Errorf("%w: replica %q is listed twice", ErrInvalidPlacement, name)
		}
	}
	bs, err := r.lookup(bucket)
	if err != nil {
		return PlacementStatus{}, err
	}
	if err := r.checkNewMembers(ctx, bucket, bs, replicas, nil); err != nil {
		return PlacementStatus{}, err
	}

	r.mu.Lock()
	rec := bs.rec
	if rec.Move != nil {
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: a move of bucket %q is in progress", ErrConflict, bucket)
	}
	if slices.Contains(replicas, rec.primary()) {
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: %q is the primary of bucket %q", ErrInvalidPlacement, rec.primary(), bucket)
	}
	for _, name := range replicas {
		if rec.member(name) < 0 && r.purges[purgeRecord{Bucket: bucket, Backing: name}] != nil {
			r.mu.Unlock()
			return PlacementStatus{}, fmt.Errorf("%w: a previous copy of bucket %q on backing %q is still being deleted; retry later",
				ErrConflict, bucket, name)
		}
	}
	next := rec.clone()
	next.Mode = mode
	next.Members = next.Members[:1]
	var added, removed []*memberState
	for _, m := range rec.Members[1:] {
		if slices.Contains(replicas, m.Backing) {
			next.Members = append(next.Members, m)
		} else {
			removed = append(removed, bs.members[m.Backing])
		}
	}
	for _, name := range replicas {
		if rec.member(name) < 0 {
			next.Members = append(next.Members, memberRecord{Backing: name})
			ms := r.newMemberState(bs, name)
			ms.needFull = true
			bs.members[name] = ms
			added = append(added, ms)
		}
	}
	for _, ms := range removed {
		delete(bs.members, ms.backing.Name)
		r.addPurgeLocked(bucket, ms.backing.Name)
	}
	if mode != ModeActiveActive {
		for _, ms := range bs.members {
			ms.syncMode = false
		}
	}
	bs.rec = next
	r.mu.Unlock()

	for _, ms := range removed {
		r.cancelMember(ms)
	}
	saveErr := r.save()
	for _, ms := range bs.members {
		if ms.backing.Name != next.primary() {
			r.kick(ms)
		}
	}
	r.mu.RLock()
	for _, ms := range removed {
		r.startPurge(r.purges[purgeRecord{Bucket: bucket, Backing: ms.backing.Name}])
	}
	r.mu.RUnlock()
	if saveErr != nil {
		return r.status(bs), fmt.Errorf("the placement changed but could not be saved: %w", saveErr)
	}
	if len(added) > 0 || len(removed) > 0 || rec.Mode != mode {
		r.log.Info("bucket placement changed", "bucket", bucket, "mode", mode, "primary", next.primary(),
			"replicas", memberNames(next.Members[1:]))
	}
	return r.status(bs), nil
}

// Move starts moving the copy of bucket on from (the primary when "") to
// the backing to. target chooses the kind and location of the bucket
// created on to (nil: the backing's choice for copies); it must name a
// bucket that can receive copies (not a native bucket), and needs a
// backing with bucket kinds (else storage.ErrInvalidTarget). It is
// persisted with the move. The target is filled in the background; the
// cutover happens when it has caught up. A bucket whose versions cannot be
// copied (a native bucket) cannot be moved.
func (r *Router) Move(ctx context.Context, bucket, from, to string, target *storage.BucketTarget) (PlacementStatus, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	if _, err := r.backingByName(to); err != nil {
		return PlacementStatus{}, err
	}
	bs, err := r.lookup(bucket)
	if err != nil {
		return PlacementStatus{}, err
	}
	target = cloneTarget(target) // the caller keeps its own
	if err := r.checkNewMembers(ctx, bucket, bs, []string{to}, target); err != nil {
		return PlacementStatus{}, err
	}
	r.mu.Lock()
	rec := bs.rec
	if from == "" {
		from = rec.primary()
	}
	switch {
	case rec.Move != nil:
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: a move of bucket %q is already in progress", ErrConflict, bucket)
	case rec.member(from) < 0:
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: backing %q holds no copy of bucket %q", ErrInvalidPlacement, from, bucket)
	case rec.member(to) >= 0:
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: backing %q already holds a copy of bucket %q", ErrConflict, to, bucket)
	case r.purges[purgeRecord{Bucket: bucket, Backing: to}] != nil:
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: a previous copy of bucket %q on backing %q is still being deleted; retry later",
			ErrConflict, bucket, to)
	}
	next := rec.clone()
	next.Move = &moveRecord{From: from, To: to, StartedAt: time.Now().UTC(), Target: target}
	ms := r.newMemberState(bs, to)
	ms.needFull = true
	bs.members[to] = ms
	bs.movePh = phaseCopying
	bs.rec = next
	r.mu.Unlock()
	saveErr := r.save()
	r.kick(ms)
	r.log.Info("bucket move started", "bucket", bucket, "from", from, "to", to)
	if saveErr != nil {
		return r.status(bs), fmt.Errorf("the move started but could not be saved: %w", saveErr)
	}
	return r.status(bs), nil
}

// CancelMove cancels the move of bucket in progress; the partial copy is
// deleted in the background.
func (r *Router) CancelMove(ctx context.Context, bucket string) (PlacementStatus, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	bs, err := r.lookup(bucket)
	if err != nil {
		return PlacementStatus{}, err
	}
	r.mu.Lock()
	mv := bs.rec.Move
	if mv == nil {
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: no move of bucket %q is in progress", ErrConflict, bucket)
	}
	next := bs.rec.clone()
	next.Move = nil
	bs.rec = next
	bs.movePh = ""
	ms := bs.members[mv.To]
	delete(bs.members, mv.To)
	r.addPurgeLocked(bucket, mv.To)
	r.mu.Unlock()
	if ms != nil {
		r.cancelMember(ms)
	}
	saveErr := r.save()
	r.mu.RLock()
	r.startPurge(r.purges[purgeRecord{Bucket: bucket, Backing: mv.To}])
	r.mu.RUnlock()
	r.log.Info("bucket move canceled", "bucket", bucket, "from", mv.From, "to", mv.To)
	if saveErr != nil {
		return r.status(bs), fmt.Errorf("the move was canceled but that could not be saved: %w", saveErr)
	}
	return r.status(bs), nil
}

// ReleaseBucket makes the router forget bucket without deleting any of its
// data: the backing holding it lets it go (storage.BucketReleaser; a
// native bucket of an s3 backing keeps its remote bucket and objects), and
// the bucket's placement is removed without the background deletion that
// follows DeleteBucket. The bucket must have a single copy (remove its
// replicas first) and no move in progress (ErrConflict otherwise); a
// backing that cannot release the bucket answers storage.ErrNotSupported.
func (r *Router) ReleaseBucket(ctx context.Context, name string) error {
	if r.closed.Load() {
		return ErrClosed
	}
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	bs, err := r.lookup(name)
	if err != nil {
		return err
	}
	r.mu.RLock()
	rec := bs.rec
	prim := bs.members[rec.primary()].backing
	r.mu.RUnlock()
	switch {
	case rec.Move != nil:
		return fmt.Errorf("%w: a move of bucket %q is in progress; cancel it before releasing the bucket", ErrConflict, name)
	case len(rec.Members) > 1:
		return fmt.Errorf("%w: bucket %q has replicas on %v; remove replicas first", ErrConflict, name, memberNames(rec.Members[1:]))
	}
	rel, ok := prim.Backend.(storage.BucketReleaser)
	if !ok {
		return fmt.Errorf("backing %q cannot release buckets without deleting their data: %w", prim.Name, storage.ErrNotSupported)
	}
	if err := rel.ReleaseBucket(ctx, name); err != nil {
		return r.observe(prim, err)
	}
	var cancel []*memberState
	r.mu.Lock()
	bs.removed = true
	delete(r.buckets, name)
	for _, ms := range bs.members {
		cancel = append(cancel, ms)
	}
	r.mu.Unlock()
	for _, ms := range cancel {
		r.cancelMember(ms)
	}
	r.log.Info("bucket released; its data is kept", "bucket", name, "backing", prim.Name)
	// A failed save is repaired at the next start: a placed bucket that no
	// backing holds any more is forgotten.
	r.saveLogged("releasing a bucket")
	return nil
}

// Promote makes the replica on backingName the bucket's primary. Without
// force the replica must be fully synced and healthy, and its pending
// changes are copied during a cutover first, so nothing is lost. force
// switches at once: changes not yet copied to the replica are lost.
func (r *Router) Promote(ctx context.Context, bucket, backingName string, force bool) (PlacementStatus, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	if _, err := r.backingByName(backingName); err != nil {
		return PlacementStatus{}, err
	}
	bs, err := r.lookup(bucket)
	if err != nil {
		return PlacementStatus{}, err
	}
	r.mu.RLock()
	rec := bs.rec
	i := rec.member(backingName)
	ms := bs.members[backingName]
	prim := bs.members[rec.primary()].backing
	r.mu.RUnlock()
	switch {
	case i < 0:
		return PlacementStatus{}, fmt.Errorf("%w: backing %q is not a replica of bucket %q", ErrInvalidPlacement, backingName, bucket)
	case i == 0:
		return r.status(bs), nil
	case rec.Move != nil:
		return PlacementStatus{}, fmt.Errorf("%w: a move of bucket %q is in progress", ErrConflict, bucket)
	}
	if !force {
		ms.mu.Lock()
		busy := ms.needFull || ms.progress != nil || !ms.fullDone
		ms.mu.Unlock()
		switch {
		case !rec.Members[i].Synced:
			return PlacementStatus{}, fmt.Errorf("%w: the replica on %q has not finished its initial copy (use force to promote it anyway)", ErrConflict, backingName)
		case !ms.backing.healthy.Load():
			return PlacementStatus{}, fmt.Errorf("%w: backing %q is unhealthy", ErrConflict, backingName)
		case !prim.healthy.Load():
			return PlacementStatus{}, fmt.Errorf("%w: the primary backing %q is unhealthy, so the replica cannot catch up (use force to promote it anyway)", ErrConflict, prim.Name)
		case busy:
			return PlacementStatus{}, fmt.Errorf("%w: the replica on %q is still being compared with the primary; retry when it is in sync (or use force)", ErrConflict, backingName)
		}
		release, err := r.quiesce(ctx, bs)
		if err != nil {
			return PlacementStatus{}, fmt.Errorf("%w: %w", ErrConflict, err)
		}
		defer release()
		if err := r.drain(ctx, ms); err != nil {
			return PlacementStatus{}, fmt.Errorf("%w: copying the last changes to %q: %w", ErrConflict, backingName, err)
		}
	}
	r.mu.Lock()
	if bs.removed || bs.rec != rec {
		r.mu.Unlock()
		return PlacementStatus{}, fmt.Errorf("%w: the placement of bucket %q changed meanwhile; retry", ErrConflict, bucket)
	}
	r.promoteLocked(bs, i, force)
	r.mu.Unlock()
	r.log.Warn("promoted a bucket replica to primary", "bucket", bucket, "new_primary", backingName,
		"old_primary", prim.Name, "forced", force)
	if err := r.save(); err != nil {
		return r.status(bs), fmt.Errorf("the replica was promoted but that could not be saved: %w", err)
	}
	return r.status(bs), nil
}

// Resync schedules a full comparison of the replica on backingName (every
// replica and the move target when "") with the primary.
func (r *Router) Resync(ctx context.Context, bucket, backingName string) (PlacementStatus, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	bs, err := r.lookup(bucket)
	if err != nil {
		return PlacementStatus{}, err
	}
	r.mu.RLock()
	prim := bs.rec.primary()
	var targets []*memberState
	for name, ms := range bs.members {
		if name != prim && (backingName == "" || name == backingName) {
			targets = append(targets, ms)
		}
	}
	r.mu.RUnlock()
	if backingName != "" && len(targets) == 0 {
		return PlacementStatus{}, fmt.Errorf("%w: backing %q is not a replica of bucket %q", ErrInvalidPlacement, backingName, bucket)
	}
	for _, ms := range targets {
		ms.mu.Lock()
		ms.failures = 0
		if ms.retry != nil {
			ms.retry.Stop()
			ms.retry = nil
		}
		ms.mu.Unlock()
		r.markFull(ms)
	}
	return r.status(bs), nil
}
