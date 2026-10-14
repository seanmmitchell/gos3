package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// resticBackingFile (below the system directory) persists the backing's
// restic S3 settings set through the admin API.
const resticBackingFile = "restic-backing.json"

// resticBackingDocument is the content of resticBackingFile. A nil S3 means
// local repositories, overriding any startup setting.
type resticBackingDocument struct {
	S3 *storage.ResticS3 `json:"s3"`
}

// ValidateResticS3 checks restic S3 settings (nil means local storage).
func ValidateResticS3(s *storage.ResticS3) error { return s.Validate() }

// copyResticS3 returns a copy of s (nil for nil).
func copyResticS3(s *storage.ResticS3) *storage.ResticS3 {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// loadResticBacking sets the backing's restic settings at Open: the
// persisted ones, or initial (the startup configuration) when none were
// persisted.
func (b *Backend) loadResticBacking(initial *storage.ResticS3) error {
	cfg := copyResticS3(initial)
	raw, err := os.ReadFile(filepath.Join(b.sysDir, resticBackingFile))
	if err == nil {
		var doc resticBackingDocument
		if err = json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("fs: invalid restic backing settings: %w", err)
		}
		cfg = doc.S3
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ValidateResticS3(cfg); err != nil {
		return err
	}
	if err := b.checkResticLocation(cfg); err != nil {
		return err
	}
	b.resticS3 = cfg
	return nil
}

// resticBacking returns a copy of the backing's restic S3 settings (nil
// for local repositories).
func (b *Backend) resticBacking() *storage.ResticS3 {
	b.resticConfigMu.RLock()
	defer b.resticConfigMu.RUnlock()
	return copyResticS3(b.resticS3)
}

// resticRepository returns the repository of the bucket with ID id under
// the restic settings cfg.
func (b *Backend) resticRepository(id string, cfg *storage.ResticS3) string {
	if cfg == nil {
		return filepath.Join(b.sysDir, "restic", id)
	}
	u, _ := url.Parse(cfg.Endpoint)
	u.Path = "/" + cfg.Bucket + "/" + strings.Trim(cfg.Prefix, "/")
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.Path += id
	return "s3:" + u.String()
}

// resticRecord is a recovery record with the ID of its bucket.
type resticRecord struct {
	id  string
	rec resticRepository
}

// initializedRepositories returns the recovery records of every
// initialized repository of the backing, including those of deleted
// buckets. A record whose repository was never initialized (preparation
// failed) holds no archive and is skipped.
func (b *Backend) initializedRepositories() ([]resticRecord, error) {
	files, err := filepath.Glob(filepath.Join(b.sysDir, "keys", "*.restic.json"))
	if err != nil {
		return nil, err
	}
	var out []resticRecord
	for _, path := range files {
		rec, _, err := readRecovery(path)
		if err != nil {
			return nil, err
		}
		if rec.Initialized {
			out = append(out, resticRecord{id: strings.TrimSuffix(filepath.Base(path), ".restic.json"), rec: rec})
		}
	}
	return out, nil
}

// checkResticLocation refuses restic settings cfg that would move an
// initialized repository. Recovery records include deleted buckets'
// archives: changing the backing's location must not strand any
// repository; use a placement move to a new backing instead.
func (b *Backend) checkResticLocation(cfg *storage.ResticS3) error {
	recs, err := b.initializedRepositories()
	if err != nil {
		return err
	}
	for _, r := range recs {
		// A filesystem restore can relocate the entire backing directory.
		// Local repository identity is relative to that backing, not its old
		// absolute path. Remote repositories remain fixed.
		repo := r.rec.Repository
		if cfg == nil && !strings.HasPrefix(repo, "s3:") && filepath.Base(repo) == r.id && filepath.Base(filepath.Dir(repo)) == "restic" {
			continue
		}
		if repo != b.resticRepository(r.id, cfg) {
			return errors.New("restic backing location is fixed after repository initialization; move buckets to another backing")
		}
	}
	return nil
}

// ResticBackingSettings returns the backing's restic S3 settings (nil for
// local repositories) and whether their location is fixed because a
// repository has been initialized (see storage.ResticBackingBackend).
func (b *Backend) ResticBackingSettings(ctx context.Context) (*storage.ResticS3, bool, error) {
	if err := b.begin(ctx); err != nil {
		return nil, false, err
	}
	recs, err := b.initializedRepositories()
	return b.resticBacking(), len(recs) > 0, err
}

// ConfigureResticBacking validates and persists the backing's restic S3
// settings cfg (nil for local repositories). With initialized repositories
// only credentials and region may change, and the new credentials must
// open every one of them; otherwise read access to the location is probed
// without creating anything.
func (b *Backend) ConfigureResticBacking(ctx context.Context, cfg *storage.ResticS3) error {
	if err := b.begin(ctx); err != nil {
		return err
	}
	cfg = copyResticS3(cfg)
	if err := ValidateResticS3(cfg); err != nil {
		return err
	}
	b.resticMu.Lock()
	defer b.resticMu.Unlock()
	if err := b.checkResticLocation(cfg); err != nil {
		return err
	}
	recs, err := b.initializedRepositories()
	if err != nil {
		return err
	}
	if cfg != nil {
		// Validate against every initialized repository, including archives
		// whose buckets were deleted. With no repositories, probe read access
		// without initializing or publishing any data.
		if len(recs) == 0 {
			probe := resticRepository{Repository: b.resticRepository(randomHex(16), cfg), Password: randomHex(32)}
			probe.withS3(cfg)
			if err := b.validateResticRepository(ctx, probe); err != nil {
				return err
			}
		}
		for _, r := range recs {
			rec := r.rec
			rec.withS3(cfg)
			rec.CacheDir = filepath.Join(b.sysDir, resticCacheDir)
			if err := b.validateResticRepository(ctx, rec); err != nil {
				return err
			}
		}
	}
	// Repositories prepared meanwhile are checked again under the lock that
	// keeps preparations out while the location changes.
	b.restic.locationMu.Lock()
	defer b.restic.locationMu.Unlock()
	if err := b.checkResticLocation(cfg); err != nil {
		return err
	}
	raw, err := json.Marshal(resticBackingDocument{S3: cfg})
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(filepath.Join(b.sysDir, resticBackingFile), raw, filePerm); err != nil {
		return err
	}
	b.resticConfigMu.Lock()
	b.resticS3 = cfg
	b.resticConfigMu.Unlock()
	b.restic.forgetPrepared()
	return nil
}
