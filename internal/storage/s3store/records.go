package s3store

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/index"
)

// Journal records and snapshot chunks of the backend.
//
// Every mutation is journaled as one record holding its exact effect, so
// replaying the records in LSN order on the loaded snapshot rebuilds the
// state: index changes carry the versions added (with stamp, ID, metadata
// and blob reference), the stamps removed and the metadata replaced.
//
// A snapshot holds, for every bucket, a header chunk (scope "b") with the
// bucket's ID, info, configuration documents and uploads, followed by
// object chunks (scope "o", at most snapChunkKeys keys each), all with the
// LSN read under the bucket's lock while the bucket was copied; and one
// chunk (scope "a") per aux namespace (AuxState). On load, a record
// applies to a bucket (or aux namespace) only when its LSN is above that
// chunk LSN.

// Record operations.
const (
	opMkBucket   = "mkb"
	opRmBucket   = "rmb"
	opVersioning = "ver"
	opObjectLock = "olk"
	opPutConfig  = "cfg"
	opDelConfig  = "rmcfg"
	opChange     = "chg"
	opMkUpload   = "mku"
	opRmUpload   = "rmu"
	opPart       = "part"
	opAuxPut     = "aux"
	opAuxDel     = "rmaux"
	opStage      = "stg"
	opUnstage    = "rmstg"
	opKeyMark    = "keymark"
)

// snapChunkKeys is the most keys one object chunk holds.
const snapChunkKeys = 4096

// record is one journal record.
type record struct {
	Op string `json:"op"`
	// B is the bucket name (bucket records) .
	B string `json:"b,omitempty"`
	// ID is the bucket ID (opMkBucket).
	ID   string              `json:"id,omitempty"`
	Info *storage.BucketInfo `json:"info,omitempty"`
	Ver  *string             `json:"ver,omitempty"`
	Lock *bool               `json:"lock,omitempty"`
	// K is a configuration key, an aux key or prefix, or a staged name.
	K string `json:"k,omitempty"`
	// D is a configuration document or an aux value.
	D []byte     `json:"d,omitempty"`
	C *changeRec `json:"c,omitempty"`
	// RmUpload removes an upload after C (a completed upload).
	RmUpload string              `json:"rmu,omitempty"`
	U        *storage.UploadInfo `json:"u,omitempty"`
	P        *partRec            `json:"p,omitempty"`
	// NS is the aux namespace.
	NS string `json:"ns,omitempty"`
}

// changeRec is an index.Change.
type changeRec struct {
	Key     string      `json:"k"`
	Removed []uint64    `json:"rm,omitempty"`
	Added   *versionRec `json:"add,omitempty"`
	Updated *updateRec  `json:"upd,omitempty"`
}

type updateRec struct {
	Stamp uint64             `json:"s"`
	Meta  storage.ObjectMeta `json:"m"`
}

// versionRec is an index.Version.
type versionRec struct {
	Stamp        uint64              `json:"s"`
	ID           string              `json:"id"`
	DeleteMarker bool                `json:"dm,omitempty"`
	Imported     bool                `json:"imp,omitempty"`
	Meta         *storage.ObjectMeta `json:"m,omitempty"`
	LastModified time.Time           `json:"lm"`
	Size         int64               `json:"sz,omitempty"`
	Compression  string              `json:"z,omitempty"`
	StoredSize   int64               `json:"st,omitempty"`
	Data         blobRef             `json:"d"`
}

// partRec is an index.Part of an upload.
type partRec struct {
	Upload string           `json:"u"`
	Info   storage.PartInfo `json:"i"`
	Data   blobRef          `json:"d"`
}

func encodeVersion(v *index.Version[blobRef]) *versionRec {
	r := &versionRec{
		Stamp: v.Stamp, ID: v.ID, DeleteMarker: v.DeleteMarker, Imported: v.Imported,
		LastModified: v.LastModified, Size: v.Size, Compression: v.Compression,
		StoredSize: v.StoredSize, Data: v.Data,
	}
	if !v.DeleteMarker {
		m := v.Meta
		r.Meta = &m
	}
	return r
}

