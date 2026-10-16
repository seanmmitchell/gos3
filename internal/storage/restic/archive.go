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
	"unicode"
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

// Snapshot tags written by gos3. Names are encoded in unpadded base64url,
// which restic keeps intact (a comma would split a tag).
const (
	// TagObject marks the snapshot of one object version (Archiver).
	// Whole-bucket snapshot retention never forgets such snapshots.
	TagObject = "gos3-object"
	// TagBucketPrefix starts the tag naming a snapshot's bucket (BucketTag).
	TagBucketPrefix = "gos3-bucket:"
	// TagBucketIDPrefix starts the tag naming a snapshot's bucket ID
	// (BucketIDTag).
	TagBucketIDPrefix = "gos3-bucket-id:"
	// TagKeyPrefix starts the tag naming an object version's key (KeyTag).
	TagKeyPrefix = "gos3-key:"
)

// BucketTag returns the tag naming the bucket bucket.
func BucketTag(bucket string) string {
	return TagBucketPrefix + base64.RawURLEncoding.EncodeToString([]byte(bucket))
}

// BucketIDTag returns the tag naming the bucket ID id (verbatim: bucket
// IDs are tokens; see ValidTag).
func BucketIDTag(id string) string { return TagBucketIDPrefix + id }

// KeyTag returns the tag naming the object key key.
func KeyTag(key string) string {
	return TagKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(key))
}

// ValidTag reports whether restic keeps tag as one tag: it is not empty
// and holds no comma (restic splits tags at commas), white space or
// control characters.
func ValidTag(tag string) bool {
	if tag == "" {
		return false
	}
	for _, r := range tag {
		if r == ',' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ArchiveOptions identifies the object version an Archiver archives.
type ArchiveOptions struct {
	// Bucket is the bucket's name (tagged with BucketTag).
	Bucket string
	// BucketID is the bucket's ID, tagged with BucketIDTag unless "". It
	// must be a ValidTag; otherwise the first write fails.
	BucketID string
	// Key is the object's key (tagged with KeyTag).
	Key string
}

// NewArchiver returns an Archiver of a version of bucket/key into the
// repository loc: NewArchiverWith without a bucket ID.
func (rt *Runtime) NewArchiver(ctx context.Context, loc Location, bucket, key string) *Archiver {
	return rt.NewArchiverWith(ctx, loc, ArchiveOptions{Bucket: bucket, Key: key})
}

// NewArchiverWith returns an Archiver of a version of o.Bucket/o.Key into
// the repository loc, which the caller has prepared (see PrepareWith). The
// snapshot is tagged TagObject, BucketTag(o.Bucket), KeyTag(o.Key) and,
// when o.BucketID is set, BucketIDTag(o.BucketID), with host "gos3" and
// file name "object". Nothing runs until the first byte is written; the
// caller must Finish or Abort it.
func (rt *Runtime) NewArchiverWith(ctx context.Context, loc Location, o ArchiveOptions) *Archiver {
	a := &Archiver{rt: rt, ctx: ctx, loc: loc, tags: []string{TagObject, BucketTag(o.Bucket), KeyTag(o.Key)}}
	if o.BucketID != "" {
		a.tags = append(a.tags, BucketIDTag(o.BucketID))
	}
	a.cmdCtx, a.cancel = context.WithCancel(ctx)
	return a
}

// start runs restic.
func (a *Archiver) start() error {
	a.started = true
	args := []string{"backup", "--stdin", "--stdin-filename", "object", "--json", "--host", "gos3"}
	for _, t := range a.tags {
		if !ValidTag(t) {
			return fmt.Errorf("restic: invalid snapshot tag %q", t)
		}
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

// maxRecord bounds one JSON record of restic's output.
const maxRecord = 1 << 20

// jsonLines splits restic's JSON output (one record per line) and passes
// each non-blank record to record as it completes, so that long outputs
// (backup progress, diffs) are consumed incrementally instead of being
// buffered. An error from record stops the output (and so restic).
type jsonLines struct {
	record  func([]byte) error
	pending []byte
}

func (w *jsonLines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		line, rest, complete := bytes.Cut(p, []byte{'\n'})
		if len(w.pending)+len(line) > maxRecord {
			return 0, errors.New("restic: oversized JSON record")
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

// consume passes the pending record (the last one may lack a newline:
// call consume once restic exited).
func (w *jsonLines) consume() error {
	line := bytes.TrimSpace(w.pending)
	w.pending = w.pending[:0]
	if len(line) == 0 {
		return nil
	}
	return w.record(line)
}

// snapshotOutput consumes restic's JSON backup output incrementally: a long
// upload must not fill a bounded buffer and lose the final summary that
// holds the snapshot ID.
type snapshotOutput struct {
	jsonLines
	snapshot string  // the summary's snapshot ID
	summary  Summary // valid when snapshot is set
}

// Write implements io.Writer.
func (w *snapshotOutput) Write(p []byte) (int, error) {
	if w.record == nil {
		w.record = w.decode
	}
	return w.jsonLines.Write(p)
}

// consume decodes the pending record (see jsonLines.consume).
func (w *snapshotOutput) consume() error {
	if w.record == nil {
		w.record = w.decode
	}
	return w.jsonLines.consume()
}

// decode decodes one JSON record, keeping a summary.
func (w *snapshotOutput) decode(line []byte) error {
	var item struct {
		MessageType string `json:"message_type"`
	}
	if err := json.Unmarshal(line, &item); err != nil {
		return fmt.Errorf("restic: decode backup result: %w", err)
	}
	if item.MessageType != "summary" {
		return nil
	}
	var s resticSummary
	if err := json.Unmarshal(line, &s); err != nil {
		return fmt.Errorf("restic: decode backup summary: %w", err)
	}
	w.summary = s.summary()
	w.snapshot = s.SnapshotID
	return nil
}
