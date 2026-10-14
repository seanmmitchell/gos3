package fs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// resticWriter archives one object version by streaming it into
// `restic backup --stdin` while it is written, so that committing does not
// wait for the whole upload to the repository. restic starts with the
// first byte: empty objects never reach restic and have no data file.
//
// restic saves the snapshot only when its input ends, which only finish
// does after check passed: an aborted or refused write leaves no snapshot
// (but possibly unreferenced repository data, which `restic prune`
// removes). Not safe for concurrent use; the object writers serialize it.
type resticWriter struct {
	b *Backend
	// ctx is the writer's context; restic runs on cmdCtx, derived from it
	// and canceled to stop restic before its input ends.
	ctx, cmdCtx      context.Context
	cancel           context.CancelFunc
	id, bucket, key  string
	cfg              resticRepository
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	out              snapshotOutput
	diag             stderrTail
	size             int64
	err              error // sticky
	started, stopped bool
}

// newResticWriter prepares the repository of the bucket with ID id (and
// the bucket key its password derives from, which a replica receiving its
// first copy does not have yet) and returns a writer archiving bucket/key
// into it.
func (b *Backend) newResticWriter(ctx context.Context, id, bucket, key string) (*resticWriter, error) {
	if _, err := b.bucketKey(id, true); err != nil {
		return nil, err
	}
	if err := b.prepareRestic(ctx, id); err != nil {
		return nil, err
	}
	cfg, err := b.resticSettings(id)
	if err != nil {
		return nil, err
	}
	w := &resticWriter{b: b, ctx: ctx, id: id, bucket: bucket, key: key, cfg: cfg}
	w.cmdCtx, w.cancel = context.WithCancel(ctx)
	return w, nil
}

// start runs restic.
func (w *resticWriter) start() error {
	w.started = true
	cmd, err := resticCommandFor(w.cmdCtx, w.cfg, "backup", "--stdin", "--stdin-filename", "object", "--json", "--host", "gos3",
		"--tag", "gos3-object", "--tag", "gos3-bucket:"+base64.RawURLEncoding.EncodeToString([]byte(w.bucket)),
		"--tag", "gos3-key:"+base64.RawURLEncoding.EncodeToString([]byte(w.key)))
	if err != nil {
		return err
	}
	if w.stdin, err = cmd.StdinPipe(); err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = &w.out, &w.diag
	if err := cmd.Start(); err != nil {
		return w.b.resticFailure(w.ctx, "archive object", err, nil, w.cfg)
	}
	w.cmd = cmd
	return nil
}

func (w *resticWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if !w.started {
		if err := w.start(); err != nil {
			w.err = err
			w.abort()
			return 0, err
		}
	}
	n, err := w.stdin.Write(p)
	w.size += int64(n)
	if err != nil {
		// restic exited (or is exiting): report why it did.
		werr := w.wait()
		if werr == nil {
			werr = err
		}
		w.err = w.b.resticFailure(w.ctx, "archive object", werr, &w.diag, w.cfg)
		return n, w.err
	}
	return n, nil
}

// wait waits for restic to exit (once) and returns its result.
func (w *resticWriter) wait() error {
	if w.stopped || w.cmd == nil {
		return nil
	}
	w.stopped = true
	return w.cmd.Wait()
}

// abort stops restic before its input ends, so no snapshot is saved.
func (w *resticWriter) abort() {
	w.cancel()
	_ = w.wait()
}

// finish runs check (when not nil; e.g. the write's preconditions), then
// ends restic's input and waits for the snapshot. It returns the data ID
// and final path of the version's snapshot reference as finishData does
// ("" for an empty object, which has no snapshot). On any failure, check's
// included, nothing is archived.
func (w *resticWriter) finish(check func() error) (dataID, path string, err error) {
	defer w.abort()
	if w.err != nil {
		return "", "", w.err
	}
	if check != nil {
		if err := check(); err != nil {
			return "", "", err
		}
	}
	if !w.started {
		return "", "", nil
	}
	if err := w.stdin.Close(); err != nil {
		return "", "", w.b.resticFailure(w.ctx, "archive object", err, &w.diag, w.cfg)
	}
	if err := w.wait(); err != nil {
		return "", "", w.b.resticFailure(w.ctx, "archive object", err, &w.diag, w.cfg)
	}
	if err := w.out.consume(); err != nil {
		return "", "", w.b.resticFailure(w.ctx, "archive object", err, &w.diag, w.cfg)
	}
	if !validSnapshot(w.out.snapshot) {
		return "", "", w.b.resticFailure(w.ctx, "archive object", errors.New("backup returned no valid snapshot ID"), &w.diag, w.cfg)
	}
	// The local data file is an immutable pointer. No plaintext is kept.
	ref := w.b.newTemp()
	if _, err := ref.Write([]byte(w.out.snapshot)); err != nil {
		ref.discard()
		return "", "", err
	}
	return w.b.finishData(ref, w.id)
}

// checkWriteConditions evaluates the preconditions of opts for a new
// version of bucket/key before it is archived, so that a refused write
// costs no snapshot. commitObject evaluates them again atomically; a write
// racing in between can still leave an unreferenced snapshot.
func (b *Backend) checkWriteConditions(bucket, bucketID, key string, opts storage.CommitOptions) error {
	if !opts.IfNoneMatch && opts.IfMatch == "" {
		return nil
	}
	return b.view(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucketID(tx, bucket, bucketID)
		if err != nil {
			return err
		}
		return checkPreconditions(bb, key, opts)
	})
}

// snapshotOutput consumes restic's JSON progress incrementally: a long
// upload must not fill a bounded buffer and lose the final summary that
// holds the snapshot ID.
type snapshotOutput struct {
	pending  []byte
	snapshot string
}

func (w *snapshotOutput) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		line, rest, complete := bytes.Cut(p, []byte{'\n'})
		if len(w.pending)+len(line) > 1<<20 {
			return 0, errors.New("restic: oversized backup progress record")
		}
		w.pending = append(w.pending, line...)
		if !complete {
			break
		}
		if err := w.consume(); err != nil {
			return 0, err
		}
		p = rest
	}
	return n, nil
}

// consume decodes the pending JSON record, keeping the snapshot ID of a
// summary.
func (w *snapshotOutput) consume() error {
	if len(bytes.TrimSpace(w.pending)) == 0 {
		w.pending = w.pending[:0]
		return nil
	}
	var item struct {
		MessageType string `json:"message_type"`
		SnapshotID  string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(w.pending, &item); err != nil {
		return fmt.Errorf("restic: decode backup result: %w", err)
	}
	if item.MessageType == "summary" {
		w.snapshot = item.SnapshotID
	}
	w.pending = w.pending[:0]
	return nil
}
