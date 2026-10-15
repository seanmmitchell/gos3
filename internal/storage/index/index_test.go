package index

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// testBucket returns a bucket whose stamps come from clock at a fixed,
// non-advancing time (so stamps are consecutive).
func testBucket(clock *Clock, versioning storage.VersioningStatus) *Bucket[string] {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return NewBucket[string](storage.BucketInfo{Name: "bkt", Versioning: versioning},
		func() (uint64, time.Time) { return clock.Next(now) })
}

// state is a comparable dump of a bucket's versions.
type state map[string][]Version[string]

func dump(t *testing.T, b *Bucket[string]) state {
	t.Helper()
	out := state{}
	for key, obj := range b.Objects() {
		if len(obj.versions) == 0 {
			t.Fatalf("key %q kept without versions", key)
		}
		imported := 0
		for i, v := range obj.versions {
			if i > 0 && obj.versions[i-1].Stamp >= v.Stamp {
				t.Fatalf("versions of %q not in stamp order", key)
			}
			if v.Imported {
				imported++
			}
			out[key] = append(out[key], *v)
		}
		if imported != obj.imported {
			t.Fatalf("imported count of %q = %d, want %d", key, obj.imported, imported)
		}
	}
	return out
}

// decode simulates journaling a change: the versions are copied, so the
// replayed index shares nothing with the original.
func decode(c Change[string]) Change[string] {
	cp := func(v *Version[string]) *Version[string] {
		if v == nil {
			return nil
		}
		n := *v
		n.Meta = CloneMeta(v.Meta)
		return &n
	}
	out := Change[string]{Key: c.Key, Added: cp(c.Added), Updated: cp(c.Updated)}
	for _, r := range c.Removed {
		// A journal only needs the stamp of a removed version.
		out.Removed = append(out.Removed, &Version[string]{Stamp: r.Stamp})
	}
	return out
}

// randomOp runs one random operation on b and returns its change and error.
func randomOp(rng *rand.Rand, b *Bucket[string], keys []string, step int) (Change[string], error) {
	key := keys[rng.IntN(len(keys))]
	// A version ID of key: existing, "null", foreign or unknown.
	versionID := func() string {
		if obj, ok := b.Object(key); ok && rng.IntN(4) > 0 {
			return obj.versions[rng.IntN(len(obj.versions))].ID
		}
		return []string{storage.NullVersionID, "foreign-" + strconv.Itoa(rng.IntN(3)), NewVersionID(uint64(rng.IntN(1 << 20)))}[rng.IntN(3)]
	}
	data := "data-" + strconv.Itoa(step)
	switch rng.IntN(8) {
	case 0, 1:
		opts := storage.CommitOptions{IfNoneMatch: rng.IntN(6) == 0}
		if rng.IntN(6) == 0 {
			opts.IfMatch = []string{"*", `"etag-1"`, "etag-2"}[rng.IntN(3)]
		}
		if rng.IntN(3) == 0 {
			opts.BeforePublish = func(meta, current *storage.ObjectMeta, created time.Time, config func(string) []byte) error {
				if rng.IntN(5) == 0 {
					return errors.New("refused")
				}
				meta.ContentType = string(config("ct"))
				meta.Encryption = "hook" // must not stick
				if current != nil {
					meta.UserMeta = map[string]string{"prev": current.ETag}
				}
				return nil
			}
		}
		meta := storage.ObjectMeta{ETag: "etag-" + strconv.Itoa(rng.IntN(3)), Tags: map[string]string{"s": data}}
		if rng.IntN(2) == 0 {
			meta.Encryption = storage.EncryptionAES256
		}
		c := Content[string]{Data: data, Size: int64(len(data))}
		if rng.IntN(3) == 0 {
			c.Compression, c.StoredSize = storage.CompressionDeflate, 3
		}
		_, ch, err := b.Commit(key, c, meta, opts)
		return ch, err
	case 2:
		vid := ""
		if rng.IntN(2) == 0 {
			vid = versionID()
		}
		_, ch, err := b.Delete(key, vid)
		return ch, err
	case 3:
		vid := ""
		if rng.IntN(2) == 0 {
			vid = versionID()
		}
		_, ch, err := b.DeleteIf(key, vid, func(storage.ObjectInfo, bool) error {
			if rng.IntN(3) == 0 {
				return storage.ErrPreconditionFailed
			}
			return nil
		}, rng.IntN(2) == 0)
		return ch, err
	case 4:
		vid := ""
		if rng.IntN(2) == 0 {
			vid = versionID()
		}
		_, ch, err := b.UpdateMeta(key, vid, nil, func(m *storage.ObjectMeta) error {
			if rng.IntN(5) == 0 {
				return errors.New("refused")
			}
			m.Tags = map[string]string{"updated": strconv.Itoa(step)}
			m.Encryption = "changed" // must not stick
			return nil
		})
		return ch, err
	case 5, 6:
		info := storage.ObjectInfo{
			VersionID:    versionID(),
			DeleteMarker: rng.IntN(4) == 0,
			LastModified: time.Unix(int64(rng.IntN(1000)), 0),
			ObjectMeta:   storage.ObjectMeta{ETag: "imp", Encryption: storage.EncryptionAES256},
		}
		c := Content[string]{Data: data, Size: int64(len(data))}
		if info.DeleteMarker {
			c = Content[string]{}
		}
		_, ch, err := b.Import(key, info, c)
		return ch, err
	default:
		b.Info.Versioning = []storage.VersioningStatus{
			storage.VersioningUnversioned, storage.VersioningEnabled, storage.VersioningSuspended,
		}[rng.IntN(3)]
		return Change[string]{}, nil
	}
}