func (r *versionRec) version() *index.Version[blobRef] {
	v := &index.Version[blobRef]{
		Stamp: r.Stamp, ID: r.ID, DeleteMarker: r.DeleteMarker, Imported: r.Imported,
		LastModified: r.LastModified, Size: r.Size, Compression: r.Compression,
		StoredSize: r.StoredSize, Data: r.Data,
	}
	if r.Meta != nil {
		v.Meta = *r.Meta
	}
	return v
}

func encodeChange(c index.Change[blobRef]) *changeRec {
	r := &changeRec{Key: c.Key}
	for _, v := range c.Removed {
		r.Removed = append(r.Removed, v.Stamp)
	}
	if c.Added != nil {
		r.Added = encodeVersion(c.Added)
	}
	if c.Updated != nil {
		r.Updated = &updateRec{Stamp: c.Updated.Stamp, Meta: c.Updated.Meta}
	}
	return r
}

// applyChange applies a journaled change to ix.
func applyChange(ix *index.Bucket[blobRef], r *changeRec, clock *index.Clock) {
	ch := index.Change[blobRef]{Key: r.Key}
	for _, s := range r.Removed {
		ch.Removed = append(ch.Removed, &index.Version[blobRef]{Stamp: s})
	}
	if r.Added != nil {
		ch.Added = r.Added.version()
		clock.Observe(ch.Added.Stamp)
	}
	if r.Updated != nil {
		ch.Updated = &index.Version[blobRef]{Stamp: r.Updated.Stamp, Meta: r.Updated.Meta}
	}
	ix.Apply(ch)
}

// blobsOf returns the blob keys of versions (removed by a change).
func (b *Backend) blobsOf(bucketID string, vs []*index.Version[blobRef]) []string {
	var out []string
	for _, v := range vs {
		if v.Data.ID != "" {
			out = append(out, b.dataKey(bucketID, v.Data.ID))
		}
	}
	return out
}

// partBlobs returns the blob keys of an upload's parts.
func (b *Backend) partBlobs(u *index.Upload[blobRef]) []string {
	var out []string
	for _, p := range u.Parts() {
		if p.Data.ID != "" {
			out = append(out, b.partKey(u.Info.UploadID, p.Info.PartNumber, p.Data.ID))
		}
	}
	return out
}

// --- snapshot chunks ---

type bucketChunk struct {
	Name    string             `json:"b"`
	ID      string             `json:"id"`
	Info    storage.BucketInfo `json:"info"`
	Configs map[string][]byte  `json:"cfg,omitempty"`
	Uploads []uploadChunk      `json:"up,omitempty"`
	Keyed   bool               `json:"keyed,omitempty"`
}

type uploadChunk struct {
	Info  storage.UploadInfo `json:"i"`
	Parts []partRec          `json:"p,omitempty"`
}

type objectsChunk struct {
	Name    string      `json:"b"`
	Objects []objectRec `json:"o"`
}

type objectRec struct {
	Key      string        `json:"k"`
	Versions []*versionRec `json:"v"`
}

type auxChunk struct {
	NS      string            `json:"ns"`
	Records map[string][]byte `json:"r,omitempty"`
	Staged  []string          `json:"s,omitempty"`
}

type snapExtra struct {
	LastStamp uint64 `json:"lastStamp"`
}

// snapshot writes the state (EngineOptions.Snapshot).
func (b *Backend) snapshot(ctx context.Context, w SnapshotWriter) error {
	extra, _ := json.Marshal(snapExtra{LastStamp: b.clock.Last()})
	if err := w.SetExtra(extra); err != nil {
		return err
	}
	b.mu.RLock()
	st := b.st
	b.mu.RUnlock()
	if st == nil {
		return b.unavailableErr()
	}
	st.mu.RLock()
	names := slices.Sorted(maps.Keys(st.buckets))
	bks := make([]*bucket, len(names))
	for i, n := range names {
		bks[i] = st.buckets[n]
	}
	st.mu.RUnlock()

	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.snapshotBucket(w, name, bks[i]); err != nil {
			return err
		}
		if b.snapHook != nil {
			b.snapHook(name)
		}
	}

	st.auxMu.Lock()
	lsn := b.eng.LastLSN()
	var auxes []auxChunk
	for _, ns := range slices.Sorted(maps.Keys(st.aux)) {
		a := st.aux[ns]
		auxes = append(auxes, auxChunk{NS: ns, Records: maps.Clone(a.records), Staged: slices.Sorted(maps.Keys(a.staged))})
	}
	st.auxMu.Unlock()
	for _, a := range auxes {
		data, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if err := w.Chunk("a", lsn, data); err != nil {
			return err
		}
	}
	return nil
}

