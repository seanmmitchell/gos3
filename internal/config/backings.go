package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// backingNameRe matches valid backing names.
var backingNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// BackingNames returns the names of every backing: DefaultBackingName
// first, then storage.backings in order.
func (c Config) BackingNames() []string {
	names := []string{DefaultBackingName}
	for _, b := range c.Storage.Backings {
		names = append(names, b.Name)
	}
	return names
}

// validateBackings checks storage.backings, storage.default_backing and
// storage.replication.
func (c Config) validateBackings() error {
	if len(c.Storage.Backings) > MaxBackings {
		return fmt.Errorf("storage.backings: at most %d backings allowed", MaxBackings)
	}
	paths := map[string]string{} // cleaned absolute path -> key naming it
	if c.Storage.Path != "" {
		if abs, err := filepath.Abs(c.Storage.Path); err == nil {
			paths[abs] = "storage.path"
		}
	}
	names := map[string]bool{DefaultBackingName: true}
	for i, b := range c.Storage.Backings {
		key := fmt.Sprintf("storage.backings[%d]", i)
		switch {
		case b.Name == DefaultBackingName:
			return fmt.Errorf("%s.name: %q is reserved for the backing configured by storage.backend and storage.path", key, b.Name)
		case !backingNameRe.MatchString(b.Name):
			return fmt.Errorf("%s.name: %q must be 1-32 characters of a-z, 0-9, _ and -, starting with a letter or digit", key, b.Name)
		case names[b.Name]:
			return fmt.Errorf("%s.name: %q is used twice", key, b.Name)
		}
		names[b.Name] = true
		switch b.Backend {
		case BackendFS:
			if b.Path == "" {
				return fmt.Errorf("%s.path: required for an fs backing", key)
			}
			abs, err := filepath.Abs(b.Path)
			if err != nil {
				return fmt.Errorf("%s.path: %w", key, err)
			}
			if other, dup := paths[abs]; dup {
				return fmt.Errorf("%s.path: %q is also used by %s; every backing needs its own directory", key, b.Path, other)
			}
			paths[abs] = key + ".path"
		case BackendMemory:
			if b.Path != "" {
				return fmt.Errorf("%s.path: a memory backing has no path", key)
			}
		default:
			return fmt.Errorf("%s.backend: %q is invalid (expected fs or memory)", key, b.Backend)
		}
	}
	// No backing directory may contain another one's system directory
	// (gos3 keeps its metadata in <path>/.gos3).
	for p, k := range paths {
		for q, kq := range paths {
			if p != q && (q == filepath.Join(p, SystemDirName) || strings.HasPrefix(q, filepath.Join(p, SystemDirName)+string(filepath.Separator))) {
				return fmt.Errorf("%s: %q is inside the system directory of %s", kq, q, k)
			}
		}
	}

	def := c.Storage.DefaultBacking
	if def == "" {
		def = DefaultBackingName
	}
	if !names[def] {
		return fmt.Errorf("storage.default_backing: unknown backing %q (expected one of %s)", def, strings.Join(c.BackingNames(), ", "))
	}
	rp := c.Storage.Replication
	switch rp.DefaultMode {
	case "", ReplicationSingle, ReplicationActivePassive, ReplicationActiveActive:
	default:
		return fmt.Errorf("storage.replication.default_mode: %q is invalid (expected single, active-passive or active-active)", rp.DefaultMode)
	}
	for i, r := range rp.DefaultReplicas {
		switch {
		case !names[r]:
			return fmt.Errorf("storage.replication.default_replicas[%d]: unknown backing %q", i, r)
		case r == def:
			return fmt.Errorf("storage.replication.default_replicas[%d]: %q is the default backing, where new buckets have their primary copy", i, r)
		case slices.Contains(rp.DefaultReplicas[:i], r):
			return fmt.Errorf("storage.replication.default_replicas[%d]: %q is listed twice", i, r)
		}
	}
	if rp.DefaultMode == ReplicationSingle && len(rp.DefaultReplicas) > 0 {
		return errors.New("storage.replication.default_mode: single takes no default_replicas")
	}
	if (rp.DefaultMode == ReplicationActivePassive || rp.DefaultMode == ReplicationActiveActive) && len(rp.DefaultReplicas) == 0 {
		return fmt.Errorf("storage.replication.default_mode: %s needs storage.replication.default_replicas", rp.DefaultMode)
	}
	// Zero values mean the defaults (filled in by Normalize).
	if rp.Workers < 0 || rp.Workers > MaxReplicationWorkers {
		return fmt.Errorf("storage.replication.workers: must be between 1 and %d (0 = default %d), got %d",
			MaxReplicationWorkers, DefaultReplicationWorkers, rp.Workers)
	}
	if hi := rp.HealthInterval; hi != 0 && (hi < time.Second || hi > time.Hour) {
		return fmt.Errorf("storage.replication.health_interval: must be between 1s and 1h, got %s", hi)
	}
	if ct := rp.CutoverTimeout; ct != 0 && (ct < time.Second || ct > time.Hour) {
		return fmt.Errorf("storage.replication.cutover_timeout: must be between 1s and 1h, got %s", ct)
	}
	return nil
}

// backingsList parses GOS3_STORAGE_BACKINGS: comma-separated entries
// "name=/path" (an fs backing), "name=fs:/path" or "name=memory". An entry
// without "=" names an fs backing without a path (refused by Validate).
func backingsList(c *Config, v string) error {
	var out []BackingConfig
	for _, e := range SplitList(v) {
		name, spec, _ := strings.Cut(e, "=")
		b := BackingConfig{Name: strings.TrimSpace(name), Backend: BackendFS}
		spec = strings.TrimSpace(spec)
		switch {
		case spec == BackendMemory:
			b.Backend = BackendMemory
		case strings.HasPrefix(spec, BackendFS+":"):
			b.Path = strings.TrimPrefix(spec, BackendFS+":")
		default:
			b.Path = spec
		}
		out = append(out, b)
	}
	c.Storage.Backings = out
	return nil
}
