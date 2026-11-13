// Package replication executes local, version-preserving S3 bucket rules.
package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// Worker reconciles acknowledged source versions with their destinations.
// Destination versions are the durable progress record: a successful retry
// finds the same version and origin and does not write it again.
type Worker struct {
	backend  storage.Backend
	iam      *iam.Manager
	log      *slog.Logger
	interval time.Duration
	wake     chan struct{}
	stop     context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
}

// Progress is the persisted scan checkpoint for one source bucket. The
// destination's imported versions remain the authoritative per-version
// completion record, so a replay after a crash is safe.
type Progress struct {
	RuleHash         string    `json:"ruleHash"`
	LastScanAt       time.Time `json:"lastScanAt"`
	LastCompletedKey string    `json:"lastCompletedKey,omitempty"`
	BackfillComplete bool      `json:"backfillComplete"`
	RetryCount       int       `json:"retryCount,omitempty"`
	LastError        string    `json:"lastError,omitempty"`
}

type progressStore interface {
	LoadReplicationProgress(context.Context, string) ([]byte, bool, error)
	SaveReplicationProgress(context.Context, string, []byte, bool, []byte) error
}

var errReplicationLeaderChanged = errors.New("replication leader changed during scan")

func (w *Worker) isActiveLeader() bool {
	leader, ok := w.backend.(interface{ IsClusterLeader() bool })
	return !ok || leader.IsClusterLeader()
}

func NewWorker(backend storage.Backend, identity *iam.Manager, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Worker{backend: backend, iam: identity, log: logger, interval: 30 * time.Second, wake: make(chan struct{}, 1)}
}

// Start begins periodic reconciliation. Router change events prompt a scan,
// but the periodic scan also catches work missed during a crash or outage.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		return
	}
	ctx, w.stop = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go w.loop(ctx, w.done)
}

func (w *Worker) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	var unsubscribe func()
	if router, ok := w.backend.(*multi.Router); ok {
		unsubscribe = router.Subscribe(func(change multi.Change) {
			if change.Kind == multi.ChangeObject || change.ConfigKey == storage.ConfigReplication {
				select {
				case w.wake <- struct{}{}:
				default:
				}
			}
		})
		defer unsubscribe()
	}
	timer := time.NewTicker(w.interval)
	defer timer.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("bucket replication has pending copies", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-timer.C:
		}
	}
}

