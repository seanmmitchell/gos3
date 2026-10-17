package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// BackingDefinition is an additional backing managed through the admin API.
type BackingDefinition struct {
	Name    string `json:"name"`
	Backend string `json:"backend"` // "fs", "memory" or "s3"
	// Path is the directory of an fs backing ("" for memory and s3).
	Path string `json:"path"`
	// S3 is the remote service of an s3 backing (nil otherwise).
	S3 *S3Definition `json:"s3,omitempty"`
}

// S3Definition is the JSON form of S3Config in the admin API and in
// backings.json. The secret and encryption keys are write-only: Public
// blanks them and sets the *Configured flags instead, and a blank value in
// a definition passed to BackingSettings.Apply keeps the stored one.
type S3Definition struct {
	Endpoint   string `json:"endpoint"`
	Region     string `json:"region"`
	HomeBucket string `json:"homeBucket"`
	HomePrefix string `json:"homePrefix"`
	// DefaultKind is "wrapped" or "native" ("" = the default for the home,
	// filled in by Apply: wrapped with a home, native without).
	DefaultKind string `json:"defaultKind"`
	Addressing  string `json:"addressing"`
	PartSize    int64  `json:"partSize,omitempty"`
	AccessKey   string `json:"accessKey"`
	// SecretKey and EncryptionKey are write-only (see S3Definition).
	SecretKey     string `json:"secretKey,omitempty"`
	EncryptionKey string `json:"encryptionKey,omitempty"`
	// SecretKeyConfigured and EncryptionKeyConfigured report whether the
	// key is set (read-only).
	SecretKeyConfigured     bool `json:"secretKeyConfigured"`
	EncryptionKeyConfigured bool `json:"encryptionKeyConfigured"`
	// The *Source fields are "env" when the value comes from the
	// environment (BackingEnv), "" otherwise (read-only). Such values are
	// not written to backings.json.
	AccessKeySource     string `json:"accessKeySource,omitempty"`
	SecretKeySource     string `json:"secretKeySource,omitempty"`
	EncryptionKeySource string `json:"encryptionKeySource,omitempty"`
	// EnvFields lists the S3Config YAML keys (e.g. "endpoint",
	// "secret_key") of every value taken from the environment, the
	// credentials included (read-only). Such values are not written to
	// backings.json.
	EnvFields []string `json:"envFields,omitempty"`
	// LegacyBucket, LegacyPrefix and LegacyGateway are the deprecated keys
	// bucket, prefix and gateway (see S3Config), accepted on input only
	// (backings.json of earlier versions, API clients): Apply maps them to
	// homeBucket, homePrefix and defaultKind native, and they are never
	// written.
	LegacyBucket  string `json:"bucket,omitempty"`
	LegacyPrefix  string `json:"prefix,omitempty"`
	LegacyGateway bool   `json:"gateway,omitempty"`
}

// SourceEnv is the S3Definition *Source value of settings taken from the
// environment.
const SourceEnv = "env"

// BackingSettings overrides backing definitions and the new-bucket default.
// The default system backing and replication policy remain startup settings.
type BackingSettings struct {
	Backings       []BackingDefinition `json:"backings"`
	DefaultBacking string              `json:"defaultBacking"`
}

// lookupEnv reads the environment for the BackingEnv fallback of Apply
// (replaced by tests).
var lookupEnv = os.LookupEnv

// Config returns the static form of d, secrets included.
func (d BackingDefinition) Config() BackingConfig {
	b := BackingConfig{Name: d.Name, Backend: d.Backend, Path: d.Path}
	if s := d.S3; s != nil {
		b.S3 = &S3Config{
			Endpoint:      s.Endpoint,
			Region:        s.Region,
			HomeBucket:    s.HomeBucket,
			HomePrefix:    s.HomePrefix,
			Addressing:    s.Addressing,
			PartSize:      s.PartSize,
			AccessKey:     s.AccessKey,
			SecretKey:     s.SecretKey,
			EncryptionKey: s.EncryptionKey,
			DefaultKind:   s.DefaultKind,
			LegacyBucket:  s.LegacyBucket,
			LegacyPrefix:  s.LegacyPrefix,
			LegacyGateway: s.LegacyGateway,
		}
		for _, f := range s.EnvFields {
			if slices.Contains(s3EnvFields, f) {
				b.S3.setEnv(f, true)
			}
		}
		b.S3.setEnv("access_key", b.S3.isEnv("access_key") || s.AccessKeySource == SourceEnv)
		b.S3.setEnv("secret_key", b.S3.isEnv("secret_key") || s.SecretKeySource == SourceEnv)
		b.S3.setEnv("encryption_key", b.S3.isEnv("encryption_key") || s.EncryptionKeySource == SourceEnv)
	}
	return b
}

