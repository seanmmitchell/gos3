package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BackingDefinition is an additional backing managed through the admin API.
type BackingDefinition struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Path    string `json:"path"`
}

// BackingSettings overrides backing definitions and the new-bucket default.
// The default system backing and replication policy remain startup settings.
type BackingSettings struct {
	Backings       []BackingDefinition `json:"backings"`
	DefaultBacking string              `json:"defaultBacking"`
}

// BackingsFrom returns the editable portion of storage configuration.
func BackingsFrom(s StorageConfig) BackingSettings {
	out := BackingSettings{Backings: []BackingDefinition{}, DefaultBacking: s.DefaultBacking}
	if out.DefaultBacking == "" {
		out.DefaultBacking = DefaultBackingName
	}
	for _, b := range s.Backings {
		out.Backings = append(out.Backings, BackingDefinition{b.Name, b.Backend, b.Path})
	}
	return out
}

// Apply validates against the system backing and configured replication defaults.
func (s BackingSettings) Apply(base StorageConfig) (StorageConfig, error) {
	previous := base.Backings
	base.Backings = nil
	base.DefaultBacking = s.DefaultBacking
	for _, b := range s.Backings {
		next := BackingConfig{Name: b.Name, Backend: b.Backend, Path: b.Path}
		// The live backing form edits identity/placement, not restic secrets.
		// Retain startup S3 settings when the same filesystem is kept, without
		// putting credentials into the backing-definition response or overlay.
		for _, old := range previous {
			oldPath, _ := filepath.Abs(old.Path)
			newPath, _ := filepath.Abs(b.Path)
			if old.Name == b.Name && old.Backend == b.Backend && oldPath == newPath && old.ResticS3 != nil {
				cfg := *old.ResticS3
				next.ResticS3 = &cfg
				break
			}
		}
		base.Backings = append(base.Backings, next)
	}
	if err := (Config{Storage: base}).validateBackings(); err != nil {
		return base, err
	}
	return base, nil
}

// BackingsPath is the overlay path, or empty for an ephemeral server.
func (c Config) BackingsPath() string {
	if dir := c.SystemDir(); dir != "" {
		return filepath.Join(dir, "backings.json")
	}
	return ""
}

// LoadBackings restores UI changes before storage is opened. Like settings.json,
// the overlay is ignored when management is disabled.
func (c *Config) LoadBackings() error {
	if !c.Admin.Enabled || c.BackingsPath() == "" {
		return nil
	}
	data, err := os.ReadFile(c.BackingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading backing settings: %w", err)
	}
	s := BackingsFrom(c.Storage)
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("invalid backing settings: %w", err)
	}
	c.Storage, err = s.Apply(c.Storage)
	return err
}

// Save atomically persists backing settings; an empty path is memory only.
func (s BackingSettings) Save(path string) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}
