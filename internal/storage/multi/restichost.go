package multi

import (
	"context"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// The router is a restic.Host: the repositories of a bucket are those of
// its copies' backends, and the router answers for the bucket's primary.
//
// Restic calls are not observed (see Router.observe): a repository that
// cannot be prepared (no restic executable, a missing key) says nothing
// about the health of the backing holding the bucket, whose own I/O
// failures show in the bucket's other operations.

var _ restic.Host = (*Router)(nil)

// ResticRepository implements restic.Host for the bucket's primary copy,
// which receives every write first and so holds the archive that is
// complete: it forwards to the primary's backend. A backend that keeps no
// restic repositories yields an error wrapping errors.ErrUnsupported;
// unknown buckets storage.ErrBucketNotFound. Each other copy has a
// repository of its own on its backing (see ResticRepositories). The
// info's Backing names the primary that answered.
func (r *Router) ResticRepository(ctx context.Context, bucket string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	b, err := r.primary(bucket)
	if err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	h, err := resticHost(b)
	if err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	info, loc, err := h.ResticRepository(ctx, bucket, create)
	info.Backing = b.Name
	return info, loc, err
}

// ResticAuxRepository implements restic.Host on the default backing (where
// new buckets are created): it forwards to that backing's backend, which
// chooses the repository's location and keeps its key. A caller that wants
// the repository on another backing (the whole-bucket snapshots of a
// bucket naming a backing) asks that backing's backend directly, through
// BackingBackend. A default backing whose backend keeps no restic
// repositories yields an error wrapping errors.ErrUnsupported.
func (r *Router) ResticAuxRepository(ctx context.Context, name string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	if r.closed.Load() {
		return restic.RepositoryInfo{}, restic.Location{}, ErrClosed
	}
	h, err := resticHost(r.topology.Load().def)
	if err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	return h.ResticAuxRepository(ctx, name, create)
}

// MemberRepository describes the restic repository of one copy of a bucket
// (see ResticRepositories), without secrets.
type MemberRepository struct {
	// Backing holds the copy; Role is its role in the bucket's placement
	// (RolePrimary, RoleReplica or RoleMoveTarget).
	Backing string `json:"backing"`
	Role    string `json:"role"`
	// Info describes the copy's repository; it is zero when Err is set.
	Info restic.RepositoryInfo `json:"info"`
	// Err says why the repository could not be described (for example a
	// backing that keeps no restic repositories).
	Err string `json:"error,omitempty"`
}

// ResticRepositories describes the restic repository of every copy of
// bucket: the primary first, then the replicas in placement order, then a
// move target. Each backing describes its own copy's repository without
// creating it (ResticRepository with create false); a backing that cannot
// is reported in its entry's Err rather than failing the whole answer. It
// is for the operators' recovery information and never returns a
// repository's password or credentials (restic.Location).
func (r *Router) ResticRepositories(ctx context.Context, bucket string) ([]MemberRepository, error) {
	st, err := r.Placement(bucket)
	if err != nil {
		return nil, err
	}
	topology := r.topology.Load()
	out := make([]MemberRepository, 0, len(st.Members))
	for _, m := range st.Members {
		mr := MemberRepository{Backing: m.Backing, Role: m.Role}
		info, err := memberRepository(ctx, topology.backings[m.Backing], m.Backing, bucket)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			mr.Err = err.Error()
		} else {
			mr.Info = info
		}
		out = append(out, mr)
	}
	return out, nil
}

// memberRepository describes the repository of bucket's copy on backing b
// (named name; nil when it is no longer configured).
func memberRepository(ctx context.Context, b *backing, name, bucket string) (restic.RepositoryInfo, error) {
	if b == nil {
		return restic.RepositoryInfo{}, fmt.Errorf("%w %q", ErrUnknownBacking, name)
	}
	h, err := resticHost(b)
	if err != nil {
		return restic.RepositoryInfo{}, err
	}
	info, _, err := h.ResticRepository(ctx, bucket, false)
	return info, err
}

// resticHost returns b's backend as a restic.Host, or an error wrapping
// errors.ErrUnsupported when it keeps no restic repositories.
func resticHost(b *backing) (restic.Host, error) {
	h, ok := b.Backend.(restic.Host)
	if !ok {
		return nil, fmt.Errorf("backing %q keeps no restic repositories: %w", b.Name, errors.ErrUnsupported)
	}
	return h, nil
}
