package fs

import (
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// recoveryBatch is the number of directory entries examined per read
// transaction during recovery.
const recoveryBatch = 256

// orphanRunLayout formats the start time of a recovery pass in the names
// of quarantine directories (orphans/<time>-<random>).
const orphanRunLayout = "20060102T150405Z"

// recoveryStats counts what the recovery pass moved to the quarantine.
type recoveryStats struct {
	dataFiles  int // unreferenced data files
	bucketDirs int // data directories of deleted buckets
	uploadDirs int // directories of finished/aborted uploads
	partFiles  int // unreferenced part files
	ignored    int // entries with names gos3 does not generate (left alone)
	purged     int // expired quarantine directories deleted
	errors     int

	// runDir is the quarantine directory of this pass, created on first use.
	runDir string
}

// recover moves data and part files that the metadata does not reference
// to the quarantine and deletes expired quarantine directories. It runs in
// the background after Open and may run concurrently with normal
// operations: files of in-flight writes are registered in b.pending before
// they are renamed into place and stay there until their metadata
// transaction finished. For each candidate the pending set is consulted
// first and the metadata second, so a file is only moved if it was not
// pending at a point where its transaction would already be visible.
func (b *Backend) recover(ctx context.Context) {
	started := time.Now()
	var st recoveryStats
	b.purgeOrphans(ctx, started, &st)
	b.recoverData(ctx, &st)
	b.recoverUploads(ctx, &st)
	if ctx.Err() != nil {
		b.log.Debug("fs: recovery interrupted by close")
		return
	}
	attrs := []any{
		"orphan_data_files", st.dataFiles,
		"deleted_bucket_dirs", st.bucketDirs,
		"stale_upload_dirs", st.uploadDirs,
		"orphan_part_files", st.partFiles,
		"unrecognized_entries", st.ignored,
		"purged_quarantine_dirs", st.purged,
		"errors", st.errors,
		"duration", time.Since(started).Round(time.Millisecond),
	}
	switch {
	case st.dataFiles+st.bucketDirs+st.uploadDirs+st.partFiles > 0:
		attrs = append(attrs, "quarantine", st.runDir, "retention", b.retention)
		b.log.Warn("fs: recovery moved unreferenced files to the quarantine; "+
			"they are deleted after the retention period", attrs...)
	case st.purged+st.errors > 0:
		b.log.Info("fs: recovery finished", attrs...)
	default:
		b.log.Debug("fs: recovery found nothing to clean", attrs...)
	}
}

// quarantine moves path (a file or directory below the system directory)
// into the quarantine directory of this pass, keeping its relative path. It
// reports whether path was moved.
func (b *Backend) quarantine(path string, st *recoveryStats) bool {
	rel, err := filepath.Rel(b.sysDir, path)
	if err != nil || !filepath.IsLocal(rel) {
		st.errors++
		b.log.Warn("fs: recovery: path outside the system directory", "path", path)
		return false
	}
	if st.runDir == "" {
		st.runDir = filepath.Join(b.orphanDir, time.Now().UTC().Format(orphanRunLayout)+"-"+randomHex(4))
	}
	dst := filepath.Join(st.runDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		st.errors++
		b.log.Warn("fs: recovery: create quarantine directory", "path", filepath.Dir(dst), "err", err)
		return false
	}
	if err := os.Rename(path, dst); err != nil {
		if !errors.Is(err, iofs.ErrNotExist) {
			// Never fall back to deleting: leave it for the next start.
			st.errors++
			b.log.Warn("fs: recovery: move to quarantine", "path", path, "err", err)
		}
		return false
	}
	return true
}

// ignore counts and logs an entry whose name gos3 never generates.
func (b *Backend) ignore(path string, st *recoveryStats) {
	st.ignored++
	b.log.Debug("fs: recovery: leaving unrecognized entry alone", "path", path)
}

// purgeOrphans deletes quarantine directories older than the retention.
// Only entries named like quarantine directories are touched.
func (b *Backend) purgeOrphans(ctx context.Context, now time.Time, st *recoveryStats) {
	entries, err := os.ReadDir(b.orphanDir)
	if err != nil {
		if !errors.Is(err, iofs.ErrNotExist) {
			st.errors++
			b.log.Warn("fs: recovery: read quarantine directory", "err", err)
		}
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		stamp, _, ok := strings.Cut(e.Name(), "-")
		if !ok || !e.IsDir() {
			continue
		}
		t, err := time.Parse(orphanRunLayout, stamp)
		if err != nil || now.Sub(t) <= b.retention {
			continue
		}
		if err := os.RemoveAll(filepath.Join(b.orphanDir, e.Name())); err != nil {
			st.errors++
			b.log.Warn("fs: recovery: purge quarantine", "dir", e.Name(), "err", err)
			continue
		}
		st.purged++
	}
}

// readDirBatches calls fn with batches of entry names of dir.
func readDirBatches(ctx context.Context, dir string, fn func(entries []iofs.DirEntry) error) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := d.ReadDir(recoveryBatch)
		if len(entries) > 0 {
			if ferr := fn(entries); ferr != nil {
				return ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (b *Backend) recoverData(ctx context.Context, st *recoveryStats) {
	err := readDirBatches(ctx, b.dataDir, func(entries []iofs.DirEntry) error {
		for _, e := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			path := filepath.Join(b.dataDir, e.Name())
			if !e.IsDir() || !validBucketID(e.Name()) {
				b.ignore(path, st)
				continue
			}
			bucketID := e.Name()
			var exists bool
			if err := b.view(func(tx *bbolt.Tx) error {
				exists = tx.Bucket(bucketDataName(bucketID)) != nil
				return nil
			}); err != nil {
				return err
			}
			if !exists {
				if b.quarantine(path, st) {
					st.bucketDirs++
				}
				continue
			}
			if err := b.recoverBucketData(ctx, bucketID, path, st); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrClosed) {
		st.errors++
		b.log.Warn("fs: recovery: scan data directory", "err", err)
	}
}

func (b *Backend) recoverBucketData(ctx context.Context, bucketID, dir string, st *recoveryStats) error {
	return readDirBatches(ctx, dir, func(subs []iofs.DirEntry) error {
		for _, sub := range subs {
			subPath := filepath.Join(dir, sub.Name())
			if !sub.IsDir() || len(sub.Name()) != 2 || !validHex(sub.Name()) {
				b.ignore(subPath, st)
				continue
			}
			err := readDirBatches(ctx, subPath, func(files []iofs.DirEntry) error {
				// Pending check strictly before the metadata check.
				candidates := make([]string, 0, len(files))
				for _, f := range files {
					p := filepath.Join(subPath, f.Name())
					if !validDataID(f.Name()) || f.Name()[:2] != sub.Name() {
						b.ignore(p, st)
						continue
					}
					if !b.pending.has(p) {
						candidates = append(candidates, f.Name())
					}
				}
				var orphans []string
				if err := b.view(func(tx *bbolt.Tx) error {
					bb := tx.Bucket(bucketDataName(bucketID))
					var dat *bbolt.Bucket
					if bb != nil {
						dat = bb.Bucket(subData)
					}
					for _, name := range candidates {
						if dat == nil || dat.Get([]byte(name)) == nil {
							orphans = append(orphans, name)
						}
					}
					return nil
				}); err != nil {
					return err
				}
				for _, name := range orphans {
					if b.quarantine(filepath.Join(subPath, name), st) {
						st.dataFiles++
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *Backend) recoverUploads(ctx context.Context, st *recoveryStats) {
	err := readDirBatches(ctx, b.uploadDir, func(entries []iofs.DirEntry) error {
		for _, e := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			uploadID := e.Name()
			path := filepath.Join(b.uploadDir, uploadID)
			if !e.IsDir() || !validUploadID(uploadID) {
				b.ignore(path, st)
				continue
			}
			var bucketID string
			if err := b.view(func(tx *bbolt.Tx) error {
				if v := tx.Bucket(topUploads).Get([]byte(uploadID)); v != nil {
					bucketID = string(v)
				}
				return nil
			}); err != nil {
				return err
			}
			if bucketID == "" {
				if b.quarantine(path, st) {
					st.uploadDirs++
				}
				continue
			}
			if err := b.recoverUploadParts(ctx, bucketID, uploadID, path, st); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrClosed) {
		st.errors++
		b.log.Warn("fs: recovery: scan uploads directory", "err", err)
	}
}

func (b *Backend) recoverUploadParts(ctx context.Context, bucketID, uploadID, dir string, st *recoveryStats) error {
	return readDirBatches(ctx, dir, func(files []iofs.DirEntry) error {
		candidates := make([]string, 0, len(files))
		for _, f := range files {
			p := filepath.Join(dir, f.Name())
			if _, ok := parsePartFileName(f.Name()); !ok {
				b.ignore(p, st)
				continue
			}
			if !b.pending.has(p) {
				candidates = append(candidates, f.Name())
			}
		}
		var orphans []string
		if err := b.view(func(tx *bbolt.Tx) error {
			var parts *bbolt.Bucket
			if bb := tx.Bucket(bucketDataName(bucketID)); bb != nil {
				parts = bb.Bucket(subParts)
			}
			for _, name := range candidates {
				n, _ := parsePartFileName(name)
				referenced := false
				if parts != nil {
					if v := parts.Get(partKey(uploadID, n)); v != nil {
						var rec partRecord
						referenced = decodeRecord(v, &rec) == nil && rec.File == name
					}
				}
				if !referenced {
					orphans = append(orphans, name)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, name := range orphans {
			if b.quarantine(filepath.Join(dir, name), st) {
				st.partFiles++
			}
		}
		return nil
	})
}