// TestReplayChanges checks that applying the changes recorded for a random
// sequence of operations to a fresh index rebuilds the same state, that
// failed operations change nothing, and that a snapshot loaded with Put
// (in any order) followed by the remaining changes does too.
func TestReplayChanges(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 7))
			var clock Clock
			b := testBucket(&clock, storage.VersioningEnabled)
			b.Configs["ct"] = []byte("text/plain")
			replay := NewBucket[string](storage.BucketInfo{Name: "bkt"}, nil)
			keys := []string{"a", "b", "c", "d/e"}

			var log []Change[string]
			var snapshot state
			var removed, updated, imported, markers, failed int
			snapAt := 300
			for step := 0; step < 600; step++ {
				before := dump(t, b)
				ch, err := randomOp(rng, b, keys, step)
				if err != nil {
					failed++
					if !ch.Empty() {
						t.Fatalf("step %d: failed operation returned a change: %+v", step, ch)
					}
					if after := dump(t, b); !reflect.DeepEqual(before, after) {
						t.Fatalf("step %d: failed operation (%v) changed the index", step, err)
					}
					continue
				}
				removed += len(ch.Removed)
				if ch.Updated != nil {
					updated++
				}
				if ch.Added != nil && ch.Added.Imported {
					imported++
				}
				if ch.Added != nil && ch.Added.DeleteMarker {
					markers++
				}
				d := decode(ch)
				log = append(log, d)
				replay.Apply(d)
				if got, want := dump(t, replay), dump(t, b); !reflect.DeepEqual(got, want) {
					t.Fatalf("step %d: replayed state differs:\n got %+v\nwant %+v", step, got, want)
				}
				if snapshot == nil && step >= snapAt {
					snapshot = dump(t, b)
					log = log[:0]
				}
			}

			if removed == 0 || updated == 0 || imported == 0 || markers == 0 || failed == 0 {
				t.Fatalf("weak coverage: removed %d updated %d imported %d markers %d failed %d",
					removed, updated, imported, markers, failed)
			}

			// Load the snapshot in random order, then replay the rest.
			loaded := NewBucket[string](storage.BucketInfo{Name: "bkt"}, nil)
			type kv struct {
				key string
				v   Version[string]
			}
			var all []kv
			// Sorted first, so that the shuffle (and any failure) depends
			// on the seed only, not on map iteration order.
			for _, key := range slices.Sorted(maps.Keys(snapshot)) {
				for _, v := range snapshot[key] {
					all = append(all, kv{key, v})
				}
			}
			rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
			var restored Clock
			for _, e := range all {
				v := e.v
				if old := loaded.Put(e.key, &v); old != nil {
					t.Fatalf("Put replaced a version of %q", e.key)
				}
				restored.Observe(v.Stamp)
			}
			for _, c := range log {
				loaded.Apply(c)
				if c.Added != nil {
					restored.Observe(c.Added.Stamp)
				}
			}
			if got, want := dump(t, loaded), dump(t, b); !reflect.DeepEqual(got, want) {
				t.Fatal("snapshot + replay differs from the live index")
			}
			// The clock restored from the stamps continues above every stamp
			// in the index.
			var maxStamp uint64
			for _, obj := range b.Objects() {
				for _, v := range obj.versions {
					maxStamp = max(maxStamp, v.Stamp)
				}
			}
			if restored.Last() < maxStamp || restored.Last() > clock.Last() {
				t.Fatalf("restored clock %d, max stamp %d, live clock %d", restored.Last(), maxStamp, clock.Last())
			}
			if s, _ := restored.Next(time.Unix(0, 0)); s <= maxStamp {
				t.Fatalf("next stamp after restore = %d, not above %d", s, maxStamp)
			}
		})
	}
}

