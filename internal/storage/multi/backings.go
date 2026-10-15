package multi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Published topologies are immutable. Bucket operations and background workers
// can retain a backing while an unrelated empty backing is reconfigured.
type backingTopology struct {
	backings map[string]*backing
	order    []*backing
	def      *backing
}

// BackingOpener creates and reconfigures backings for live configuration
// (see Router.ConfigureBackings). Neither function is called with the
// router's placement lock held, so both may do remote I/O.
type BackingOpener struct {
	// Open opens a new backing for def (secrets included).
	Open func(def config.BackingDefinition) (Backing, error)
	// Reconfigure prepares a change of the connection settings of backing
	// b (an s3 backing: endpoint, region, credentials, encryption key,
	// addressing, part size) to those of def, which has the same identity
	// (see config.BackingDefinition.Identity) as b's current definition.
	// It verifies the new settings without changing b, and returns commit,
	// which switches b to them; commit is only called once the new
	// configuration is saved, and not at all when anything else fails
	// (the prepared change is then dropped). A nil Reconfigure makes such
	// changes replace the backing, which needs it to be unused and empty.
	Reconfigure func(ctx context.Context, b Backing, def config.BackingDefinition) (commit func(), err error)
}

type backingManagement struct {
	base   config.StorageConfig // secrets included
	path   string
	opener BackingOpener
	// generation counts the applied configurations; it is part of the
	// revision, so that a change of secrets alone changes the revision.
	generation uint64
}

// revisionNonce makes revisions unpredictable across processes: a
// revision reveals nothing about the secrets it covers.
var revisionNonce = func() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}()

// revision is hex(sha256(json(public) || revisionNonce || generation)).
func (m *backingManagement) revision(public config.BackingSettings) string {
	data, _ := json.Marshal(public)
	h := sha256.New()
	h.Write(data)
	h.Write(revisionNonce)
	h.Write(binary.BigEndian.AppendUint64(nil, m.generation))
	return hex.EncodeToString(h.Sum(nil))
}

// BackingConfiguration is the editable configuration plus its revision token.
// Secrets are never included (see config.BackingSettings.Public).
type BackingConfiguration struct {
	config.BackingSettings
	Revision   string `json:"revision"`
	Persistent bool   `json:"persistent"`
}

// EnableBackingManagement wires persistence and backend creation before serving.
// base is the startup storage configuration, secrets included.
func (r *Router) EnableBackingManagement(base config.StorageConfig, path string, opener BackingOpener) {
	base.Backings = slices.Clone(base.Backings)
	for i := range base.Backings {
		if base.Backings[i].Backend == config.BackendFS {
			base.Backings[i].Path, _ = filepath.Abs(base.Backings[i].Path)
		}
	}
	r.management = &backingManagement{base: base, path: path, opener: opener}
}

// BackingConfiguration returns a detached copy of the current configuration,
// without secrets.
func (r *Router) BackingConfiguration() (BackingConfiguration, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	return r.backingConfiguration()
}

func (r *Router) backingConfiguration() (BackingConfiguration, error) {
	m := r.management
	if m == nil {
		return BackingConfiguration{}, fmt.Errorf("%w: backing configuration is unavailable", ErrUnknownBacking)
	}
	settings := config.BackingsFrom(m.base).Public()
	return BackingConfiguration{BackingSettings: settings, Revision: m.revision(settings), Persistent: m.path != ""}, nil
}

// reconfiguration is a planned change of a backing's connection settings.
type reconfiguration struct {
	b   *backing
	def config.BackingDefinition
	// fallback: the change may replace b when Reconfigure fails (b was
	// never listed and the router places nothing on it).
	fallback bool
}

// backingPlan is a configuration change in progress (see
// ConfigureBackings).
type backingPlan struct {
	revision    string // of the configuration the change applies to
	base        config.StorageConfig
	defs        config.BackingSettings
	old         *backingTopology
	removed     []*backing // removed or replaced
	reconfigure []reconfiguration
	// opened are the new backings by name, with their buckets in listed:
	// empty, except for one replacing a backing never listed (pendingAdopt),
	// whose buckets are adopted.
	opened    map[string]*backing
	listed    map[string][]storage.BucketInfo
	adoptable map[string]bool // names whose old backing was never listed
	commits   []func()
	committed bool
}

// close closes the opened backings unless the plan was committed.
func (p *backingPlan) close() {
	if p.committed {
		return
	}
	for _, b := range p.opened {
		_ = b.Backend.Close()
	}
}

// usedLocked reports whether the router places anything on backing name:
// a copy, a move target or a pending purge. Router.mu must be held.
func (r *Router) usedLocked(name string) bool {
	for _, bs := range r.buckets {
		if bs.rec.holds(name) {
			return true
		}
	}
	for p := range r.purges {
		if p.Backing == name {
			return true
		}
	}
	return false
}

func errInUse(name string) error {
	return fmt.Errorf("%w: backing %q is in use; move its buckets and remove replicas first", ErrConflict, name)
}

