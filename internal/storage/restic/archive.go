package restic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Archiver archives one object version by streaming it into
// `restic backup --stdin` while it is written, so that committing does not
// wait for the whole upload to the repository. restic starts with the
// first byte: empty objects never reach restic and have no snapshot.
//
// restic saves the snapshot only when its input ends, which only Finish
// does after its check passed: an aborted or refused write leaves no
// snapshot (but possibly unreferenced repository data, which
// `restic prune` removes). An Archiver is not safe for concurrent use.
type Archiver struct {
	rt *Runtime
	// ctx is the writer's context; restic runs on cmdCtx, derived from it
	// and canceled to stop restic before its input ends.
	ctx, cmdCtx      context.Context
	cancel           context.CancelFunc
	loc              Location
	tags             []string
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	out              snapshotOutput
	diag             stderrTail
	size             int64
	err              error // sticky
	started, stopped bool
}

// NewArchiver returns an Archiver of a version of bucket/key into the
// repository loc, which the caller has prepared (see Prepare). The
// snapshot is tagged "gos3-object", "gos3-bucket:<bucket>" and
// "gos3-key:<key>" (names in unpadded base64url), with host "gos3" and
// file name "object". Nothing runs until the first byte is written; the
// caller must Finish or Abort it.
func (rt *Runtime) NewArchiver(ctx context.Context, loc Location, bucket, key string) *Archiver {
	a := &Archiver{rt: rt, ctx: ctx, loc: loc, tags: []string{
		"gos3-object",
		"gos3-bucket:" + base64.RawURLEncoding.EncodeToString([]byte(bucket)),
		"gos3-key:" + base64.RawURLEncoding.EncodeToString([]byte(key)),
	}}
	a.cmdCtx, a.cancel = context.WithCancel(ctx)
	return a
}

// start runs restic.
func (a *Archiver) start() error {
	a.started = true
	args := []string{"backup", "--stdin", "--stdin-filename", "object", "--json", "--host", "gos3"}
	for _, t := range a.tags {
		args = append(args, "--tag", t)
	}
	cmd, err := a.rt.Command(a.cmdCtx, a.loc, args...)
	if err != nil {
		return err
	}
	if a.stdin, err = cmd.StdinPipe(); err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = &a.out, &a.diag
	if err := cmd.Start(); err != nil {
		return a.rt.failure(a.ctx, "archive object", err, nil, a.loc)
	}
	a.cmd = cmd
	return nil
}

// Write streams p to restic, starting it with the first byte.
func (a *Archiver) Write(p []byte) (int, error) {
	if a.err != nil {
		return 0, a.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if !a.started {
		if err := a.start(); err != nil {
			a.err = err
			a.Abort()
			return 0, err
		}
	}
	n, err := a.stdin.Write(p)
	a.size += int64(n)
	if err != nil {
		// restic exited (or is exiting): report why it did.
		werr := a.wait()
		if werr == nil {
			werr = err
		}
		a.err = a.rt.failure(a.ctx, "archive object", werr, &a.diag, a.loc)
		return n, a.err
	}
	return n, nil
}

// Size returns the number of bytes written.
func (a *Archiver) Size() int64 { return a.size }

// wait waits for restic to exit (once) and returns its result.
func (a *Archiver) wait() error {
	if a.stopped || a.cmd == nil {
		return nil
	}
	a.stopped = true
	return a.cmd.Wait()
}

// Abort stops restic before its input ends, so no snapshot is saved. It
// may be called more than once, and after Finish.
func (a *Archiver) Abort() {
	a.cancel()
	_ = a.wait()
}

// Finish runs check (when not nil; e.g. the write's preconditions), then
// ends restic's input and waits for the snapshot, whose ID it returns: ""
// for an empty object, which has no snapshot. On any failure, check's
// included, nothing is archived.
func (a *Archiver) Finish(check func() error) (string, error) {
	defer a.Abort()
	if a.err != nil {
		return "", a.err
	}
	if check != nil {
		if err := check(); err != nil {
			return "", err
		}
	}
	if !a.started {
		return "", nil
	}
	if err := a.stdin.Close(); err != nil {
		return "", a.rt.failure(a.ctx, "archive object", err, &a.diag, a.loc)
	}
	if err := a.wait(); err != nil {
		return "", a.rt.failure(a.ctx, "archive object", err, &a.diag, a.loc)
	}
	if err := a.out.consume(); err != nil {
		return "", a.rt.failure(a.ctx, "archive object", err, &a.diag, a.loc)
	}
	if !ValidSnapshot(a.out.snapshot) {
		return "", a.rt.failure(a.ctx, "archive object", errors.New("backup returned no valid snapshot ID"), &a.diag, a.loc)
	}
	return a.out.snapshot, nil
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