func TestCommitChanges(t *testing.T) {
	var clock Clock
	b := testBucket(&clock, storage.VersioningUnversioned)
	put := func(data string, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, Change[string]) {
		t.Helper()
		info, ch, err := b.Commit("k", Content[string]{Data: data, Size: int64(len(data))}, meta, opts)
		if err != nil {
			t.Fatal(err)
		}
		return info, ch
	}

	// Unversioned: the null version is replaced, and reported removed.
	info, ch := put("one", storage.ObjectMeta{ETag: "e1"}, storage.CommitOptions{})
	if info.VersionID != storage.NullVersionID || len(ch.Removed) != 0 || ch.Added == nil || ch.Added.Data != "one" {
		t.Fatalf("first commit: %+v %+v", info, ch)
	}
	first := ch.Added
	_, ch = put("two", storage.ObjectMeta{ETag: "e2"}, storage.CommitOptions{})
	if len(ch.Removed) != 1 || ch.Removed[0] != first || ch.Added.Stamp <= first.Stamp {
		t.Fatalf("overwrite: %+v", ch)
	}

	// Enabled: versions are kept; IDs embed the stamp.
	b.Info.Versioning = storage.VersioningEnabled
	info, ch = put("three", storage.ObjectMeta{ETag: "e3"}, storage.CommitOptions{IfMatch: `"e2"`})
	if s, ok := StampFromVersionID(info.VersionID); !ok || s != ch.Added.Stamp || len(ch.Removed) != 0 {
		t.Fatalf("versioned commit: %+v %+v", info, ch)
	}

	// Preconditions fail before a stamp is taken.
	last := clock.Last()
	if _, ch, err := b.Commit("k", Content[string]{}, storage.ObjectMeta{}, storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) || !ch.Empty() {
		t.Fatalf("If-None-Match: %v %+v", err, ch)
	}
	if clock.Last() != last {
		t.Fatal("failed precondition took a stamp")
	}

	// BeforePublish sees the current metadata, the creation time of the
	// stamp and the configs; it cannot change Encryption.
	b.Configs["c"] = []byte("v")
	var created time.Time
	info, ch = put("four", storage.ObjectMeta{ETag: "e4", Encryption: storage.EncryptionAES256}, storage.CommitOptions{
		BeforePublish: func(meta, current *storage.ObjectMeta, at time.Time, config func(string) []byte) error {
			if current == nil || current.ETag != "e3" || string(config("c")) != "v" {
				return fmt.Errorf("hook saw %+v", current)
			}
			created = at
			meta.ContentType = "set/by-hook"
			meta.Encryption = ""
			return nil
		},
	})
	if info.ContentType != "set/by-hook" || info.Encryption != storage.EncryptionAES256 || !info.LastModified.Equal(created) || ch.Added.Stamp != clock.Last() {
		t.Fatalf("hook commit: %+v", info)
	}

	// Suspended: a delete marker takes the null ID and replaces the null
	// version ("two").
	b.Info.Versioning = storage.VersioningSuspended
	res, ch, err := b.Delete("k", "")
	if err != nil || !res.DeleteMarker || res.VersionID != storage.NullVersionID || len(ch.Removed) != 1 || ch.Removed[0].Data != "two" {
		t.Fatalf("suspended delete: %+v %+v %v", res, ch, err)
	}
	obj, _ := b.Object("k")
	if n := len(obj.Versions()); n != 3 || !obj.Latest().DeleteMarker {
		t.Fatalf("versions after delete: %d", n)
	}

	// Deleting every version removes the key.
	for _, v := range slices.Clone(obj.Versions()) {
		if _, ch, err := b.Delete("k", v.ID); err != nil || len(ch.Removed) != 1 || ch.Removed[0] != v {
			t.Fatalf("delete %s: %+v %v", v.ID, ch, err)
		}
	}
	if _, ok := b.Object("k"); ok || b.Len() != 0 {
		t.Fatal("key kept without versions")
	}
}

