package main

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/server"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// Offline use of `gos3 restic` (the server stopped): the repository and its
// password are read from the backings of the server's configuration
// (configuration file, GOS3_* variables, backings.json in the system
// directory), never written.
//
//   - fs backings: the bucket's ID comes from the metadata database
//     <path>/.gos3/meta.db, opened read-only (it is locked while a server
//     uses it; then the command fails and suggests --server); a bucket that
//     no longer exists is found by the name in its recovery record. The
//     record <path>/.gos3/keys/<id>.restic.json holds the password; the
//     repository is <path>/.gos3/restic/<id>.
//   - s3 backings: the recovery records keys/<id>.restic.json of a
//     container name the bucket; the password is the hex of the bucket's
//     master key keys/<id>.key, unwrapped with the backing's encryption_key
//     (s3store.ReadResticKey). A backing without a home keeps its registry
//     in <path>/.gos3/s3/<backing>/state.db, which names the container of
//     each wrapped bucket. A backing with a home keeps it in the home's
//     journal, which only the server reads: offline, only the home's own
//     buckets (the default container) are found; wrapped buckets in other
//     containers need --server.
//   - whole-bucket snapshots (--snapshots): the snapshot scheduler's state
//     <path>/.gos3/restic-snapshots.json names the target of each bucket's
//     schedule: the bucket's own repository, or the aux repository <repo>
//     of a backing (fs: restic-aux/<repo> with the record
//     keys/aux/<repo>.restic.json; s3: <home prefix>restic-aux/<repo> with
//     the wrapped key keys/aux/<repo>.key).

// errNotHere reports that a backing holds no repository of the bucket.
var errNotHere = errors.New("no restic repository of the bucket on this backing")

// errServerRunning is the error for a database a running server holds.
func errServerRunning(path string) error {
	return fmt.Errorf("%s is locked by a running gos3 server: stop it, or ask it with --server URL (online)", path)
}

// lockTimeout bounds the wait for the lock of a database a server holds.
const lockTimeout = time.Second

// offlineBacking is one backing of the configuration.
type offlineBacking struct {
	name, backend, path string
	s3                  *config.S3Config
}

// offlineSource finds repositories in the backings of a configuration.
type offlineSource struct {
	backings []offlineBacking
	sysDir   string // the server's system directory ("" when ephemeral)
	bucketID string // --bucket-id
	notes    io.Writer
}

// openOffline loads the server's configuration for offline use.
func (f *resticFlags) openOffline(notes io.Writer) (*offlineSource, error) {
	var overrides []config.Override
	if f.data != "" {
		overrides = append(overrides, config.Override{Key: "storage.path", Value: f.data})
	}
	cfg, err := config.Load(f.configPath, overrides...)
	if err != nil {
		return nil, err
	}
	// Backings configured live through the admin API, as the server
	// reads them at its start.
	if err := cfg.LoadBackings(); err != nil {
		return nil, err
	}
	st := cfg.Storage
	o := &offlineSource{sysDir: cfg.SystemDir(), bucketID: f.bucketID, notes: notes}
	o.backings = append(o.backings, offlineBacking{name: config.DefaultBackingName, backend: st.Backend, path: st.Path, s3: st.S3})
	for _, b := range st.Backings {
		o.backings = append(o.backings, offlineBacking{name: b.Name, backend: b.Backend, path: b.Path, s3: b.S3})
	}
	return o, nil
}

// backing returns the backing name.
func (o *offlineSource) backing(name string) (offlineBacking, error) {
	for _, b := range o.backings {
		if b.name == name {
			return b, nil
		}
	}
	names := make([]string, len(o.backings))
	for i, b := range o.backings {
		names[i] = b.name
	}
	return offlineBacking{}, fmt.Errorf("the configuration has no backing %q (it has %s)", name, strings.Join(names, ", "))
}

