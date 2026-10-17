package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"
)

// resticTimeFormat is the layout of restic's --time option.
const resticTimeFormat = "2006-01-02 15:04:05"

// Summary is restic's account of a backup.
type Summary struct {
	// FilesNew, FilesChanged and FilesUnmodified count the files the
	// parent snapshot lacked, those restic read and found different, and
	// those it kept (unread when their size and modification time matched
	// the parent's, see TreeOptions).
	FilesNew        int64 `json:"filesNew"`
	FilesChanged    int64 `json:"filesChanged"`
	FilesUnmodified int64 `json:"filesUnmodified"`
	// DirsNew, DirsChanged and DirsUnmodified count directories likewise.
	DirsNew        int64 `json:"dirsNew"`
	DirsChanged    int64 `json:"dirsChanged"`
	DirsUnmodified int64 `json:"dirsUnmodified"`
	// DataAdded is the size of the data the backup added to the repository
	// (before compression).
	DataAdded int64 `json:"dataAdded"`
	// TotalFilesProcessed and TotalBytesProcessed count every file of the
	// snapshot and their sizes.
	TotalFilesProcessed int64 `json:"totalFilesProcessed"`
	TotalBytesProcessed int64 `json:"totalBytesProcessed"`
	// SnapshotID is the snapshot's ID.
	SnapshotID string `json:"snapshotId,omitempty"`
}

// resticSummary is restic's JSON form of a Summary: the summary record of
// `backup --json` and the summary of a listed snapshot.
type resticSummary struct {
	FilesNew            int64  `json:"files_new"`
	FilesChanged        int64  `json:"files_changed"`
	FilesUnmodified     int64  `json:"files_unmodified"`
	DirsNew             int64  `json:"dirs_new"`
	DirsChanged         int64  `json:"dirs_changed"`
	DirsUnmodified      int64  `json:"dirs_unmodified"`
	DataAdded           int64  `json:"data_added"`
	TotalFilesProcessed int64  `json:"total_files_processed"`
	TotalBytesProcessed int64  `json:"total_bytes_processed"`
	SnapshotID          string `json:"snapshot_id"`
}

// summary converts s.
func (s resticSummary) summary() Summary {
	return Summary{
		FilesNew: s.FilesNew, FilesChanged: s.FilesChanged, FilesUnmodified: s.FilesUnmodified,
		DirsNew: s.DirsNew, DirsChanged: s.DirsChanged, DirsUnmodified: s.DirsUnmodified,
		DataAdded: s.DataAdded, TotalFilesProcessed: s.TotalFilesProcessed,
		TotalBytesProcessed: s.TotalBytesProcessed, SnapshotID: s.SnapshotID,
	}
}

// TreeOptions adjusts BackupTree.
type TreeOptions struct {
	// Parent is the snapshot (a full ID) the backup compares files with: a
	// file whose size and modification time match the parent's file at the
	// same path (and its inode and change time, unless ignored) is not
	// read; its content is taken from the parent. "" lets restic choose
	// the newest snapshot of the same host and paths.
	Parent string
	// Tags are the snapshot's tags (each a ValidTag).
	Tags []string
	// Host is the snapshot's host name ("": the server's host name).
	Host string
	// Time is the snapshot's time (zero: now). restic keeps whole seconds.
	Time time.Time
	// IgnoreInode and IgnoreCtime leave the inode and the change time out
	// of the comparison with the parent: a file replaced by a placeholder
	// of the same size and modification time (MakePlaceholder) is then
	// unchanged.
	IgnoreInode, IgnoreCtime bool
}

// BackupTree backs up the directory tree dir into a new snapshot of the
// repository loc and returns restic's summary, which holds the snapshot's
// ID. The snapshot holds dir's entries at its root (dir's own path is
// recorded in Snapshot.Paths only): dir/a/b is /a/b in the snapshot.
//
// When restic saved a snapshot but failed (it could not read some files:
// the snapshot is incomplete), the error is returned with the summary,
// whose SnapshotID names that snapshot so that the caller can forget it.
func (rt *Runtime) BackupTree(ctx context.Context, loc Location, dir string, o TreeOptions) (Summary, error) {
	if fi, err := os.Stat(dir); err != nil {
		return Summary{}, fmt.Errorf("restic: back up tree: %w", err)
	} else if !fi.IsDir() {
		return Summary{}, fmt.Errorf("restic: back up tree: %s is not a directory", dir)
	}
	// --no-scan: the progress estimate would walk the whole tree once more.
	args := []string{"backup", "--json", "--no-scan"}
	if o.Parent != "" {
		if !ValidSnapshot(o.Parent) {
			return Summary{}, fmt.Errorf("restic: invalid parent snapshot ID %q", o.Parent)
		}
		args = append(args, "--parent="+o.Parent)
	}
	if o.IgnoreInode {
		args = append(args, "--ignore-inode")
	}
	if o.IgnoreCtime {
		args = append(args, "--ignore-ctime")
	}
	var copts cmdOptions
	if !o.Time.IsZero() {
		var arg string
		arg, copts.env = timeArg(o.Time)
		args = append(args, "--time="+arg)
	}
	if o.Host != "" {
		if !validValue(o.Host) {
			return Summary{}, fmt.Errorf("restic: invalid host name %q", o.Host)
		}
		args = append(args, "--host="+o.Host)
	}
	for _, t := range o.Tags {
		if !ValidTag(t) {
			return Summary{}, fmt.Errorf("restic: invalid snapshot tag %q", t)
		}
		args = append(args, "--tag="+t)
	}
	// restic replaces a target of only relative components by the
	// directory's entries: they become the snapshot's root.
	args = append(args, ".")
	cmd, err := rt.command(ctx, loc, copts, args...)
	if err != nil {
		return Summary{}, err
	}
	cmd.Dir = dir
	var out snapshotOutput
	var diag stderrTail
	cmd.Stdout, cmd.Stderr = &out, &diag
	err = cmd.Run()
	if cerr := out.consume(); err == nil {
		err = cerr
	}
	sum := out.summary
	sum.SnapshotID = out.snapshot
	if sum.SnapshotID != "" && !ValidSnapshot(sum.SnapshotID) {
		return Summary{}, rt.failure(ctx, "back up tree", fmt.Errorf("backup returned invalid snapshot ID %q", sum.SnapshotID), &diag, loc)
	}
	if err != nil {
		return sum, rt.failure(ctx, "back up tree", err, &diag, loc)
	}
	if sum.SnapshotID == "" {
		return Summary{}, rt.failure(ctx, "back up tree", errors.New("backup returned no snapshot ID"), &diag, loc)
	}
	return sum, nil
}