// DefinitionOf returns the definition of the static backing b, secrets
// included, with the *Configured flags and *Source fields set.
func DefinitionOf(b BackingConfig) BackingDefinition {
	d := BackingDefinition{Name: b.Name, Backend: b.Backend, Path: b.Path}
	if s := b.S3; s != nil {
		d.S3 = &S3Definition{
			Endpoint:                s.Endpoint,
			Region:                  s.Region,
			HomeBucket:              s.HomeBucket,
			HomePrefix:              s.HomePrefix,
			DefaultKind:             s.DefaultKind,
			Addressing:              s.Addressing,
			PartSize:                s.PartSize,
			AccessKey:               s.AccessKey,
			SecretKey:               s.SecretKey,
			EncryptionKey:           s.EncryptionKey,
			SecretKeyConfigured:     s.SecretKey != "",
			EncryptionKeyConfigured: s.EncryptionKey != "",
			AccessKeySource:         source(s.isEnv("access_key")),
			SecretKeySource:         source(s.isEnv("secret_key")),
			EncryptionKeySource:     source(s.isEnv("encryption_key")),
			EnvFields:               s.EnvFields(),
			LegacyBucket:            s.LegacyBucket,
			LegacyPrefix:            s.LegacyPrefix,
			LegacyGateway:           s.LegacyGateway,
		}
	}
	return d
}

func source(fromEnv bool) string {
	if fromEnv {
		return SourceEnv
	}
	return ""
}

// Identity names the data a backing holds, so that two definitions with
// the same identity open the same data: "fs:<absolute path>" for fs,
// "memory:<name>" for memory and, for s3, "s3:home:<home bucket,
// lower-cased>/<home prefix>" with a home or "s3:local:<name>" for a
// backing whose records live in the system directory. The default kind and
// the connection settings (endpoint, region, credentials, encryption key,
// addressing, part size) are not part of it: changing them reconfigures
// the backing in place. Deprecated keys count as their replacements.
func (d BackingDefinition) Identity() string {
	switch d.Backend {
	case BackendMemory:
		return BackendMemory + ":" + d.Name
	case BackendS3:
		var s S3Config
		if d.S3 != nil {
			s = *d.Config().S3
		}
		s.normalize()
		return BackendS3 + ":" + s.identity(strings.TrimSpace(d.Name))
	}
	p, err := filepath.Abs(d.Path)
	if err != nil {
		p = d.Path
	}
	return d.Backend + ":" + p
}

// Equal reports whether d and o have the same settings, secrets included
// (the read-only *Configured, *Source and EnvFields fields are ignored;
// deprecated keys count as their replacements).
func (d BackingDefinition) Equal(o BackingDefinition) bool {
	if d.Name != o.Name || d.Backend != o.Backend || d.Path != o.Path || (d.S3 == nil) != (o.S3 == nil) {
		return false
	}
	if d.S3 == nil {
		return true
	}
	a, b := *d.Config().S3, *o.Config().S3
	a.mapLegacy()
	b.mapLegacy()
	a.fromEnv, b.fromEnv = 0, 0
	a.legacy, b.legacy = 0, 0
	return a == b
}

// BackingsFrom returns the editable portion of storage configuration
// (secrets included: see Public).
func BackingsFrom(s StorageConfig) BackingSettings {
	out := BackingSettings{Backings: []BackingDefinition{}, DefaultBacking: s.DefaultBacking}
	if out.DefaultBacking == "" {
		out.DefaultBacking = DefaultBackingName
	}
	for _, b := range s.Backings {
		out.Backings = append(out.Backings, DefinitionOf(b))
	}
	return out
}

// Public returns a copy of s for display: the secret and encryption keys of
// s3 backings are blanked, with their *Configured flags set.
func (s BackingSettings) Public() BackingSettings {
	out := BackingSettings{Backings: make([]BackingDefinition, len(s.Backings)), DefaultBacking: s.DefaultBacking}
	for i, d := range s.Backings {
		if d.S3 != nil {
			cp := *d.S3
			cp.SecretKeyConfigured = cp.SecretKey != "" || cp.SecretKeyConfigured
			cp.EncryptionKeyConfigured = cp.EncryptionKey != "" || cp.EncryptionKeyConfigured
			cp.SecretKey, cp.EncryptionKey = "", ""
			d.S3 = &cp
		}
		out.Backings[i] = d
	}
	return out
}