// snapshotBucket copies one bucket under its read lock (version pointers
// only: versions are immutable) and writes its chunks.
func (b *Backend) snapshotBucket(w SnapshotWriter, name string, bk *bucket) error {
	type keyVersions struct {
		key string
		vs  []*index.Version[blobRef]
	}
	bk.mu.RLock()
	if bk.gone() {
		bk.mu.RUnlock()
		return nil
	}
	lsn := b.eng.LastLSN()
	hdr := bucketChunk{Name: name, ID: bk.id, Info: bk.ix.Info, Configs: maps.Clone(bk.ix.Configs), Keyed: bk.keyed}
	for _, u := range bk.ix.Uploads() {
		uc := uploadChunk{Info: index.CloneUpload(u.Info)}
		for _, p := range u.Parts() {
			uc.Parts = append(uc.Parts, partRec{Upload: u.Info.UploadID, Info: p.Info, Data: p.Data})
		}
		hdr.Uploads = append(hdr.Uploads, uc)
	}
	objs := make([]keyVersions, 0, bk.ix.Len())
	for key, o := range bk.ix.Objects() {
		objs = append(objs, keyVersions{key, slices.Clone(o.Versions())})
	}
	bk.mu.RUnlock()

	data, err := json.Marshal(hdr)
	if err != nil {
		return err
	}
	if err := w.Chunk("b", lsn, data); err != nil {
		return err
	}
	for len(objs) > 0 {
		n := min(len(objs), snapChunkKeys)
		oc := objectsChunk{Name: name, Objects: make([]objectRec, n)}
		for i, kv := range objs[:n] {
			rec := objectRec{Key: kv.key, Versions: make([]*versionRec, len(kv.vs))}
			for j, v := range kv.vs {
				rec.Versions[j] = encodeVersion(v)
			}
			oc.Objects[i] = rec
		}
		objs = objs[n:]
		data, err := json.Marshal(oc)
		if err != nil {
			return err
		}
		if err := w.Chunk("o", lsn, data); err != nil {
			return err
		}
	}
	return nil
}

// --- loading ---

// loader builds the state during Engine.Open.
type loader struct {
	st *state
	// scope maps "b/"+bucket and "a/"+namespace to the LSN of their
	// snapshot chunks.
	scope map[string]uint64
}

// load is EngineOptions.Load.
func (b *Backend) load(ev LoadEvent) error {
	switch ev.Kind {
	case LoadBegin:
		b.loading = &loader{st: newState(), scope: map[string]uint64{}}
		return nil
	case LoadSnapshot:
		var x snapExtra
		if len(ev.Header.Extra) > 0 {
			if err := json.Unmarshal(ev.Header.Extra, &x); err != nil {
				return fmt.Errorf("s3store: snapshot extra: %w", err)
			}
		}
		b.clock.Observe(x.LastStamp)
		return nil
	case LoadChunk:
		return b.loadChunk(ev.Chunk)
	case LoadRecord:
		var r record
		if err := json.Unmarshal(ev.Record.Payload, &r); err != nil {
			return fmt.Errorf("%w: record %d: %v", ErrCorruptJournal, ev.Record.LSN, err)
		}
		scope := "b/" + r.B
		switch r.Op {
		case opAuxPut, opAuxDel, opStage, opUnstage:
			scope = "a/" + r.NS
		}
		if ev.Record.LSN <= b.loading.scope[scope] {
			return nil
		}
		return b.replay(b.loading.st, &r)
	}
	return nil
}

