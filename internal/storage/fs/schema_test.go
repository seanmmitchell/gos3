package fs

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestVersionKeyEncoding(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []byte{0x00, 0x01, 0x02, 'a', 'b', 0xfe, 0xff}
	randKey := func() string {
		b := make([]byte, rng.IntN(6))
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		return string(b)
	}
	type entry struct {
		key string
		inv uint64
	}
	var entries []entry
	for range 2000 {
		entries = append(entries, entry{randKey(), rng.Uint64N(4)})
	}
	for _, e := range entries {
		vk := versionKey(e.key, e.inv)
		k, inv, ok := splitVersionKey(vk)
		if !ok || k != e.key || inv != e.inv {
			t.Fatalf("roundtrip %q/%d = %q/%d/%v", e.key, e.inv, k, inv, ok)
		}
		if !bytes.HasPrefix(vk, keyVersionsPrefix(e.key)) || !bytes.HasPrefix(vk, escapeKey(nil, e.key)) {
			t.Fatalf("prefix property broken for %q", e.key)
		}
		if bytes.Compare(vk, keyVersionsEnd(e.key)) >= 0 {
			t.Fatalf("keyVersionsEnd(%q) not after its versions", e.key)
		}
	}
	// Order of encodings == (key ascending, inv ascending).
	for i := 1; i < len(entries); i++ {
		a, b := entries[i-1], entries[i]
		want := strings.Compare(a.key, b.key)
		if want == 0 {
			want = cmpU64(a.inv, b.inv)
		}
		if got := bytes.Compare(versionKey(a.key, a.inv), versionKey(b.key, b.inv)); got != want {
			t.Fatalf("order of (%q,%d) vs (%q,%d) = %d, want %d", a.key, a.inv, b.key, b.inv, got, want)
		}
		// keyVersionsEnd(a) is <= every version key of a greater key.
		if a.key < b.key && bytes.Compare(keyVersionsEnd(a.key), versionKey(b.key, b.inv)) > 0 {
			t.Fatalf("keyVersionsEnd(%q) after versions of %q", a.key, b.key)
		}
		// Prefix filtering on escaped prefixes matches string prefixes.
		if got, want := bytes.HasPrefix(versionKey(b.key, 0), escapeKey(nil, a.key)), strings.HasPrefix(b.key, a.key); got != want {
			t.Fatalf("HasPrefix(%q, %q) = %v, want %v", b.key, a.key, got, want)
		}
	}
	for _, bad := range [][]byte{nil, {'a'}, {'a', 0}, {'a', 0, 2, 1, 2, 3, 4, 5, 6, 7, 8}, {'a', 0, 1, 1}} {
		if _, _, ok := splitVersionKey(bad); ok {
			t.Errorf("splitVersionKey(%q) accepted", bad)
		}
	}
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestIdentifiers(t *testing.T) {
	seen := map[string]bool{}
	for _, inv := range []uint64{0, 1, 1 << 40, invert(1), invert(1 << 50)} {
		id := newVersionID(inv)
		if len(id) != versionIDLen || len(id) > 64 || id == storage.NullVersionID || !isURLSafe(id) || seen[id] {
			t.Fatalf("bad version ID %q", id)
		}
		seen[id] = true
		if got, ok := parseVersionID(id); !ok || got != inv {
			t.Fatalf("parseVersionID(%q) = %d, %v", id, got, ok)
		}
	}
	for _, bad := range []string{"", "null", "abc", strings.Repeat("!", versionIDLen), strings.Repeat("A", versionIDLen+1)} {
		if _, ok := parseVersionID(bad); ok {
			t.Errorf("parseVersionID(%q) accepted", bad)
		}
	}
	for range 100 {
		id := newUploadID()
		if !validUploadID(id) || seen[id] {
			t.Fatalf("bad upload ID %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "../x", strings.Repeat("A", uploadIDLen-1), strings.Repeat("/", uploadIDLen)} {
		if validUploadID(bad) {
			t.Errorf("validUploadID(%q)", bad)
		}
	}
	for _, c := range []struct {
		name string
		n    int
		ok   bool
	}{
		{"1-0123abcd", 1, true},
		{"10000-fff", 10000, true},
		{"10001-fff", 0, false},
		{"0-fff", 0, false},
		{"01-fff", 0, false},
		{"1-", 0, false},
		{"1-xyz", 0, false},
		{"garbage", 0, false},
	} {
		n, ok := parsePartFileName(c.name)
		if n != c.n || ok != c.ok {
			t.Errorf("parsePartFileName(%q) = %d, %v", c.name, n, ok)
		}
	}
	if name := partFileName(42); !strings.HasPrefix(name, "42-") {
		t.Fatalf("partFileName = %q", name)
	}
	if n, ok := parsePartFileName(partFileName(42)); !ok || n != 42 {
		t.Fatal("partFileName does not round-trip")
	}
	if validDataID("../../x") || validDataID("ab") || !validDataID(randomHex(16)) {
		t.Fatal("validDataID")
	}
}

func fullVersionRecord() versionRecord {
	return versionRecord{
		VersionID:    "AAAAAAAAAAAAAAAAAAAAAA",
		DeleteMarker: false,
		Size:         1 << 40,
		LastModified: time.Date(2026, 10, 9, 1, 2, 3, 456789, time.UTC),
		DataID:       "0123456789abcdef0123456789abcdef",
		Meta: storage.ObjectMeta{
			ETag: "abc-2", ContentType: "text/plain", ContentEncoding: "gzip",
			ContentDisposition: "attachment", ContentLanguage: "en", CacheControl: "no-cache",
			Expires: "Thu, 01 Dec 1994 16:00:00 GMT", UserMeta: map[string]string{"a": "1", "raw": "caf\xe9", "": ""},
			Tags: map[string]string{}, StorageClass: "STANDARD_IA", ACL: "public-read",
			Checksums: map[string]string{"CRC32": "AAAAAA==", "SHA256": "x"}, ChecksumType: "COMPOSITE",
			PartSizes: []int64{5 << 20, 1}, PartChecksums: []map[string]string{{"CRC32": "a"}, nil, {}},
			SSE: "aws:kms", SSEKMSKeyID: "key", RetentionMode: "GOVERNANCE",
			RetainUntil: time.Date(2030, 1, 1, 0, 0, 0, 1, time.UTC), LegalHold: true, WebsiteRedirect: "/x",
		},
	}
}

func TestRecordEncoding(t *testing.T) {
	compressed := fullVersionRecord()
	compressed.Compression, compressed.StoredSize = storage.CompressionDeflate, 12345
	for _, rec := range []versionRecord{fullVersionRecord(), {VersionID: "null", DeleteMarker: true}, {}, compressed} {
		data, err := encodeRecord(&rec)
		if err != nil {
			t.Fatal(err)
		}
		// Only compressed versions need the newer format.
		wantFormat := recordFormatVersionV1
		if rec.Compression != "" {
			wantFormat = recordFormatVersionV2
		}
		eq(t, "format", data[0], wantFormat)
		var got versionRecord
		if err := decodeRecord(data, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, rec) {
			t.Fatalf("round trip:\n got %#v\nwant %#v", got, rec)
		}
		// Every truncation and trailing garbage is rejected without panicking.
		for n := 1; n < len(data); n++ {
			if err := decodeRecord(data[:n], &got); !errors.Is(err, errCorrupt) {
				t.Fatalf("truncated at %d/%d: %v", n, len(data), err)
			}
		}
		if err := decodeRecord(append(bytes.Clone(data), 0), &got); !errors.Is(err, errCorrupt) {
			t.Fatalf("trailing byte: %v", err)
		}
	}

	// Other records use JSON.
	up := uploadRecord{Key: "k", Initiated: time.Unix(5, 0).UTC(), Meta: storage.ObjectMeta{
		ETag: "e", UserMeta: map[string]string{"<html>": "&"}, PartSizes: []int64{}, Tags: nil,
	}}
	data, err := encodeRecord(&up)
	if err != nil {
		t.Fatal(err)
	}
	if data[0] != recordFormatJSON || bytes.HasSuffix(data, []byte("\n")) || !bytes.Contains(data, []byte("<html>")) {
		t.Fatalf("encoding %q", data)
	}
	var gotUp uploadRecord
	if err := decodeRecord(data, &gotUp); err != nil {
		t.Fatal(err)
	}
	if gotUp.Meta.UserMeta["<html>"] != "&" || gotUp.Meta.PartSizes == nil || gotUp.Meta.Tags != nil || gotUp.Key != "k" {
		t.Fatalf("decoded %+v", gotUp)
	}
	var vr versionRecord
	for _, bad := range [][]byte{nil, {}, {9, '{', '}'}, {1, '{'}, {2, 0xff}, {3}} {
		if err := decodeRecord(bad, &vr); !errors.Is(err, errCorrupt) {
			t.Errorf("decodeRecord(%q) = %v", bad, err)
		}
	}
	if err := decodeRecord(data, &vr); err != nil {
		// JSON of an upload decodes into a version record leniently; only
		// the binary format is type-checked.
		t.Logf("cross-type decode: %v", err)
	}
	vdata, _ := encodeRecord(&versionRecord{VersionID: "x"})
	if err := decodeRecord(vdata, &gotUp); !errors.Is(err, errCorrupt) {
		t.Fatalf("version format into upload record: %v", err)
	}
}

func FuzzDecodeVersionRecord(f *testing.F) {
	full := fullVersionRecord()
	f.Add(encodeVersionRecord(&full)[1:], false)
	full.Compression, full.StoredSize = storage.CompressionDeflate, 99
	f.Add(encodeVersionRecord(&full)[1:], true)
	f.Add([]byte{}, false)
	f.Add([]byte{0, 0, 0xff, 0xff, 0xff, 0xff, 0x0f}, true)
	f.Fuzz(func(t *testing.T, data []byte, v2 bool) {
		var r versionRecord
		if err := decodeVersionRecord(data, &r, v2); err != nil {
			return
		}
		// Whatever decodes must re-encode to an equivalent record.
		var r2 versionRecord
		enc := encodeVersionRecord(&r)
		if err := decodeVersionRecord(enc[1:], &r2, enc[0] == recordFormatVersionV2); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		if !reflect.DeepEqual(r, r2) {
			t.Fatalf("unstable round trip")
		}
	})
}

func TestEtagMatches(t *testing.T) {
	for _, c := range []struct {
		cond, etag string
		ok         bool
	}{
		{"abc", "abc", true},
		{`"abc"`, "abc", true},
		{" abc ", "abc", true},
		{"*", "anything", true},
		{"abd", "abc", false},
		{`"`, `"`, true},
	} {
		if got := etagMatches(c.cond, c.etag); got != c.ok {
			t.Errorf("etagMatches(%q, %q) = %v", c.cond, c.etag, got)
		}
	}
}

func TestCommitterGroupsAndIsolatesFailures(t *testing.T) {
	b := openTest(t, "")
	waitRecovery(t, b)
	var c committer
	boom := errors.New("boom")
	var wg sync.WaitGroup
	var runs atomic.Int64
	const n = 64
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.do(b.db, func(tx *bbolt.Tx) error {
				runs.Add(1)
				if i%7 == 0 {
					return boom
				}
				bk, err := tx.CreateBucketIfNotExists([]byte("test"))
				if err != nil {
					return err
				}
				return bk.Put([]byte{byte(i)}, []byte{byte(i)})
			})
		}()
	}
	wg.Wait()
	var keys []int
	if err := b.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("test")).ForEach(func(k, v []byte) error {
			keys = append(keys, int(k[0]))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	for i, err := range errs {
		if (i%7 == 0) != errors.Is(err, boom) || (i%7 != 0 && err != nil) {
			t.Fatalf("call %d: %v", i, err)
		}
		if i%7 != 0 && !slices.Contains(keys, i) {
			t.Fatalf("call %d not committed", i)
		}
	}
	if slices.ContainsFunc(keys, func(k int) bool { return k%7 == 0 }) {
		t.Fatal("failed call committed")
	}

	// A panicking function surfaces in its caller, not in the commit loop.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_ = c.do(b.db, func(*bbolt.Tx) error { panic("kaboom") })
	}()
	if err := c.do(b.db, func(*bbolt.Tx) error { return nil }); err != nil {
		t.Fatalf("committer broken after panic: %v", err)
	}
}

func TestRecordEncodingNonUTF8(t *testing.T) {
	rec := uploadRecord{Key: "k", Meta: storage.ObjectMeta{
		ETag:          "e",
		UserMeta:      map[string]string{"raw": "caf\xe9", "ok": "x"},
		Tags:          map[string]string{"\xff": "v"},
		PartChecksums: []map[string]string{{"CRC32": "x"}},
		RetainUntil:   time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC),
	}}
	data, err := encodeRecord(&rec)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "format", data[0], recordFormatGob)
	var got uploadRecord
	if err := decodeRecord(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Meta.UserMeta["raw"] != "caf\xe9" || got.Meta.Tags["\xff"] != "v" || !got.Meta.RetainUntil.Equal(rec.Meta.RetainUntil) || got.Meta.PartChecksums[0]["CRC32"] != "x" {
		t.Fatalf("decoded %+v", got.Meta)
	}

	// End to end through the backend.
	b, bkt := newBucket(t, "")
	w, _ := b.NewObjectWriter(ctx, bkt, "k")
	w.Write([]byte("x"))
	if _, err := w.Commit(ctx, rec.Meta, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := b.GetObjectInfo(ctx, bkt, "k", "")
	if err != nil || info.UserMeta["raw"] != "caf\xe9" || info.Tags["\xff"] != "v" {
		t.Fatalf("stored meta %+v, %v", info.ObjectMeta, err)
	}
	up, err := b.CreateMultipartUpload(ctx, bkt, "mp", rec.Meta, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetMultipartUpload(ctx, bkt, "mp", up.UploadID); err != nil || got.Meta.UserMeta["raw"] != "caf\xe9" {
		t.Fatalf("upload meta %+v, %v", got.Meta, err)
	}
	keys := keysOf(t, b, bkt, "", "")
	eq(t, "walk", len(keys), 1)
}

// TestCommitterPanicInSharedBatch: a function panicking inside a shared
// transaction must not strand the other calls of its batch.
func TestCommitterPanicInSharedBatch(t *testing.T) {
	b := openTest(t, "")
	var c committer
	release := make(chan struct{})
	started := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- c.do(b.db, func(*bbolt.Tx) error {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			return nil
		})
	}()
	<-started // the loop is busy; the next calls queue into one batch

	const n = 4
	results := make(chan string, n)
	for i := range n {
		go func() {
			defer func() {
				if p := recover(); p != nil {
					results <- "panic"
				}
			}()
			err := c.do(b.db, func(tx *bbolt.Tx) error {
				if i == 1 {
					panic("kaboom")
				}
				bk, err := tx.CreateBucketIfNotExists([]byte("shared"))
				if err != nil {
					return err
				}
				return bk.Put([]byte{byte(i)}, []byte{1})
			})
			if err != nil {
				results <- "error: " + err.Error()
				return
			}
			results <- "ok"
		}()
	}
	// Wait until all calls are queued behind the blocked one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		queued := len(c.queue)
		c.mu.Unlock()
		if queued == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d calls queued", queued)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for range n {
		select {
		case r := <-results:
			got[r]++
		case <-time.After(5 * time.Second):
			t.Fatalf("co-batched callers stranded after a panic; got %v", got)
		}
	}
	if got["panic"] != 1 || got["ok"] != n-1 {
		t.Fatalf("results = %v", got)
	}
	if err := b.db.View(func(tx *bbolt.Tx) error {
		if k := tx.Bucket([]byte("shared")).Stats().KeyN; k != n-1 {
			t.Errorf("committed keys = %d, want %d", k, n-1)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