func TestUpdateMetaCopyOnWrite(t *testing.T) {
	var clock Clock
	b := testBucket(&clock, storage.VersioningEnabled)
	_, ch, err := b.Commit("k", Content[string]{Data: "x", Size: 1}, storage.ObjectMeta{ETag: "e", Encryption: "enc"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	orig := ch.Added
	info, ch, err := b.UpdateMeta("k", "", func(o storage.ObjectInfo) error {
		if o.ETag != "e" || !o.IsLatest {
			return errors.New("bad check info")
		}
		return nil
	}, func(m *storage.ObjectMeta) error {
		m.ContentType = "new"
		m.Encryption = ""
		return nil
	})
	if err != nil || info.ContentType != "new" || info.Encryption != "enc" {
		t.Fatalf("update: %+v %v", info, err)
	}
	if ch.Updated == nil || ch.Updated.Stamp != orig.Stamp || ch.Updated.Meta.ContentType != "new" || ch.Added != nil || len(ch.Removed) != 0 {
		t.Fatalf("update change: %+v", ch)
	}
	if orig.Meta.ContentType != "" {
		t.Fatal("a version handed out earlier was modified")
	}
	v, _, _ := b.Find("k", "")
	if v == ch.Updated || v.Meta.ContentType != "new" || v.Data != "x" {
		t.Fatalf("index version %+v", v)
	}

	// Delete markers cannot be updated.
	if _, _, err := b.Delete("k", ""); err != nil {
		t.Fatal(err)
	}
	if _, ch, err := b.UpdateMeta("k", "", nil, func(*storage.ObjectMeta) error { return nil }); !errors.Is(err, storage.ErrObjectNotFound) || !ch.Empty() {
		t.Fatalf("update of a delete marker: %v", err)
	}
}

func TestImportReplacesByID(t *testing.T) {
	var clock Clock
	b := testBucket(&clock, storage.VersioningEnabled)
	imp := func(id string, data string) Change[string] {
		t.Helper()
		_, ch, err := b.Import("k", storage.ObjectInfo{VersionID: id, LastModified: time.Unix(5, 0)},
			Content[string]{Data: data, Size: int64(len(data))})
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	a := imp("foreign-a", "1")
	// An ID shaped like a generated one, whose stamp means nothing here.
	other := NewVersionID(1)
	imp(other, "2")
	imp(storage.NullVersionID, "3")
	ch := imp("foreign-a", "4")
	if len(ch.Removed) != 1 || ch.Removed[0] != a.Added || !ch.Added.Imported {
		t.Fatalf("reimport: %+v", ch)
	}
	obj, _ := b.Object("k")
	var ids []string
	for _, v := range obj.Versions() {
		ids = append(ids, v.ID)
	}
	if !slices.Equal(ids, []string{other, storage.NullVersionID, "foreign-a"}) || obj.imported != 2 {
		t.Fatalf("ids %v, imported %d", ids, obj.imported)
	}
	if v, latest, err := b.Find("k", other); err != nil || v.Data != "2" || latest {
		t.Fatalf("find imported generated-shape ID: %+v %v", v, err)
	}
	if _, _, err := b.Import("k", storage.ObjectInfo{VersionID: ""}, Content[string]{}); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("empty ID: %v", err)
	}
	if _, _, err := b.Import("k", storage.ObjectInfo{VersionID: "x", DeleteMarker: true}, Content[string]{Size: 1}); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("delete marker with data: %v", err)
	}
}

func TestDeleteIf(t *testing.T) {
	var clock Clock
	b := testBucket(&clock, storage.VersioningEnabled)
	var calls []bool
	check := func(_ storage.ObjectInfo, exists bool) error {
		calls = append(calls, exists)
		return nil
	}
	// Absent key: consulted only with absentOK.
	if _, _, err := b.DeleteIf("k", "", check, false); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("absent: %v", err)
	}
	res, ch, err := b.DeleteIf("k", "", check, true)
	if err != nil || !res.DeleteMarker || ch.Added == nil || !slices.Equal(calls, []bool{false}) {
		t.Fatalf("absent with absentOK: %+v %v %v", res, err, calls)
	}
	refuse := errors.New("no")
	if _, ch, err := b.DeleteIf("k", res.VersionID, func(o storage.ObjectInfo, _ bool) error {
		if !o.DeleteMarker || !o.IsLatest {
			return errors.New("bad info")
		}
		return refuse
	}, false); !errors.Is(err, refuse) || !ch.Empty() {
		t.Fatalf("refused: %v", err)
	}
}

func TestClock(t *testing.T) {
	var c Clock
	at := time.Unix(100, 0)
	s1, now := c.Next(at)
	if s1 != uint64(at.UnixNano()) || now.Location() != time.UTC {
		t.Fatalf("first stamp %d %v", s1, now)
	}
	if s2, _ := c.Next(at.Add(-time.Hour)); s2 != s1+1 {
		t.Fatalf("clock going back: %d", s2)
	}
	c.Observe(s1 + 100)
	c.Observe(s1) // lower stamps are ignored
	if c.Last() != s1+100 {
		t.Fatalf("Last = %d", c.Last())
	}
	if s, _ := c.Next(time.Unix(-5, 0)); s != s1+101 {
		t.Fatalf("negative time: %d", s)
	}
}

