package restic

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrSnapshotNotFound is wrapped by the errors for snapshots a
	// repository does not hold.
	ErrSnapshotNotFound = errors.New("restic: snapshot not found")
	// ErrForgetRefused is wrapped by Forget's error when its guard
	// rejected a snapshot.
	ErrForgetRefused = errors.New("restic: snapshot may not be forgotten")
)

// Snapshot is a snapshot of a repository as restic lists it.
type Snapshot struct {
	// ID is the snapshot's full ID (see ValidSnapshot); ShortID is the
	// prefix restic shows.
	ID      string `json:"id"`
	ShortID string `json:"shortId"`
	// Time is the snapshot's time: when the backup started, or
	// TreeOptions.Time.
	Time time.Time `json:"time"`
	// Host is the snapshot's host name.
	Host string `json:"host"`
	// Tags are the snapshot's tags.
	Tags []string `json:"tags,omitempty"`
	// Paths are the backed-up paths, absolute on the host that made the
	// snapshot.
	Paths []string `json:"paths,omitempty"`
	// Parent is the ID of the snapshot the backup compared files with
	// ("" for none).
	Parent string `json:"parent,omitempty"`
	// Summary is the backup's account (nil for snapshots made by restic
	// releases before 0.17, which recorded none).
	Summary *Summary `json:"summary,omitempty"`
}

// HasTag reports whether s is tagged tag.
func (s Snapshot) HasTag(tag string) bool { return slices.Contains(s.Tags, tag) }

// resticSnapshot is restic's JSON form of a Snapshot (`snapshots --json`).
type resticSnapshot struct {
	ID       string         `json:"id"`
	ShortID  string         `json:"short_id"`
	Time     time.Time      `json:"time"`
	Hostname string         `json:"hostname"`
	Tags     []string       `json:"tags"`
	Paths    []string       `json:"paths"`
	Parent   string         `json:"parent"`
	Summary  *resticSummary `json:"summary"`
}

// snapshot converts s.
func (s resticSnapshot) snapshot() Snapshot {
	out := Snapshot{
		ID: s.ID, ShortID: s.ShortID, Time: s.Time, Host: s.Hostname,
		Tags: s.Tags, Paths: s.Paths, Parent: s.Parent,
	}
	if s.Summary != nil {
		sum := s.Summary.summary()
		sum.SnapshotID = s.ID
		out.Summary = &sum
	}
	return out
}

// SnapshotFilter selects snapshots (see Runtime.Snapshots). The zero value
// selects every snapshot.
type SnapshotFilter struct {
	// Host, when not "", is the host name the snapshots must have.
	Host string
	// Tags are tags the snapshots must all have (each a ValidTag).
	Tags []string
	// IDs, when not empty, limits the listing to these snapshots (full
	// IDs, see ValidSnapshot), sparing restic loading the others; IDs the
	// repository lacks are left out.
	IDs []string
}

// args returns restic's arguments for the filter.
func (f SnapshotFilter) args() ([]string, error) {
	var args []string
	if f.Host != "" {
		if !validValue(f.Host) {
			return nil, fmt.Errorf("restic: invalid host name %q", f.Host)
		}
		args = append(args, "--host="+f.Host)
	}
	if len(f.Tags) > 0 {
		for _, t := range f.Tags {
			if !ValidTag(t) {
				return nil, fmt.Errorf("restic: invalid snapshot tag %q", t)
			}
		}
		// One --tag: a comma-separated list of tags that must all match.
		args = append(args, "--tag="+strings.Join(f.Tags, ","))
	}
	for _, id := range f.IDs {
		if !ValidSnapshot(id) {
			return nil, fmt.Errorf("restic: invalid snapshot ID %q", id)
		}
	}
	return args, nil
}

// match reports whether s passes the filter (restic applies it too; this
// keeps the result independent of the restic release, which may not
// filter snapshots named by ID).
func (f SnapshotFilter) match(s Snapshot) bool {
	if f.Host != "" && s.Host != f.Host {
		return false
	}
	for _, t := range f.Tags {
		if !s.HasTag(t) {
			return false
		}
	}
	return len(f.IDs) == 0 || slices.Contains(f.IDs, s.ID)
}

