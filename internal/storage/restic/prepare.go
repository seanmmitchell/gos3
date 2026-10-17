package restic

import (
	"context"
	"errors"
)

// Prepare makes sure the repository loc of the bucket with ID id exists,
// initializing a new one, and that the bucket's recovery record in store is
// current. It is PrepareWith without a bucket name: the record keeps the
// name it has.
func (rt *Runtime) Prepare(ctx context.Context, id string, loc Location, store RecoveryStore) error {
	return rt.PrepareWith(ctx, id, "", loc, store)
}

// PrepareWith makes sure the repository loc of the bucket with ID id
// exists, initializing a new one, and that the bucket's recovery record in
// store is current, recording bucket (when not "") as the bucket's name
// (Record.Bucket). A repository found ready is not checked again by this
// process (its record is only rewritten when called with another bucket
// name); repositories are prepared independently of each other.
//
// The record is written before the repository is created, so that an
// initialized repository never lacks one, and marked Initialized once the
// repository exists. Only restic's repository-not-found exit code permits
// initializing, and only while the record does not say the repository was
// initialized before: a wrong password, network or access errors, or a
// repository that went missing never start a new, empty archive.
func (rt *Runtime) PrepareWith(ctx context.Context, id, bucket string, loc Location, store RecoveryStore) error {
	st := rt.repo(loc.Repository)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.prepared && (bucket == "" || bucket == st.bucket) {
		return nil
	}
	saved, err := store.Load(ctx, id)
	if err != nil {
		return err
	}
	// Retain recovery information independently of the bucket's metadata:
	// deleting a bucket must not make its retained archive unrecoverable.
	rec := Record{Initialized: saved.Initialized || st.prepared, Repository: loc.Repository, Bucket: saved.Bucket}
	if bucket != "" {
		rec.Bucket = bucket
	}
	if err := store.Store(ctx, id, rec); err != nil {
		return err
	}
	if st.prepared {
		// Only the bucket name changed.
		st.bucket = rec.Bucket
		return nil
	}
	cmd, err := rt.Command(ctx, loc, "cat", "config")
	if err != nil {
		return err
	}
	var diag stderrTail
	cmd.Stderr = &diag
	if err := cmd.Run(); err != nil {
		if exitCode(err) != exitNoRepository {
			return rt.failure(ctx, "open repository", err, &diag, loc)
		}
		if rec.Initialized {
			return rt.failure(ctx, "open repository", errors.New("initialized repository is missing; restore the repository from backup"), nil, loc)
		}
		if cmd, err = rt.Command(ctx, loc, "init"); err != nil {
			return err
		}
		diag = stderrTail{}
		cmd.Stderr = &diag
		if err := cmd.Run(); err != nil {
			return rt.failure(ctx, "initialize repository", err, &diag, loc)
		}
	}
	rec.Initialized = true
	if err := store.Store(ctx, id, rec); err != nil {
		return err
	}
	st.prepared, st.bucket = true, rec.Bucket
	return nil
}

// Prepared reports whether this process already prepared the repository
// loc (see Prepare).
func (rt *Runtime) Prepared(loc Location) bool {
	st := rt.repo(loc.Repository)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.prepared
}

// Validate checks, without initializing or recording anything, that the
// repository loc of the bucket with ID id can be opened with its password,
// or does not exist yet while the bucket's record in store does not say it
// was initialized. A repository this process prepared is not checked again.
func (rt *Runtime) Validate(ctx context.Context, id string, loc Location, store RecoveryStore) error {
	if rt.Prepared(loc) {
		return nil
	}
	saved, err := store.Load(ctx, id)
	if err != nil {
		return err
	}
	cmd, err := rt.Command(ctx, loc, "cat", "config")
	if err != nil {
		return err
	}
	var diag stderrTail
	cmd.Stderr = &diag
	err = cmd.Run()
	if err == nil || (exitCode(err) == exitNoRepository && !saved.Initialized) {
		return nil
	}
	return rt.failure(ctx, "open repository", err, &diag, loc)
}