// DiffEntry is one difference between two snapshots (see Runtime.Diff).
type DiffEntry struct {
	// Path is the item's absolute path in the snapshots ("/a/b");
	// directories end in "/".
	Path string `json:"path"`
	// Modifier is restic's: "+" added, "-" removed, "M" content changed,
	// "T" type changed, "U" metadata changed, "?" content damaged (bitrot),
	// or a combination such as "TM".
	Modifier string `json:"modifier"`
}

// Diff returns the differences from snapshot a to snapshot b (full IDs) of
// the repository loc, in restic's order: depth first, by name. Items
// whose metadata alone changed are not reported. It reads the repository
// without a lock.
func (rt *Runtime) Diff(ctx context.Context, loc Location, a, b string) ([]DiffEntry, error) {
	for _, id := range []string{a, b} {
		if !ValidSnapshot(id) {
			return nil, fmt.Errorf("restic: invalid snapshot ID %q", id)
		}
	}
	cmd, err := rt.Command(ctx, loc, "--no-lock", "diff", "--json", a, b)
	if err != nil {
		return nil, err
	}
	var entries []DiffEntry
	out := jsonLines{record: func(line []byte) error {
		var item struct {
			MessageType string `json:"message_type"`
			Path        string `json:"path"`
			Modifier    string `json:"modifier"`
		}
		if err := json.Unmarshal(line, &item); err != nil {
			return fmt.Errorf("restic: decode diff: %w", err)
		}
		if item.MessageType == "change" {
			entries = append(entries, DiffEntry{Path: item.Path, Modifier: item.Modifier})
		}
		return nil
	}}
	var diag stderrTail
	cmd.Stdout, cmd.Stderr = &out, &diag
	err = cmd.Run()
	if cerr := out.consume(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, rt.failure(ctx, "diff snapshots", err, &diag, loc)
	}
	return entries, nil
}

// DumpFile writes the content of the file at the absolute path name
// ("/a/b") of the snapshot (a full ID) of the repository loc to w. On
// failure, w may have received part of it. name must name a file: restic
// writes a directory as a tar archive (see DumpTar).
func (rt *Runtime) DumpFile(ctx context.Context, loc Location, snapshot, name string, w io.Writer) error {
	if name == "/" {
		return errors.New("restic: dump: the snapshot root is not a file")
	}
	return rt.dumpPath(ctx, loc, "dump file", snapshot, name, w)
}

// DumpTar writes a tar archive of the directory at the absolute path name
// ("/" for the whole snapshot) of the snapshot (a full ID) of the
// repository loc to w. Entry names are the items' paths in the snapshot
// without the leading slash ("a/b/c" for /a/b/c); directories end in "/".
// On failure, w may have received part of it. name must name a directory:
// restic writes a file as is.
func (rt *Runtime) DumpTar(ctx context.Context, loc Location, snapshot, name string, w io.Writer) error {
	return rt.dumpPath(ctx, loc, "dump tar archive", snapshot, name, w, "--archive", "tar")
}

// dumpPath runs restic dump with the options opts for the item name of
// snapshot, writing to w. It reads the repository without a lock.
func (rt *Runtime) dumpPath(ctx context.Context, loc Location, op, snapshot, name string, w io.Writer, opts ...string) error {
	if !ValidSnapshot(snapshot) {
		return fmt.Errorf("restic: invalid snapshot ID %q", snapshot)
	}
	if name == "" || name[0] != '/' || path.Clean(name) != name {
		return fmt.Errorf("restic: invalid snapshot path %q", name)
	}
	args := append([]string{"--no-lock", "dump"}, opts...)
	cmd, err := rt.Command(ctx, loc, append(args, snapshot, name)...)
	if err != nil {
		return err
	}
	out := &trackedWriter{w: w}
	var diag stderrTail
	cmd.Stdout, cmd.Stderr = out, &diag
	err = cmd.Run()
	if out.err != nil {
		// The destination failed (and stopped restic).
		return fmt.Errorf("restic: %s: %w", op, out.err)
	}
	if err != nil {
		return rt.failure(ctx, op, err, &diag, loc)
	}
	return nil
}

// trackedWriter remembers the first error of its writer.
type trackedWriter struct {
	w   io.Writer
	err error
}

func (t *trackedWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil && t.err == nil {
		t.err = err
	}
	return n, err
}
