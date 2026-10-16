package multi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// resticMem is a memory backend that is a restic.Host: each bucket's
// repository is named after the backing and the bucket, and the
// passwords say whose they are (they must never leak into
// ResticRepositories).
type resticMem struct {
	*memory.Backend
	name string
	// gate, when not nil, makes CreateBucket wait until it is closed (or
	// the context ends), to hold a move in its copying phase.
	gate chan struct{}
	// fail, when set, is the error ResticRepository returns.
	fail error

	mu    sync.Mutex
	calls []string
}

var _ restic.Host = (*resticMem)(nil)

func newResticMem(name string) *resticMem {
	return &resticMem{Backend: memory.New(), name: name}
}

func (m *resticMem) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.Backend.CreateBucket(ctx, name, opts)
}

func (m *resticMem) record(call string) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
}

func (m *resticMem) callLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func (m *resticMem) ResticRepository(ctx context.Context, bucket string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	m.record(fmt.Sprintf("repo %s %v", bucket, create))
	if m.fail != nil {
		return restic.RepositoryInfo{}, restic.Location{}, m.fail
	}
	if _, err := m.GetBucket(ctx, bucket); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	repo := "/repos/" + m.name + "/" + bucket
	return restic.RepositoryInfo{BucketID: m.name + "-" + bucket, Repository: repo, PasswordSource: "record of " + m.name, Initialized: create},
		restic.Location{Repository: repo, Password: "secret-of-" + m.name}, nil
}

func (m *resticMem) ResticAuxRepository(_ context.Context, name string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	m.record(fmt.Sprintf("aux %s %v", name, create))
	repo := "/aux/" + m.name + "/" + name
	return restic.RepositoryInfo{Repository: repo, PasswordSource: "aux key of " + m.name, Initialized: create},
		restic.Location{Repository: repo, Password: "aux-secret-of-" + m.name}, nil
}

// The router answers for a bucket's restic repository with its primary's,
// for aux repositories with the default backing's, and describes the
// repository of every copy without secrets.
func TestResticHostForwarding(t *testing.T) {
	ctx := context.Background()
	a, b, d := newResticMem("a"), newResticMem("b"), newResticMem("d")
	d.gate = make(chan struct{})
	r := openRouter(t, Options{
		Backings: []Backing{
			{Name: "a", Backend: a}, {Name: "b", Backend: b},
			{Name: "c", Backend: memory.New()}, {Name: "d", Backend: d},
		},
		Default: "b",
	})
	defer close(d.gate)
	if err := r.CreateBucket(ctx, "x", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}

	info, loc, err := r.ResticRepository(ctx, "x", true)
	if err != nil || info.Repository != "/repos/b/x" || !info.Initialized || loc.Password != "secret-of-b" {
		t.Fatalf("ResticRepository = %+v, %+v, %v", info, loc, err)
	}
	info, loc, err = r.ResticAuxRepository(ctx, "snap-1", true)
	if err != nil || info.Repository != "/aux/b/snap-1" || loc.Password != "aux-secret-of-b" {
		t.Fatalf("ResticAuxRepository = %+v, %+v, %v", info, loc, err)
	}
	if calls := a.callLog(); len(calls) != 0 {
		t.Fatalf("the non-default, non-primary backing was asked: %v", calls)
	}

	// Every copy: replicas (one on a backing without repositories) and a
	// move target that has not received the bucket yet.
	if _, err := r.SetReplication(ctx, "x", ModeActivePassive, []string{"a", "c"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "x", StateInSync)
	if _, err := r.Move(ctx, "x", "c", "d", nil); err != nil {
		t.Fatal(err)
	}
	repos, err := r.ResticRepositories(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ backing, role, repo, err string }
	var got []row
	for _, m := range repos {
		got = append(got, row{m.Backing, m.Role, m.Info.Repository, m.Err})
	}
	if len(got) != 4 ||
		got[0] != (row{"b", RolePrimary, "/repos/b/x", ""}) ||
		got[1] != (row{"a", RoleReplica, "/repos/a/x", ""}) ||
		got[2].backing != "c" || got[2].role != RoleReplica || !strings.Contains(got[2].err, "keeps no restic repositories") ||
		got[3].backing != "d" || got[3].role != RoleMoveTarget || !strings.Contains(got[3].err, "not found") {
		t.Fatalf("ResticRepositories = %+v", got)
	}
	for _, m := range []*resticMem{a, b, d} {
		for _, c := range m.callLog() {
			if strings.HasPrefix(c, "repo x true") && m != b {
				t.Errorf("ResticRepositories created a repository on %s: %v", m.name, m.callLog())
			}
		}
	}
	raw, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("ResticRepositories discloses a secret: %s", raw)
	}

	// Unknown buckets.
	if _, _, err := r.ResticRepository(ctx, "nobucket", false); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("ResticRepository of an unknown bucket: %v", err)
	}
	if _, err := r.ResticRepositories(ctx, "nobucket"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("ResticRepositories of an unknown bucket: %v", err)
	}
}

// A primary without repositories answers errors.ErrUnsupported; a failing
// repository does not make its backing unhealthy.
func TestResticHostUnsupportedAndFailures(t *testing.T) {
	ctx := context.Background()
	f := newResticMem("f")
	f.fail = errors.New("restic: repository locked")
	r := openRouter(t, Options{Backings: []Backing{{Name: "m", Backend: memory.New()}, {Name: "f", Backend: f}}})
	if err := r.CreateBucket(ctx, "plain", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBucket(ctx, "locked", storage.CreateBucketOptions{Backing: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ResticRepository(ctx, "plain", true); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("ResticRepository on a memory primary: %v", err)
	}
	if _, _, err := r.ResticAuxRepository(ctx, "aux", true); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("ResticAuxRepository on a memory default backing: %v", err)
	}
	if _, _, err := r.ResticRepository(ctx, "locked", true); err == nil || err.Error() != f.fail.Error() {
		t.Fatalf("ResticRepository with a failing repository: %v", err)
	}
	repos, err := r.ResticRepositories(ctx, "locked")
	if err != nil || len(repos) != 1 || repos[0].Err != f.fail.Error() {
		t.Fatalf("ResticRepositories with a failing repository: %+v, %v", repos, err)
	}
	for _, st := range r.Backings() {
		if !st.Healthy {
			t.Fatalf("backing %s unhealthy after restic failures: %s", st.Name, st.LastError)
		}
	}
}
