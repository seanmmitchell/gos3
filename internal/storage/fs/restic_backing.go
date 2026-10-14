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

const resticBackingFile = "restic-backing.json"

type resticBackingDocument struct {
	S3 *storage.ResticS3 `json:"s3"`
}

func ValidateResticS3(s *storage.ResticS3) error { return s.Validate() }

func copyResticS3(s *storage.ResticS3) *storage.ResticS3 {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

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

func (b *Backend) resticBacking() *storage.ResticS3 {
	b.resticConfigMu.RLock()
	defer b.resticConfigMu.RUnlock()
	return copyResticS3(b.resticS3)
}

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

func (b *Backend) resticRecoveryFiles() ([]string, error) {
	return filepath.Glob(filepath.Join(b.sysDir, "keys", "*.restic.json"))
}

// Recovery records include deleted buckets' archives. Changing the backing's
// location must not strand any repository; use a placement move to a new backing.
func (b *Backend) checkResticLocation(cfg *storage.ResticS3) error {
	files, err := b.resticRecoveryFiles()
	if err != nil {
		return err
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var rec resticRepository
		if err = json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		id := strings.TrimSuffix(filepath.Base(path), ".restic.json")
		// A filesystem restore can relocate the entire backing directory.
		// Local repository identity is relative to that backing, not its old
		// absolute path. Remote repositories remain fixed.
		if cfg == nil && !strings.HasPrefix(rec.Repository, "s3:") && filepath.Base(rec.Repository) == id && filepath.Base(filepath.Dir(rec.Repository)) == "restic" {
			continue
		}
		if rec.Repository != b.resticRepository(id, cfg) {
			return errors.New("restic backing location is fixed after repository initialization; move buckets to another backing")
		}
	}
	return nil
}

func (b *Backend) ResticBackingSettings(ctx context.Context) (*storage.ResticS3, bool, error) {
	if err := b.begin(ctx); err != nil {
		return nil, false, err
	}
	files, err := b.resticRecoveryFiles()
	return b.resticBacking(), len(files) > 0, err
}

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
	files, err := b.resticRecoveryFiles()
	if err != nil {
		return err
	}
	if cfg != nil {
		// Validate against every initialized repository, including archives
		// whose buckets were deleted. With no repositories, probe read access
		// without initializing or publishing any data.
		if len(files) == 0 {
			probe := resticRepository{Repository: b.resticRepository(randomHex(16), cfg), Password: randomHex(32)}
			probe.withS3(cfg)
			if err := validateResticRepository(ctx, probe); err != nil {
				return err
			}
		}
		for _, path := range files {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var rec resticRepository
			if err = json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			rec.withS3(cfg)
			if err := validateResticRepository(ctx, rec); err != nil {
				return err
			}
		}
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
	return nil
}