func (w *Worker) Stop() {
	w.mu.Lock()
	stop, done := w.stop, w.done
	w.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// RunOnce scans configured versioned buckets. Invalid legacy documents or a
// rule whose activation state does not match are left inactive.
func (w *Worker) RunOnce(ctx context.Context) error {
	if !w.isActiveLeader() {
		return nil
	}
	buckets, err := w.backend.ListBuckets(ctx)
	if err != nil {
		return err
	}
	var pending []error
	for _, bucket := range buckets {
		if !w.isActiveLeader() {
			return errors.Join(append(pending, errReplicationLeaderChanged)...)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if bucket.Versioning != storage.VersioningEnabled {
			continue
		}
		if err := w.scanBucket(ctx, bucket.Name); err != nil {
			pending = append(pending, fmt.Errorf("bucket %s: %w", bucket.Name, err))
		}
	}
	return errors.Join(pending...)
}

func (w *Worker) scanBucket(ctx context.Context, source string) error {
	raw, err := w.backend.GetBucketConfig(ctx, source, storage.ConfigReplication)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	role, rules, err := s3.CompileReplicationRules(raw)
	if err != nil {
		w.log.Warn("unsupported stored replication rules are inactive", "bucket", source, "err", err)
		return nil
	}
	stateRaw, err := w.backend.GetBucketConfig(ctx, source, storage.ConfigReplicationState)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var state s3.ReplicationState
	if json.Unmarshal(stateRaw, &state) != nil || state.Hash != s3.ReplicationRuleHash(raw) || state.ActivatedAt.IsZero() {
		return nil
	}
	var oldProgress []byte
	var progressFound bool
	var progress Progress
	if store, ok := w.backend.(progressStore); ok {
		oldProgress, progressFound, err = store.LoadReplicationProgress(ctx, source)
		if err != nil {
			return err
		}
		if progressFound && json.Unmarshal(oldProgress, &progress) != nil {
			return errors.New("invalid persisted replication progress")
		}
		if progress.RuleHash != state.Hash {
			progress = Progress{RuleHash: state.Hash}
		}
	}
	var pending []error
	var keyVersions []storage.ObjectInfo
	var lastCompletedKey string
	flushKey := func() {
		if len(keyVersions) == 0 {
			return
		}
		// WalkVersions reports newest first. Import oldest first so a new
		// destination has the same latest version as the source.
		// Once a version fails for a destination, leave later versions for
		// the next pass. Publishing them now could make the failed older
		// version impossible to import without changing latest-object order.
		blocked := make(map[string]bool)
		key := keyVersions[0].Key
		for i := len(keyVersions) - 1; i >= 0; i-- {
			if !w.isActiveLeader() {
				pending = append(pending, errReplicationLeaderChanged)
				break
			}
			version := keyVersions[i]
			if version.ReplicationOrigin != "" {
				continue
			}
			for _, rule := range s3.MatchingReplicationRules(rules, version.Key, version.Tags, version.DeleteMarker) {
				if !rule.Backfill && version.LastModified.Before(state.ActivatedAt) {
					continue
				}
				if blocked[rule.Destination] {
					continue
				}
				if err := w.copyVersion(ctx, role, source, version, rule); err != nil {
					pending = append(pending, fmt.Errorf("%s@%s to %s: %w", version.Key, version.VersionID, rule.Destination, err))
					blocked[rule.Destination] = true
				}
			}
		}
		if len(blocked) == 0 && len(pending) == 0 {
			lastCompletedKey = key
		}
		keyVersions = keyVersions[:0]
	}
	err = w.backend.WalkVersions(ctx, source, "", "", "", func(version storage.ObjectInfo) error {
		if !w.isActiveLeader() {
			return errReplicationLeaderChanged
		}
		if len(keyVersions) > 0 && keyVersions[0].Key != version.Key {
			flushKey()
			if !w.isActiveLeader() {
				return errReplicationLeaderChanged
			}
		}
		keyVersions = append(keyVersions, version)
		return nil
	})
	// A failed walk may have delivered only the newest portion of this key.
	// Do not import it until the older versions can be enumerated as well.
	if err == nil {
		flushKey()
	}
	if err != nil {
		pending = append(pending, err)
	}
	if store, ok := w.backend.(progressStore); ok && w.isActiveLeader() {
		now := time.Now().UTC()
		if progressFound && progress.RuleHash == state.Hash && progress.BackfillComplete && len(pending) == 0 &&
			progress.LastCompletedKey == lastCompletedKey && now.Sub(progress.LastScanAt) < 5*time.Minute {
			return nil
		}
		progress.RuleHash = state.Hash
		progress.LastScanAt = now
		progress.LastCompletedKey = lastCompletedKey
		progress.BackfillComplete = len(pending) == 0
		if len(pending) == 0 {
			progress.RetryCount, progress.LastError = 0, ""
		} else {
			progress.RetryCount++
			progress.LastError = errors.Join(pending...).Error()
		}
		next, marshalErr := json.Marshal(progress)
		if marshalErr != nil {
			pending = append(pending, marshalErr)
		} else if saveErr := store.SaveReplicationProgress(ctx, source, oldProgress, progressFound, next); saveErr != nil {
			pending = append(pending, fmt.Errorf("saving replication progress: %w", saveErr))
		}
	}
	return errors.Join(pending...)
}

func (w *Worker) copyVersion(ctx context.Context, role, source string, version storage.ObjectInfo, rule s3.ExecutableReplicationRule) error {
	if !w.isActiveLeader() {
		return errReplicationLeaderChanged
	}
	if w.iam == nil {
		return errors.New("replication role store unavailable")
	}
	origin := source + "\x00" + version.Key + "\x00" + version.VersionID
	id := copier.MapVersionID(version.VersionID)
	existing, err := w.backend.GetObjectInfo(ctx, rule.Destination, version.Key, id)
	if err == nil {
		if existing.ReplicationOrigin == origin {
			expected := version.ObjectMeta
			expected.ReplicationOrigin = origin
			if existing.DeleteMarker == version.DeleteMarker && existing.Size == version.Size && existing.LastModified.Equal(version.LastModified) && copier.MetaEqual(existing.ObjectMeta, expected) {
				return nil
			}
			return errors.New("destination copy changed after replication")
		}
		return errors.New("destination version ID belongs to another object")
	}
	if !errors.Is(err, storage.ErrObjectNotFound) && !errors.Is(err, storage.ErrVersionNotFound) {
		return err
	}
	actions := []string{iam.ReplicateObjectAction}
	if version.DeleteMarker {
		actions = []string{iam.ReplicateDeleteAction}
	}
	if len(version.Tags) > 0 {
		actions = append(actions, iam.ReplicateTagsAction)
	}
	if version.LegalHold || version.RetentionMode != "" || !version.RetainUntil.IsZero() {
		actions = append(actions, iam.ReplicateObjectLockAction)
	}
	if !w.iam.CanReplicate(ctx, role, source, rule.Destination, version.Key, actions...) {
		return errors.New("replication role denied destination write")
	}
	destination, err := w.backend.GetBucket(ctx, rule.Destination)
	if err != nil {
		return err
	}
	if destination.Versioning != storage.VersioningEnabled {
		return errors.New("destination versioning is not enabled")
	}
	if (version.LegalHold || version.RetentionMode != "" || !version.RetainUntil.IsZero()) && !destination.ObjectLockEnabled {
		return errors.New("destination object lock is not enabled")
	}
	var newerDestination bool
	var latestDestination storage.ObjectInfo
	var destinationFound bool
	if err := w.backend.WalkVersions(ctx, rule.Destination, version.Key, "", "", func(existing storage.ObjectInfo) error {
		if existing.Key != version.Key {
			return storage.ErrStopWalk
		}
		if existing.LastModified.After(version.LastModified) {
			newerDestination = true
			return storage.ErrStopWalk
		}
		if existing.IsLatest {
			latestDestination, destinationFound = existing, true
		}
		return nil
	}); err != nil {
		return err
	}
	if newerDestination {
		return errors.New("destination has a newer version; importing this version would change the latest object")
	}
	var body io.ReadCloser
	if !version.DeleteMarker {
		current, opened, err := w.backend.OpenObject(ctx, source, version.Key, version.VersionID)
		if err != nil {
			return err
		}
		if !copier.SameVersion(current, version) || !copier.MetaEqual(current.ObjectMeta, version.ObjectMeta) {
			_ = opened.Close()
			return errors.New("source version changed during reconciliation")
		}
		version = current
		body = opened
		defer body.Close()
	}
	version.ReplicationOrigin = origin
	if !w.isActiveLeader() {
		return errReplicationLeaderChanged
	}
	_, err = copier.WriteVersionIf(ctx, w.backend, rule.Destination, version.Key, version, body, copier.Policy{}, func(current storage.ObjectInfo, found bool) error {
		if found != destinationFound || found && (!copier.SameVersion(current, latestDestination) || !copier.MetaEqual(current.ObjectMeta, latestDestination.ObjectMeta)) {
			return storage.ErrPreconditionFailed
		}
		return nil
	})
	return err
}