func (b *Backend) loadChunk(c Chunk) error {
	l := b.loading
	switch c.Scope {
	case "b":
		var h bucketChunk
		if err := json.Unmarshal(c.Payload, &h); err != nil {
			return fmt.Errorf("%w: bucket chunk: %v", ErrCorruptJournal, err)
		}
		bk := b.newBucket(h.Info, h.ID)
		bk.keyed = h.Keyed
		if h.Configs != nil {
			bk.ix.Configs = h.Configs
		}
		for _, uc := range h.Uploads {
			u := bk.ix.AddUpload(uc.Info)
			for _, p := range uc.Parts {
				u.PutPart(&index.Part[blobRef]{Info: p.Info, Data: p.Data})
			}
		}
		l.st.buckets[h.Name] = bk
		l.scope["b/"+h.Name] = c.LSN
	case "o":
		var oc objectsChunk
		if err := json.Unmarshal(c.Payload, &oc); err != nil {
			return fmt.Errorf("%w: objects chunk: %v", ErrCorruptJournal, err)
		}
		bk := l.st.buckets[oc.Name]
		if bk == nil {
			return fmt.Errorf("%w: objects chunk of unknown bucket %q", ErrCorruptJournal, oc.Name)
		}
		for _, o := range oc.Objects {
			for _, vr := range o.Versions {
				v := vr.version()
				b.clock.Observe(v.Stamp)
				bk.ix.Put(o.Key, v)
			}
		}
	case "a":
		var ac auxChunk
		if err := json.Unmarshal(c.Payload, &ac); err != nil {
			return fmt.Errorf("%w: aux chunk: %v", ErrCorruptJournal, err)
		}
		a := l.st.auxNS(ac.NS)
		if ac.Records != nil {
			a.records = ac.Records
		}
		for _, s := range ac.Staged {
			a.staged[s] = true
		}
		l.scope["a/"+ac.NS] = c.LSN
	default:
		return fmt.Errorf("%w: unknown snapshot chunk scope %q", ErrCorruptJournal, c.Scope)
	}
	return nil
}

// replay applies a journal record to a state being loaded. Records about
// buckets or uploads that do not exist (created before the snapshot's
// start and deleted while it was written) are ignored.
func (b *Backend) replay(st *state, r *record) error {
	switch r.Op {
	case opMkBucket:
		if r.Info == nil {
			return fmt.Errorf("%w: bucket record without info", ErrCorruptJournal)
		}
		st.buckets[r.B] = b.newBucket(*r.Info, r.ID)
		return nil
	case opAuxPut:
		st.auxNS(r.NS).records[r.K] = r.D
		return nil
	case opAuxDel:
		a := st.auxNS(r.NS)
		for k := range a.records {
			if strings.HasPrefix(k, r.K) {
				delete(a.records, k)
			}
		}
		return nil
	case opStage:
		st.auxNS(r.NS).staged[r.K] = true
		return nil
	case opUnstage:
		delete(st.auxNS(r.NS).staged, r.K)
		return nil
	}
	bk := st.buckets[r.B]
	if bk == nil {
		return nil
	}
	ix := bk.ix
	switch r.Op {
	case opRmBucket:
		delete(st.buckets, r.B)
	case opVersioning:
		if r.Ver != nil {
			ix.Info.Versioning = storage.VersioningStatus(*r.Ver)
		}
	case opKeyMark:
		bk.keyed = true
	case opObjectLock:
		if r.Lock != nil {
			ix.Info.ObjectLockEnabled = *r.Lock
		}
	case opPutConfig:
		ix.Configs[r.K] = r.D
		if r.D == nil {
			ix.Configs[r.K] = []byte{}
		}
	case opDelConfig:
		delete(ix.Configs, r.K)
	case opChange:
		if r.C != nil {
			applyChange(ix, r.C, &b.clock)
		}
		if r.RmUpload != "" {
			ix.RemoveUpload(r.RmUpload)
		}
	case opMkUpload:
		if r.U != nil {
			ix.AddUpload(*r.U)
		}
	case opRmUpload:
		ix.RemoveUpload(r.RmUpload)
	case opPart:
		if r.P == nil {
			return fmt.Errorf("%w: part record without part", ErrCorruptJournal)
		}
		u, err := ix.Upload(r.K, r.P.Upload)
		if err == nil {
			u.PutPart(&index.Part[blobRef]{Info: r.P.Info, Data: r.P.Data})
		}
	default:
		return fmt.Errorf("%w: unknown record operation %q", ErrCorruptJournal, r.Op)
	}
	return nil
}