// validValue reports whether s may be passed as the value of a restic
// option: not empty and without control characters.
func validValue(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Snapshots lists the snapshots of the repository loc that pass the filter
// f, oldest first. It reads the repository without a lock.
func (rt *Runtime) Snapshots(ctx context.Context, loc Location, f SnapshotFilter) ([]Snapshot, error) {
	args, err := f.args()
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	if len(f.IDs) > 0 {
		snaps, err = rt.listSnapshots(ctx, loc, append(args, f.IDs...), false)
		if err != nil && ctx.Err() == nil {
			// Some restic releases fail instead of skipping an ID they do
			// not find: list every snapshot instead.
			snaps, err = rt.listSnapshots(ctx, loc, args, true)
		}
	} else {
		snaps, err = rt.listSnapshots(ctx, loc, args, true)
	}
	if err != nil {
		return nil, err
	}
	out := snaps[:0]
	for _, s := range snaps {
		if f.match(s) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Snapshot) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// listSnapshots runs `restic snapshots --json` with the arguments args.
// Unless logged, a failure is only returned.
func (rt *Runtime) listSnapshots(ctx context.Context, loc Location, args []string, logged bool) ([]Snapshot, error) {
	cmd, err := rt.Command(ctx, loc, append([]string{"--no-lock", "snapshots", "--json"}, args...)...)
	if err != nil {
		return nil, err
	}
	var diag stderrTail
	cmd.Stderr = &diag
	out, err := cmd.Output()
	if err != nil {
		if !logged {
			return nil, rt.fault(ctx, "list snapshots", err, &diag, loc)
		}
		return nil, rt.failure(ctx, "list snapshots", err, &diag, loc)
	}
	var listed []resticSnapshot
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, rt.failure(ctx, "list snapshots", fmt.Errorf("decode snapshot list: %w", err), &diag, loc)
	}
	snaps := make([]Snapshot, 0, len(listed))
	for _, s := range listed {
		if !ValidSnapshot(s.ID) {
			return nil, rt.failure(ctx, "list snapshots", fmt.Errorf("listed invalid snapshot ID %q", s.ID), &diag, loc)
		}
		snaps = append(snaps, s.snapshot())
	}
	return snaps, nil
}

// forgetBatch bounds the snapshot IDs of one forget command line (Windows
// limits a command line to 32767 characters).
const forgetBatch = 100

// Forget removes the snapshots ids (full IDs, see ValidSnapshot) from the
// repository loc. It first lists them again and refuses, forgetting
// nothing, when one is missing (ErrSnapshotNotFound) or guard, which is
// required, rejects one (ErrForgetRefused). restic forget only ever
// receives explicit IDs, never a retention policy; the data the snapshots
// referenced stays in the repository until Prune.
//
// Snapshots are forgotten in batches of 100 IDs: if restic fails, those of
// the earlier batches are gone.
func (rt *Runtime) Forget(ctx context.Context, loc Location, ids []string, guard func(Snapshot) bool) error {
	if guard == nil {
		return errors.New("restic: forget needs a guard")
	}
	var unique []string
	for _, id := range ids {
		if !ValidSnapshot(id) {
			return fmt.Errorf("restic: invalid snapshot ID %q", id)
		}
		if !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	snaps, err := rt.Snapshots(ctx, loc, SnapshotFilter{IDs: unique})
	if err != nil {
		return err
	}
	byID := make(map[string]Snapshot, len(snaps))
	for _, s := range snaps {
		byID[s.ID] = s
	}
	for _, id := range unique {
		s, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrSnapshotNotFound, id)
		}
		if !guard(s) {
			return fmt.Errorf("%w: %s", ErrForgetRefused, id)
		}
	}
	for batch := range slices.Chunk(unique, forgetBatch) {
		cmd, err := rt.Command(ctx, loc, append([]string{"forget"}, batch...)...)
		if err != nil {
			return err
		}
		var diag stderrTail
		cmd.Stderr = &diag
		if err := cmd.Run(); err != nil {
			return rt.failure(ctx, "forget snapshots", err, &diag, loc)
		}
	}
	return nil
}

// PruneOptions adjusts Prune.
type PruneOptions struct {
	// MaxUnused is restic's --max-unused: the unused data prune may leave
	// in the repository to avoid repacking ("5%" (restic's default, used
	// for ""), "200M", "0" or "unlimited").
	MaxUnused string
	// RetryLock is how long prune waits for other commands' locks to go
	// away before failing (0: 30 seconds). Prune holds an exclusive lock:
	// while it runs, backups (and object archives) wait or fail.
	RetryLock time.Duration
}

// maxUnusedPattern matches the values restic accepts for --max-unused.
var maxUnusedPattern = regexp.MustCompile(`^(unlimited|[0-9]+(\.[0-9]+)?%|[0-9]+[kKmMgGtT]?)$`)

// Prune removes the data no snapshot of the repository loc references
// any more.
func (rt *Runtime) Prune(ctx context.Context, loc Location, o PruneOptions) error {
	args := []string{"prune"}
	if o.MaxUnused != "" {
		if !maxUnusedPattern.MatchString(o.MaxUnused) {
			return fmt.Errorf("restic: invalid max-unused %q", o.MaxUnused)
		}
		args = append(args, "--max-unused="+o.MaxUnused)
	}
	if o.RetryLock < 0 {
		return fmt.Errorf("restic: invalid lock wait %v", o.RetryLock)
	}
	cmd, err := rt.command(ctx, loc, cmdOptions{retryLock: o.RetryLock}, args...)
	if err != nil {
		return err
	}
	var diag stderrTail
	cmd.Stderr = &diag
	if err := cmd.Run(); err != nil {
		return rt.failure(ctx, "prune", err, &diag, loc)
	}
	return nil
}
