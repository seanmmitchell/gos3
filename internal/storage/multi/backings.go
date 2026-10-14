package multi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
)

// Published topologies are immutable. Bucket operations and background workers
// can retain a backing while an unrelated empty backing is reconfigured.
type backingTopology struct {
	backings map[string]*backing
	order    []*backing
	def      *backing
}

type backingManagement struct {
	base config.StorageConfig
	path string
	open func(config.BackingDefinition) (Backing, error)
}

// BackingConfiguration is the editable configuration plus its revision token.
type BackingConfiguration struct {
	config.BackingSettings
	Revision   string `json:"revision"`
	Persistent bool   `json:"persistent"`
}

// EnableBackingManagement wires persistence and backend creation before serving.
func (r *Router) EnableBackingManagement(base config.StorageConfig, path string, open func(config.BackingDefinition) (Backing, error)) {
	base.Backings = slices.Clone(base.Backings)
	for i := range base.Backings {
		if base.Backings[i].Backend == config.BackendFS {
			base.Backings[i].Path, _ = filepath.Abs(base.Backings[i].Path)
		}
	}
	r.management = &backingManagement{base: base, path: path, open: open}
}

// BackingConfiguration returns a detached copy of the current configuration.
func (r *Router) BackingConfiguration() (BackingConfiguration, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	return r.backingConfiguration()
}

func (r *Router) backingConfiguration() (BackingConfiguration, error) {
	if r.management == nil {
		return BackingConfiguration{}, fmt.Errorf("%w: backing configuration is unavailable", ErrUnknownBacking)
	}
	settings := config.BackingsFrom(r.management.base)
	data, _ := json.Marshal(settings)
	return BackingConfiguration{BackingSettings: settings, Revision: fmt.Sprintf("%x", sha256.Sum256(data)), Persistent: r.management.path != ""}, nil
}

// ConfigureBackings validates and opens replacements, saves the configuration,
// then publishes it atomically. No bucket is moved or deleted by this operation.
func (r *Router) ConfigureBackings(ctx context.Context, in BackingConfiguration) (BackingConfiguration, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	if r.closed.Load() {
		return BackingConfiguration{}, ErrClosed
	}
	m := r.management
	if m == nil {
		return BackingConfiguration{}, fmt.Errorf("%w: backing configuration is unavailable", ErrUnknownBacking)
	}
	current, _ := r.backingConfiguration()
	if in.Revision != current.Revision {
		return BackingConfiguration{}, fmt.Errorf("%w: backing configuration changed; reload and try again", ErrConflict)
	}
	in.Backings = slices.Clone(in.Backings)
	for i := range in.Backings {
		if in.Backings[i].Backend == config.BackendFS && in.Backings[i].Path != "" {
			path, err := filepath.Abs(in.Backings[i].Path)
			if err != nil {
				return BackingConfiguration{}, fmt.Errorf("%w: %v", ErrInvalidPlacement, err)
			}
			in.Backings[i].Path = path
		}
	}
	base, err := in.BackingSettings.Apply(m.base)
	if err != nil {
		return BackingConfiguration{}, fmt.Errorf("%w: %v", ErrInvalidPlacement, err)
	}
	old := r.topology.Load()
	next := &backingTopology{backings: map[string]*backing{"default": old.backings["default"]}, order: []*backing{old.backings["default"]}}
	defs := config.BackingsFrom(base)
	wanted := map[string]config.BackingDefinition{}
	for _, b := range defs.Backings {
		wanted[b.Name] = b
	}
	var removed []*backing
	for _, b := range old.order {
		if b.Name == config.DefaultBackingName {
			continue
		}
		d, exists := wanted[b.Name]
		if exists && d.Backend == b.Kind && d.Path == b.Path {
			continue
		}
		// Membership includes move targets; purges can outlive membership.
		r.mu.RLock()
		used := false
		for _, bs := range r.buckets {
			used = used || bs.rec.holds(b.Name)
		}
		for p := range r.purges {
			used = used || p.Backing == b.Name
		}
		r.mu.RUnlock()
		if used {
			return BackingConfiguration{}, fmt.Errorf("%w: backing %q is in use; move its buckets and remove replicas first", ErrConflict, b.Name)
		}
		buckets, err := b.Backend.ListBuckets(ctx)
		if err != nil {
			return BackingConfiguration{}, fmt.Errorf("checking backing %q: %w", b.Name, err)
		}
		if len(buckets) != 0 {
			return BackingConfiguration{}, fmt.Errorf("%w: backing %q still contains buckets", ErrConflict, b.Name)
		}
		removed = append(removed, b)
	}
	var opened []*backing
	committed := false
	defer func() {
		if !committed {
			for _, b := range opened {
				_ = b.Backend.Close()
			}
		}
	}()
	for _, d := range defs.Backings {
		b := old.backings[d.Name]
		if b == nil || slices.Contains(removed, b) {
			v, err := m.open(d)
			if err != nil {
				return BackingConfiguration{}, fmt.Errorf("opening backing %q: %w", d.Name, err)
			}
			b = &backing{Backing: v}
			opened = append(opened, b)
			buckets, err := b.Backend.ListBuckets(ctx)
			if err != nil {
				return BackingConfiguration{}, err
			}
			if len(buckets) != 0 {
				return BackingConfiguration{}, fmt.Errorf("%w: new backing %q must be empty; configure existing data at startup", ErrConflict, d.Name)
			}
			b.setUp(time.Now().UTC())
		}
		next.backings[d.Name] = b
		next.order = append(next.order, b)
	}
	next.def = next.backings[defs.DefaultBacking]
	if err := ctx.Err(); err != nil {
		return BackingConfiguration{}, err
	}
	if err := defs.Save(m.path); err != nil {
		return BackingConfiguration{}, fmt.Errorf("saving backing configuration: %w", err)
	}
	r.topology.Store(next)
	m.base = base
	committed = true
	for _, b := range removed {
		if err := b.Backend.Close(); err != nil {
			r.log.Error("closing removed backing", "backing", b.Name, "err", err)
		}
	}
	return r.backingConfiguration()
}