func TestWalksRunUnlocked(t *testing.T) {
	var clock Clock
	b := testBucket(&clock, storage.VersioningEnabled)
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("k%03d", i)
		for j := 0; j < 3; j++ {
			if _, _, err := b.Commit(key, Content[string]{}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		b.AddUpload(storage.UploadInfo{UploadID: b.NewUploadID(), Key: key})
	}
	locked, acquired := false, 0
	acquire := func() (*Bucket[string], func(), error) {
		if locked {
			t.Fatal("acquired twice")
		}
		locked = true
		acquired++
		return b, func() { locked = false }, nil
	}
	var objects, versions, uploads int
	ctx := context.Background()
	if err := WalkObjects(ctx, acquire, "", "", func(storage.ObjectInfo) error {
		if locked {
			t.Fatal("fn called with the lock held")
		}
		objects++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WalkVersions(ctx, acquire, "", "", "", func(storage.ObjectInfo) error {
		if locked {
			t.Fatal("fn called with the lock held")
		}
		versions++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WalkUploads(ctx, acquire, "", storage.UploadPosition{}, func(storage.UploadInfo) error {
		if locked {
			t.Fatal("fn called with the lock held")
		}
		uploads++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if objects != 100 || versions != 300 || uploads != 100 || acquired < 6 {
		t.Fatalf("objects %d versions %d uploads %d acquired %d", objects, versions, uploads, acquired)
	}
	// Errors of acquire end the walk.
	gone := errors.New("gone")
	if err := WalkObjects(ctx, func() (*Bucket[string], func(), error) { return nil, nil, gone }, "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, gone) {
		t.Fatalf("acquire error: %v", err)
	}
}

func TestUploads(t *testing.T) {
	var clock Clock
	b := testBucket(&clock, storage.VersioningUnversioned)
	t0 := time.Unix(10, 0)
	u := b.AddUpload(storage.UploadInfo{UploadID: "u1", Key: "k", Initiated: t0, Meta: storage.ObjectMeta{Tags: map[string]string{"a": "b"}}})
	b.AddUpload(storage.UploadInfo{UploadID: "u0", Key: "k", Initiated: t0.Add(time.Second)})
	b.AddUpload(storage.UploadInfo{UploadID: "u2", Key: "j", Initiated: t0.Add(time.Hour)})
	var order []string
	for _, x := range b.Uploads() {
		order = append(order, x.Info.UploadID)
	}
	if !slices.Equal(order, []string{"u2", "u1", "u0"}) {
		t.Fatalf("order %v", order)
	}
	if _, err := b.Upload("other", "u1"); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("upload of another key: %v", err)
	}
	p1 := &Part[string]{Info: storage.PartInfo{PartNumber: 2, PartMeta: storage.PartMeta{ETag: "a"}}, Data: "x"}
	if u.PutPart(p1) != nil {
		t.Fatal("new part replaced something")
	}
	u.PutPart(&Part[string]{Info: storage.PartInfo{PartNumber: 1}, Data: "y"})
	if old := u.PutPart(&Part[string]{Info: storage.PartInfo{PartNumber: 2, PartMeta: storage.PartMeta{ETag: "b"}}, Data: "z"}); old != p1 {
		t.Fatal("replaced part not returned")
	}
	if ps := u.Parts(); len(ps) != 2 || ps[0].Info.PartNumber != 1 || ps[1].Data != "z" {
		t.Fatalf("parts %+v", ps)
	}
	if _, err := u.SelectParts([]int{2}, []string{"a"}); !errors.Is(err, storage.ErrPartNotFound) {
		t.Fatalf("replaced part selected: %v", err)
	}
	if _, err := u.SelectParts([]int{3}, nil); !errors.Is(err, storage.ErrPartNotFound) {
		t.Fatalf("missing part selected: %v", err)
	}
	if ps, err := u.SelectParts([]int{2, 1}, []string{"b", ""}); err != nil || ps[0].Data != "z" || ps[1].Data != "y" {
		t.Fatalf("select: %v", err)
	}
	if b.RemoveUpload("u1") != u || b.RemoveUpload("u1") != nil {
		t.Fatal("RemoveUpload")
	}
	if got := b.ClearUploads(); len(got) != 2 || len(b.Uploads()) != 0 {
		t.Fatalf("ClearUploads returned %d", len(got))
	}
}