// primary returns the primary backing of bucket recorded in the server's
// placements.json ("" when unknown).
func (o *offlineSource) primary(bucket string) string {
	if o.sysDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(o.sysDir, server.PlacementsFileName))
	if err != nil {
		return ""
	}
	var f struct {
		Version int `json:"version"`
		Buckets map[string]struct {
			Members []struct {
				Backing string `json:"backing"`
			} `json:"members"`
		} `json:"buckets"`
	}
	if json.Unmarshal(data, &f) != nil || f.Version != 1 {
		return ""
	}
	if p, ok := f.Buckets[bucket]; ok && len(p.Members) > 0 {
		return p.Members[0].Backing
	}
	return ""
}

// archive finds the per-version archive of bucket: on backing when not "",
// else on its primary (placements.json), else on whichever backing has
// one.
func (o *offlineSource) archive(ctx context.Context, bucket, backing string) (resticRepo, error) {
	var candidates []offlineBacking
	if backing == "" {
		backing = o.primary(bucket)
	}
	if backing != "" {
		b, err := o.backing(backing)
		if err != nil {
			return resticRepo{}, err
		}
		candidates = []offlineBacking{b}
	} else {
		candidates = o.backings
	}
	var (
		found []resticRepo
		errs  []error
	)
	for _, b := range candidates {
		repo, err := o.archiveOn(ctx, b, bucket)
		switch {
		case err == nil:
			found = append(found, repo)
		case errors.Is(err, errNotHere) && len(candidates) > 1:
		default:
			errs = append(errs, err)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		var names []string
		for _, r := range found {
			names = append(names, r.backing)
		}
		return resticRepo{}, fmt.Errorf("bucket %q has restic archives on backings %s (its copies): choose one with --backing",
			bucket, strings.Join(names, ", "))
	case len(errs) > 0:
		return resticRepo{}, errors.Join(errs...)
	}
	return resticRepo{}, fmt.Errorf("no backing of the configuration holds a restic archive of bucket %q", bucket)
}

// archiveOn finds the per-version archive of bucket on backing b
// (errNotHere when b has none).
func (o *offlineSource) archiveOn(ctx context.Context, b offlineBacking, bucket string) (resticRepo, error) {
	var (
		loc restic.Location
		err error
	)
	switch b.backend {
	case config.BackendFS:
		loc, err = o.fsArchive(b, bucket)
	case config.BackendS3:
		loc, err = o.s3Archive(ctx, b, bucket)
	default:
		err = fmt.Errorf("%w: backing %q keeps its data in memory", errNotHere, b.name)
	}
	if err != nil {
		return resticRepo{}, err
	}
	return resticRepo{loc: loc, backing: b.name}, nil
}

// --- fs backings ---

// fsSystemDir returns the system directory of the fs backing at path.
func fsSystemDir(path string) string { return filepath.Join(path, config.SystemDirName) }

// fsRecord is a recovery record of an fs backing (keys/<id>.restic.json,
// keys/aux/<name>.restic.json).
type fsRecord struct {
	Repository string `json:"repository"`
	Password   string `json:"password"`
	Bucket     string `json:"bucket"`
}

// readFSRecord reads the recovery record at path (os.ErrNotExist when there
// is none).
func readFSRecord(path string) (fsRecord, error) {
	var rec fsRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("damaged restic recovery record %s: %v", path, err)
	}
	return rec, nil
}

// fsPassword returns the password of the record at recPath, or the hex of
// the key at keyPath when the record has none.
func fsPassword(rec fsRecord, keyPath string) (string, error) {
	if rec.Password != "" {
		return rec.Password, nil
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("reading the repository's key: %w", err)
	}
	return restic.PasswordFromKey(key), nil
}

