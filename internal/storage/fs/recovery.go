package fs

import (
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// recoveryBatch is the number of directory entries examined per read
// transaction during recovery.
const recoveryBatch = 256

// recoveryStats counts what the recovery pass removed.
type recoveryStats struct {
	dataFiles  int // unreferenced data files
	bucketDirs int // data directories of deleted buckets
	uploadDirs int // directories of finished/aborted uploads
	partFiles  int // unreferenced part files
	stray      int // unexpected files
	errors     int
}

// recover removes data and part files that the metadata does not reference.
// It runs in the background after Open and may run concurrently with normal
// operations: files of in-flight writes are registered in b.pending before
// they are renamed into place and stay there until their metadata
// transaction finished. For each candidate the pending set is consulted
// first and the metadata second, so a file is only removed if it was not
// pending at a point where its transaction would already be visible.
func (b *Backend) recover(ctx context.Context) {
	started := time.Now()
	var st recoveryStats
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
		"stray_entries", st.stray,
		"errors", st.errors,
		"duration", time.Since(started).Round(time.Millisecond),
	}
	if st.dataFiles+st.bucketDirs+st.uploadDirs+st.partFiles+st.stray+st.errors > 0 {
		b.log.Info("fs: recovery removed orphaned files", attrs...)
	} else {
		b.log.Debug("fs: recovery found nothing to clean", attrs...)
	}
}

// remove deletes path (recursively when all is set) and counts failures.
func (b *Backend) remove(path string, all bool, st *recoveryStats) bool {
	var err error
	if all {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		st.errors++
		b.log.Warn("fs: recovery: remove", "path", path, "err", err)
		return false
	}
	return true
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
			if !e.IsDir() {
				if b.remove(path, false, st) {
					st.stray++
				}
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
				if b.remove(path, true, st) {
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
			if !sub.IsDir() {
				if b.remove(subPath, false, st) {
					st.stray++
				}
				continue
			}
			err := readDirBatches(ctx, subPath, func(files []iofs.DirEntry) error {
				// Pending check strictly before the metadata check.
				candidates := make([]string, 0, len(files))
				for _, f := range files {
					if !b.pending.has(filepath.Join(subPath, f.Name())) {
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
					if b.remove(filepath.Join(subPath, name), true, st) {
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
			if !e.IsDir() {
				if b.remove(path, false, st) {
					st.stray++
				}
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
				if b.remove(path, true, st) {
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
			if !b.pending.has(filepath.Join(dir, f.Name())) {
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
				n, ok := parsePartFileName(name)
				referenced := false
				if ok && parts != nil {
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
			if b.remove(filepath.Join(dir, name), true, st) {
				st.partFiles++
			}
		}
		return nil
	})
}