// Apply returns base with its additional backings and default backing
// replaced by s, validated against the system backing and the configured
// replication defaults.
//
// For an s3 backing, a blank secret or encryption key keeps the value of
// the backing of the same name stored in base (an s3 backing too), and
// fields still empty are then filled from the environment (BackingEnv).
// The *Source and EnvFields fields of s are ignored: a value is from the
// environment when it was taken from there, or when it equals the
// environment-sourced value stored in base (so that a definition read
// from the API and sent back unchanged keeps following the environment).
// Likewise a default kind equal to the stored one, when that is the
// default for the stored home, is derived again from the (possibly new)
// home. Deprecated keys (bucket, prefix, gateway) are mapped to their
// replacements; one that conflicts with its replacement is an error.
func (s BackingSettings) Apply(base StorageConfig) (StorageConfig, error) {
	previous := map[string]*S3Config{}
	for _, b := range base.Backings {
		if b.Backend == BackendS3 && b.S3 != nil {
			previous[b.Name] = b.S3
		}
	}
	base.Backings = nil
	base.DefaultBacking = s.DefaultBacking
	for _, d := range s.Backings {
		next := d.Config()
		if c := next.S3; c != nil {
			c.fromEnv = 0
			c.normalize()
			if old := previous[strings.TrimSpace(d.Name)]; old != nil && strings.EqualFold(strings.TrimSpace(d.Backend), BackendS3) {
				// Write-only secrets: blank keeps the stored value.
				if c.SecretKey == "" {
					c.SecretKey = old.SecretKey
				}
				if c.EncryptionKey == "" {
					c.EncryptionKey = old.EncryptionKey
				}
				for _, f := range s3EnvFields {
					if old.isEnv(f) && c.fieldValue(f) == old.fieldValue(f) {
						c.setEnv(f, true)
					}
				}
				// The default kind follows the home unless it was chosen.
				if !c.isEnv("default_kind") && c.DefaultKind == old.DefaultKind && old.DefaultKind == defaultKindFor(old.HomeBucket) {
					c.DefaultKind = ""
				}
			}
		}
		base.Backings = append(base.Backings, next)
	}
	if err := base.applyBackingEnv(lookupEnv); err != nil {
		return base, err
	}
	cfg := Config{Storage: base}
	cfg.normalizeS3()
	base = cfg.Storage
	if err := cfg.validateBackings(); err != nil {
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
	// Decode into a fresh value: decoding into BackingsFrom's slice would
	// merge the saved definitions into the static ones element by element.
	s := BackingSettings{DefaultBacking: c.Storage.DefaultBacking}
	if s.DefaultBacking == "" {
		s.DefaultBacking = DefaultBackingName
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("invalid backing settings: %w", err)
	}
	storage, err := s.Apply(c.Storage)
	if err != nil {
		return fmt.Errorf("backing settings %s: %w", c.BackingsPath(), err)
	}
	c.Storage = storage
	return nil
}

// Save atomically persists backing settings (mode 0600: s3 secrets are
// included). Values taken from the environment (EnvFields, *Source) are
// left out, so that the environment keeps supplying them after a restart,
// and so is a default kind that is the default for the backing's home (it
// is derived again on load). Only the current keys are written: deprecated
// ones are mapped to their replacements first. An empty path is memory
// only.
func (s BackingSettings) Save(path string) error {
	if path == "" {
		return nil
	}
	out := BackingSettings{Backings: make([]BackingDefinition, len(s.Backings)), DefaultBacking: s.DefaultBacking}
	for i, d := range s.Backings {
		if d.S3 != nil {
			cfg := d.Config()
			c := cfg.S3
			c.mapLegacy()
			c.LegacyBucket, c.LegacyPrefix, c.LegacyGateway = "", "", false
			if c.DefaultKind == defaultKindFor(c.HomeBucket) {
				c.DefaultKind = ""
			}
			for _, f := range c.EnvFields() {
				c.clearField(f)
			}
			c.fromEnv, c.legacy = 0, 0
			cp := *DefinitionOf(cfg).S3
			cp.SecretKeyConfigured, cp.EncryptionKeyConfigured = false, false
			d.S3 = &cp
		}
		out.Backings[i] = d
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}