// fsArchive finds bucket's archive on the fs backing b.
func (o *offlineSource) fsArchive(b offlineBacking, bucket string) (restic.Location, error) {
	sys := fsSystemDir(b.path)
	id := o.bucketID
	if id == "" {
		var err error
		if id, err = fsBucketID(sys, bucket); errors.Is(err, errNotHere) {
			// A deleted bucket: its key, record and repository remain.
			ids, err := fsRecordsNamed(sys, bucket)
			if err != nil {
				return restic.Location{}, err
			}
			switch len(ids) {
			case 0:
				return restic.Location{}, fmt.Errorf("%w: backing %q has no bucket %q", errNotHere, b.name, bucket)
			case 1:
				id = ids[0]
				fmt.Fprintf(o.notes, "note: bucket %q no longer exists on backing %q; using the archive of its former ID %s\n", bucket, b.name, id)
			default:
				return restic.Location{}, fmt.Errorf("bucket %q no longer exists on backing %q and recovery records of several former buckets of that name remain (IDs %s): choose one with --bucket-id",
					bucket, b.name, strings.Join(ids, ", "))
			}
		} else if err != nil {
			return restic.Location{}, err
		}
	}
	if !validBucketID(id) {
		return restic.Location{}, fmt.Errorf("backing %q: bucket %q has an unexpected ID %q", b.name, bucket, id)
	}
	recPath := filepath.Join(sys, "keys", id+".restic.json")
	rec, err := readFSRecord(recPath)
	switch {
	case errors.Is(err, os.ErrNotExist) && o.bucketID != "":
		return restic.Location{}, fmt.Errorf("%w: backing %q has no recovery record %s", errNotHere, b.name, recPath)
	case errors.Is(err, os.ErrNotExist):
		return restic.Location{}, fmt.Errorf("bucket %q on backing %q has no restic archive (no recovery record %s: nothing was archived with restic yet)", bucket, b.name, recPath)
	case err != nil:
		return restic.Location{}, err
	}
	if strings.HasPrefix(rec.Repository, "s3:") {
		return restic.Location{}, fmt.Errorf("the recovery record %s names the repository %s on S3 settings this build no longer has; use restic with that repository directly", recPath, rec.Repository)
	}
	pw, err := fsPassword(rec, filepath.Join(sys, "keys", id))
	if err != nil {
		return restic.Location{}, err
	}
	return restic.Location{Repository: filepath.Join(sys, "restic", id), Password: pw}, nil
}