// ConfigureBackings validates the new configuration, opens added and
// replaced backings and prepares connection changes, saves the
// configuration, then publishes it atomically and commits the connection
// changes. No bucket is moved or deleted by this operation.
//
// A backing is compared with its current definition by name: the same
// settings leave it alone; a different identity (see
// config.BackingDefinition.Identity: another fs path, s3 bucket, prefix or
// gateway mode) replaces it; the same identity with other connection
// settings reconfigures it in place (BackingOpener.Reconfigure), also when
// it holds buckets. Blank secrets keep the stored ones (see
// config.BackingSettings.Apply).
//
// A backing can be replaced or removed only when the router places
// nothing on it, and when it holds no buckets, unless it cannot be listed:
// a backing whose contents are unknown (an unreachable s3 service) can be
// removed or replaced, and its data is left untouched. A new backing must
// be empty, except when it replaces a backing that could never be listed
// since startup: its buckets are then adopted, as at startup. A
// reconfiguration of such a backing that fails (e.g. an s3 prefix
// without the marker of the backing) replaces it instead.
//
// The remote I/O (opening, listing, preparing connections) runs without
// the placement lock; the change is applied only when the configuration
// did not change meanwhile.
func (r *Router) ConfigureBackings(ctx context.Context, in BackingConfiguration) (BackingConfiguration, error) {
	r.adminMu.Lock()
	p, err := r.planBackings(in)
	r.adminMu.Unlock()
	if err != nil {
		return BackingConfiguration{}, err
	}
	defer p.close()
	if err := r.prepareBackings(ctx, p); err != nil {
		return BackingConfiguration{}, err
	}
	r.adminMu.Lock()
	out, err := r.applyBackings(ctx, p)
	r.adminMu.Unlock()
	if err != nil {
		return BackingConfiguration{}, err
	}
	for _, b := range p.removed {
		if err := b.Backend.Close(); err != nil {
			r.log.Error("closing removed backing", "backing", b.Name, "err", err)
		}
	}
	return out, nil
}

// planBackings validates in against the current configuration and decides
// what happens to every backing. The caller holds adminMu.
func (r *Router) planBackings(in BackingConfiguration) (*backingPlan, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	m := r.management
	if m == nil {
		return nil, fmt.Errorf("%w: backing configuration is unavailable", ErrUnknownBacking)
	}
	current, _ := r.backingConfiguration()
	if in.Revision != current.Revision {
		return nil, fmt.Errorf("%w: backing configuration changed; reload and try again", ErrConflict)
	}
	in.Backings = slices.Clone(in.Backings)
	for i := range in.Backings {
		if in.Backings[i].Backend == config.BackendFS && in.Backings[i].Path != "" {
			path, err := filepath.Abs(in.Backings[i].Path)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidPlacement, err)
			}
			in.Backings[i].Path = path
		}
	}
	base, err := in.BackingSettings.Apply(m.base)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPlacement, err)
	}
	p := &backingPlan{
		revision:  current.Revision,
		base:      base,
		defs:      config.BackingsFrom(base),
		old:       r.topology.Load(),
		opened:    map[string]*backing{},
		listed:    map[string][]storage.BucketInfo{},
		adoptable: map[string]bool{},
	}
	currentDefs := map[string]config.BackingDefinition{}
	for _, d := range config.BackingsFrom(m.base).Backings {
		currentDefs[d.Name] = d
	}
	wanted := map[string]config.BackingDefinition{}
	for _, d := range p.defs.Backings {
		wanted[d.Name] = d
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range p.old.order {
		if b.Name == config.DefaultBackingName {
			continue
		}
		used := r.usedLocked(b.Name)
		pending := r.pendingAdopt[b.Name]
		if d, exists := wanted[b.Name]; exists {
			if cur, known := currentDefs[b.Name]; known && d.Identity() == cur.Identity() {
				switch {
				case d.Equal(cur) || d.Backend != config.BackendS3:
					// Unchanged, or no setting that matters to an open
					// fs or memory backing changed.
					continue
				case m.opener.Reconfigure != nil:
					p.reconfigure = append(p.reconfigure, reconfiguration{b: b, def: d, fallback: pending && !used})
					continue
				}
			}
		}
		if used {
			return nil, errInUse(b.Name)
		}
		p.removed = append(p.removed, b)
	}
	return p, nil
}