// fsBucketID returns the ID of bucket in the metadata database of the fs
// backing whose system directory is sys (errNotHere when it has no such
// bucket). The database is opened read-only; a server holding it makes
// this fail.
func fsBucketID(sys, bucket string) (string, error) {
	path := filepath.Join(sys, "meta.db")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no gos3 metadata database at %s (is the backing's path right?): %w", path, err)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var id string
	err = db.View(func(tx *bbolt.Tx) error {
		bk := tx.Bucket([]byte("buckets"))
		if bk == nil {
			return errNotHere
		}
		v := bk.Get([]byte(bucket))
		if len(v) < 2 {
			return errNotHere
		}
		var rec struct {
			ID string `json:"id"`
		}
		var err error
		// One format byte, then the record (see internal/storage/fs's
		// schema): 1 JSON, 2 gob.
		switch v[0] {
		case 1:
			err = json.Unmarshal(v[1:], &rec)
		case 2:
			err = gob.NewDecoder(bytes.NewReader(v[1:])).Decode(&rec)
		default:
			err = fmt.Errorf("unknown record format %d", v[0])
		}
		if err != nil || rec.ID == "" {
			return fmt.Errorf("%s: unreadable record of bucket %q: %v", path, bucket, err)
		}
		id = rec.ID
		return nil
	})
	return id, err
}

// fsRecordsNamed returns the IDs of the recovery records of sys/keys that
// name bucket, sorted.
func fsRecordsNamed(sys, bucket string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(sys, "keys", "*.restic.json"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range paths {
		rec, err := readFSRecord(p)
		if err != nil {
			continue
		}
		if rec.Bucket == bucket {
			ids = append(ids, strings.TrimSuffix(filepath.Base(p), ".restic.json"))
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// openReadOnly opens the bbolt database at path read-only, failing fast
// when a server holds it.
func openReadOnly(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: lockTimeout})
	if errors.Is(err, berrors.ErrTimeout) {
		return nil, errServerRunning(path)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return db, nil
}

// --- s3 backings ---

// s3Container is a remote location of an s3 backing holding wrapped
// buckets (and their restic records).
type s3Container struct {
	id, bucket, prefix, region string
}

// s3Client returns a client of the s3 backing settings c.
func s3Client(c config.S3Config) (*s3client.Client, error) {
	return s3client.New(s3client.Config{
		Endpoint: c.Endpoint, Region: c.Region, AccessKey: c.AccessKey, SecretKey: c.SecretKey, PathStyle: c.PathStyle(),
	})
}

// s3Repository returns how restic reaches the repository rel ("restic/<id>"
// or "restic-aux/<name>") of container ct with the settings c; region is
// the container's signing region (that of its client).
func s3Repository(c config.S3Config, ct s3Container, region, rel, password string) restic.Location {
	path := strings.Trim(ct.prefix, "/")
	if path != "" {
		path += "/"
	}
	return restic.S3Repository{
		Endpoint: c.Endpoint, Bucket: ct.bucket, Path: path + rel, Region: region,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, PathStyle: c.PathStyle(),
	}.Location(password, "")
}

// s3KEK returns the key-encryption key of the s3 backing b; without one the
// backing keeps no restic repositories.
func s3KEK(b offlineBacking) ([]byte, error) {
	kek, err := b.s3.KEK()
	if err != nil {
		return nil, fmt.Errorf("backing %q: %w", b.name, err)
	}
	if len(kek) == 0 {
		return nil, fmt.Errorf("%w: s3 backing %q has no encryption_key (set it, e.g. with GOS3_BACKING_%s_ENCRYPTION_KEY or GOS3_S3_ENCRYPTION_KEY for the default backing): without it no restic repository of the backing can be opened",
			errNotHere, b.name, strings.ToUpper(strings.ReplaceAll(b.name, "-", "_")))
	}
	return kek, nil
}

// s3Archive finds bucket's archive on the s3 backing b.
func (o *offlineSource) s3Archive(ctx context.Context, b offlineBacking, bucket string) (restic.Location, error) {
	if b.s3 == nil {
		return restic.Location{}, fmt.Errorf("%w: backing %q has no s3 settings", errNotHere, b.name)
	}
	c := *b.s3
	kek, err := s3KEK(b)
	if err != nil {
		return restic.Location{}, err
	}
	client, err := s3Client(c)
	if err != nil {
		return restic.Location{}, fmt.Errorf("backing %q: %w", b.name, err)
	}
	containers, err := o.s3Containers(b, bucket)
	if err != nil {
		return restic.Location{}, err
	}
	type match struct {
		ct        s3Container
		rec       s3store.ResticRecord
		backingID string
	}
	var matches []match
	for _, ct := range containers {
		cl := client.ForRegion(ct.region)
		m, _, err := s3store.ReadMarker(ctx, cl, ct.bucket, ct.prefix)
		if s3client.IsNotFound(err) {
			fmt.Fprintf(o.notes, "note: backing %q: no gos3 container at %s/%s\n", b.name, ct.bucket, ct.prefix)
			continue
		}
		if err != nil {
			return restic.Location{}, fmt.Errorf("backing %q: reading the container at %s/%s: %w", b.name, ct.bucket, ct.prefix, err)
		}
		recs, err := s3store.ListResticRecords(ctx, cl, ct.bucket, ct.prefix)
		if err != nil {
			return restic.Location{}, fmt.Errorf("backing %q: %w", b.name, err)
		}
		for _, rec := range recs {
			if rec.BucketID == "" {
				continue
			}
			if o.bucketID != "" && rec.BucketID == o.bucketID || o.bucketID == "" && rec.Bucket == bucket {
				matches = append(matches, match{ct, rec, m.BackingID})
			}
		}
	}
	switch len(matches) {
	case 0:
		hint := ""
		if c.HasHome() {
			hint = " (offline, only the buckets in the backing's home are found; wrapped buckets in other containers need --server)"
		}
		return restic.Location{}, fmt.Errorf("%w: no recovery record of the s3 backing %q names bucket %q%s", errNotHere, b.name, bucket, hint)
	case 1:
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, m.rec.BucketID)
		}
		return restic.Location{}, fmt.Errorf("several recovery records of the s3 backing %q name bucket %q (IDs %s: it was deleted and created again): choose one with --bucket-id",
			b.name, bucket, strings.Join(ids, ", "))
	}
	m := matches[0]
	cl := client.ForRegion(m.ct.region)
	key, err := s3store.ReadResticKey(ctx, cl, m.ct.bucket, kek, m.backingID, m.rec)
	if err != nil {
		return restic.Location{}, fmt.Errorf("backing %q: reading the key of bucket %q: %w", b.name, bucket, err)
	}
	return s3Repository(c, m.ct, cl.Region(), "restic/"+m.rec.BucketID, restic.PasswordFromKey(key)), nil
}

// s3Containers returns the containers of the s3 backing b that may hold
// bucket: the home of a backing with a home (the only container known
// offline); for a backing without one, the container its local registry
// names for the bucket, else all of its containers.
func (o *offlineSource) s3Containers(b offlineBacking, bucket string) ([]s3Container, error) {
	c := b.s3
	if c.HasHome() {
		return []s3Container{{id: "home", bucket: c.HomeBucket, prefix: c.HomePrefix, region: c.Region}}, nil
	}
	if o.sysDir == "" {
		return nil, fmt.Errorf("%w: s3 backing %q keeps its records in the system directory, and the configuration has none (storage.path)", errNotHere, b.name)
	}
	path := filepath.Join(server.S3RecordsDir(o.sysDir, b.name), "state.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: s3 backing %q has no records at %s", errNotHere, b.name, path)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var all []s3Container
	named := -1
	err = db.View(func(tx *bbolt.Tx) error {
		// The registry of internal/storage/s3account: namespace
		// "account", containers k/<id>/, buckets b/<name>/.
		ns := tx.Bucket([]byte("account"))
		if ns == nil {
			return nil
		}
		var kind, container string
		if v := ns.Get([]byte("b/" + bucket + "/")); v != nil {
			var rec struct {
				Kind      string `json:"kind"`
				Container string `json:"container"`
			}
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("%s: unreadable record of bucket %q: %v", path, bucket, err)
			}
			kind, container = rec.Kind, rec.Container
			if kind == "native" {
				return fmt.Errorf("bucket %q is a native bucket of the s3 backing %q: it has no restic archive (objects are stored as they are)", bucket, b.name)
			}
		}
		return ns.ForEach(func(k, v []byte) error {
			id, ok := strings.CutPrefix(string(k), "k/")
			if !ok {
				return nil
			}
			var rec struct {
				Bucket, Prefix, Region string
			}
			if err := json.Unmarshal(v, &rec); err != nil || rec.Bucket == "" {
				return nil
			}
			ct := s3Container{id: strings.TrimSuffix(id, "/"), bucket: rec.Bucket, prefix: rec.Prefix, region: rec.Region}
			all = append(all, ct)
			if kind != "" && ct.id == container {
				named = len(all) - 1
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if named >= 0 {
		return []s3Container{all[named]}, nil
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: s3 backing %q has no containers", errNotHere, b.name)
	}
	return all, nil
}

// --- whole-bucket snapshots ---

// validAuxName matches the names of aux repositories.
var validAuxName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString

// snapshots finds the repository of bucket's whole-bucket snapshots from
// the snapshot scheduler's state.
func (o *offlineSource) snapshots(ctx context.Context, bucket string) (resticRepo, error) {
	if o.sysDir == "" {
		return resticRepo{}, errors.New("the configuration has no system directory (storage.path), where the snapshot schedules' state is kept")
	}
	path := filepath.Join(o.sysDir, "restic-snapshots.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return resticRepo{}, fmt.Errorf("reading the snapshot scheduler's state: %w", err)
	}
	var f struct {
		Version int `json:"version"`
		Buckets map[string]struct {
			Target *struct {
				Kind    string `json:"kind"`
				Backing string `json:"backing"`
				Repo    string `json:"repo"`
			} `json:"target"`
			AuxRepo string `json:"auxRepo"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return resticRepo{}, fmt.Errorf("%s: %v", path, err)
	}
	if f.Version != 1 {
		return resticRepo{}, fmt.Errorf("%s has format version %d; this build reads version 1", path, f.Version)
	}
	st, ok := f.Buckets[bucket]
	if !ok || st.Target == nil {
		return resticRepo{}, fmt.Errorf("no whole-bucket snapshot schedule of bucket %q is recorded in %s", bucket, path)
	}
	switch t := st.Target; t.Kind {
	case "bucket":
		// The bucket's own repository, on its primary.
		return o.archive(ctx, bucket, "")
	case "backing":
		repo := t.Repo
		if repo == "" {
			repo = st.AuxRepo
		}
		if !validAuxName(repo) {
			return resticRepo{}, fmt.Errorf("%s: bucket %q: invalid repository name %q", path, bucket, repo)
		}
		b, err := o.backing(t.Backing)
		if err != nil {
			return resticRepo{}, err
		}
		loc, err := o.auxRepository(ctx, b, repo)
		if err != nil {
			return resticRepo{}, err
		}
		return resticRepo{loc: loc, backing: b.name}, nil
	default:
		return resticRepo{}, fmt.Errorf("%s: bucket %q: unknown snapshot target %q", path, bucket, t.Kind)
	}
}

// auxRepository finds the aux repository name of backing b.
func (o *offlineSource) auxRepository(ctx context.Context, b offlineBacking, name string) (restic.Location, error) {
	switch b.backend {
	case config.BackendFS:
		sys := fsSystemDir(b.path)
		recPath := filepath.Join(sys, "keys", "aux", name+".restic.json")
		rec, err := readFSRecord(recPath)
		if err != nil {
			return restic.Location{}, fmt.Errorf("the snapshot repository %q of backing %q: %w", name, b.name, err)
		}
		pw, err := fsPassword(rec, filepath.Join(sys, "keys", "aux", name))
		if err != nil {
			return restic.Location{}, err
		}
		return restic.Location{Repository: filepath.Join(sys, "restic-aux", name), Password: pw}, nil
	case config.BackendS3:
		if b.s3 == nil || !b.s3.HasHome() {
			return restic.Location{}, fmt.Errorf("s3 backing %q has no home, where aux repositories are kept", b.name)
		}
		kek, err := s3KEK(b)
		if err != nil {
			return restic.Location{}, err
		}
		c := *b.s3
		client, err := s3Client(c)
		if err != nil {
			return restic.Location{}, fmt.Errorf("backing %q: %w", b.name, err)
		}
		home := s3Container{id: "home", bucket: c.HomeBucket, prefix: c.HomePrefix, region: c.Region}
		m, _, err := s3store.ReadMarker(ctx, client, home.bucket, home.prefix)
		if err != nil {
			return restic.Location{}, fmt.Errorf("backing %q: reading its home: %w", b.name, err)
		}
		recs, err := s3store.ListResticRecords(ctx, client, home.bucket, home.prefix)
		if err != nil {
			return restic.Location{}, fmt.Errorf("backing %q: %w", b.name, err)
		}
		for _, rec := range recs {
			if rec.Aux != name {
				continue
			}
			key, err := s3store.ReadResticKey(ctx, client, home.bucket, kek, m.BackingID, rec)
			if err != nil {
				return restic.Location{}, fmt.Errorf("backing %q: reading the key of the snapshot repository %q: %w", b.name, name, err)
			}
			return s3Repository(c, home, client.Region(), "restic-aux/"+name, restic.PasswordFromKey(key)), nil
		}
		return restic.Location{}, fmt.Errorf("the home of s3 backing %q has no recovery record of the snapshot repository %q", b.name, name)
	}
	return restic.Location{}, fmt.Errorf("backing %q keeps no restic repositories", b.name)
}