// prepareBackings does the remote I/O of plan p: it checks the removed
// backings, prepares the connection changes and opens the new backings.
// It runs without adminMu.
func (r *Router) prepareBackings(ctx context.Context, p *backingPlan) error {
	m := r.management
	r.mu.RLock()
	for _, b := range p.removed {
		p.adoptable[b.Name] = r.pendingAdopt[b.Name]
	}
	r.mu.RUnlock()
	for _, b := range p.removed {
		buckets, err := b.Backend.ListBuckets(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			r.log.Warn("removing or replacing a storage backing whose buckets cannot be listed; its data is left untouched",
				"backing", b.Name, "err", err)
		case len(buckets) != 0:
			return fmt.Errorf("%w: backing %q still contains buckets", ErrConflict, b.Name)
		}
	}
	for _, rc := range p.reconfigure {
		commit, err := m.opener.Reconfigure(ctx, rc.b.Backing, rc.def)
		if err != nil {
			if !rc.fallback || ctx.Err() != nil {
				return fmt.Errorf("reconfiguring backing %q: %w", rc.b.Name, err)
			}
			r.log.Warn("could not reconfigure a storage backing that was never listed; replacing it instead",
				"backing", rc.b.Name, "err", err)
			p.removed = append(p.removed, rc.b)
			p.adoptable[rc.b.Name] = true
			continue
		}
		if commit != nil {
			p.commits = append(p.commits, commit)
		}
	}
	for _, d := range p.defs.Backings {
		if b := p.old.backings[d.Name]; b != nil && !slices.Contains(p.removed, b) {
			continue
		}
		v, err := m.opener.Open(d)
		if err != nil {
			return fmt.Errorf("opening backing %q: %w", d.Name, err)
		}
		b := &backing{Backing: v}
		p.opened[d.Name] = b
		buckets, err := b.Backend.ListBuckets(ctx)
		if err != nil {
			return fmt.Errorf("checking new backing %q: %w", d.Name, err)
		}
		if len(buckets) != 0 && !p.adoptable[d.Name] {
			return fmt.Errorf("%w: new backing %q must be empty; configure existing data at startup", ErrConflict, d.Name)
		}
		p.listed[d.Name] = buckets
		b.setUp(time.Now().UTC())
	}
	return ctx.Err()
}

// applyBackings saves and publishes plan p, commits its connection changes
// and adopts the buckets of backings replacing never-listed ones. The
// caller holds adminMu.
func (r *Router) applyBackings(ctx context.Context, p *backingPlan) (BackingConfiguration, error) {
	if r.closed.Load() {
		return BackingConfiguration{}, ErrClosed
	}
	m := r.management
	if current, _ := r.backingConfiguration(); current.Revision != p.revision {
		return BackingConfiguration{}, fmt.Errorf("%w: backing configuration changed; reload and try again", ErrConflict)
	}
	// Placements may have changed while the lock was released.
	r.mu.RLock()
	for _, b := range p.removed {
		if r.usedLocked(b.Name) {
			r.mu.RUnlock()
			return BackingConfiguration{}, errInUse(b.Name)
		}
	}
	r.mu.RUnlock()
	next := &backingTopology{
		backings: map[string]*backing{config.DefaultBackingName: p.old.backings[config.DefaultBackingName]},
		order:    []*backing{p.old.backings[config.DefaultBackingName]},
	}
	for _, d := range p.defs.Backings {
		b := p.opened[d.Name]
		if b == nil {
			b = p.old.backings[d.Name]
		}
		next.backings[d.Name] = b
		next.order = append(next.order, b)
	}
	next.def = next.backings[p.defs.DefaultBacking]
	if next.def == nil {
		return BackingConfiguration{}, fmt.Errorf("%w: default backing %q", ErrUnknownBacking, p.defs.DefaultBacking)
	}
	for _, name := range r.opts.NewBucketReplicas {
		switch b := next.backings[name]; {
		case b == nil:
			return BackingConfiguration{}, fmt.Errorf("%w: backing %q is a new bucket replica", ErrInvalidPlacement, name)
		case !canImport(b):
			return BackingConfiguration{}, fmt.Errorf("%w: new bucket replica: %s", ErrInvalidPlacement, errNoImport(name))
		}
	}
	if len(r.opts.NewBucketReplicas) > 0 && !canImport(next.def) {
		return BackingConfiguration{}, fmt.Errorf("%w: the default backing %q is an S3 gateway backing, which cannot "+
			"have the replicas new buckets get", ErrInvalidPlacement, next.def.Name)
	}
	if err := ctx.Err(); err != nil {
		return BackingConfiguration{}, err
	}
	if err := p.defs.Save(m.path); err != nil {
		return BackingConfiguration{}, fmt.Errorf("saving backing configuration: %w", err)
	}
	r.topology.Store(next)
	m.base = p.base
	m.generation++
	p.committed = true
	for _, commit := range p.commits {
		commit()
	}
	for _, rc := range p.reconfigure {
		if b := next.backings[rc.b.Name]; b == rc.b {
			r.log.Info("storage backing reconfigured", "backing", rc.b.Name)
			// A backing that failed with its old settings may work now.
			r.requestProbe(rc.b)
		}
	}
	// A removed backing's pending adoption ends with it; a replacement's
	// buckets are adopted now.
	r.mu.Lock()
	for _, b := range p.removed {
		delete(r.pendingAdopt, b.Name)
	}
	r.mu.Unlock()
	adopted := false
	for name, b := range p.opened {
		if len(p.listed[name]) > 0 {
			adopted = r.adoptListed(b, p.listed[name]) || adopted
		}
	}
	if adopted {
		r.saveLogged("adopting buckets")
	}
	return r.backingConfiguration()
}
