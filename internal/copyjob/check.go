package copyjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// sampleEntries is the number of source listing entries Validate reads to
// find out whether the source holds encrypted data.
const sampleEntries = 1000

// inspection is what a job needs to know about its buckets.
type inspection struct {
	srcInfo storage.BucketInfo
	srcCaps storage.BucketCaps
	// dstExists: the target bucket exists (dstInfo is set); else it is to
	// be created with create (and dstCaps are those it would have).
	dstExists bool
	dstInfo   storage.BucketInfo
	dstCaps   storage.BucketCaps
	create    storage.CreateBucketOptions
}

// identity reports whether keys are copied in identity mode: both buckets
// import versions (and the source's history can be copied exactly).
func (in *inspection) identity() bool {
	return in.srcCaps.Import && in.srcCaps.History && in.dstCaps.Import
}

// nativeSource reports whether the source is a native bucket, which other
// tools may change behind the router's back.
func (in *inspection) nativeSource() bool { return in.srcCaps.Kind == storage.BucketKindNative }

// dstLock reports whether the target has (or will have) object lock.
func (in *inspection) dstLock() bool {
	if in.dstExists {
		return in.dstInfo.ObjectLockEnabled
	}
	return in.create.ObjectLockEnabled
}

// check decodes, normalizes and validates spec (see the package
// documentation, Safety). kind is the runner's kind ("" for a dry run).
// With warn it also returns the warnings of a dry run.
func (s *Service) check(ctx context.Context, kind string, raw []byte, warn bool) (Spec, []string, error) {
	sp, err := decodeSpec(raw)
	if err != nil {
		return Spec{}, nil, err
	}
	if err := normalize(&sp, kind); err != nil {
		return Spec{}, nil, err
	}
	if s.router == nil {
		return Spec{}, nil, errors.New("copyjob: no storage router")
	}
	in, err := s.inspect(ctx, &sp, "")
	if err != nil {
		return Spec{}, nil, err
	}
	if err := s.checkRules(ctx, &sp, in); err != nil {
		return Spec{}, nil, err
	}
	// Never trust bindings supplied in the request. Validation records the
	// bucket incarnations that the runner must use, including across retries.
	sp.BucketBindings = bucketBindings{Source: identityOf(in.srcInfo)}
	if in.dstExists {
		sp.BucketBindings.Target = identityOf(in.dstInfo)
	}
	if err := checkJobs(&sp, s.activeJobs(nil)); err != nil {
		return Spec{}, nil, err
	}
	if in.dstExists && sp.Options.OnExisting == OnExistingFail {
		empty, err := rangeEmpty(ctx, s.router, sp.Target.Bucket, sp.Target.Prefix)
		if err != nil {
			return Spec{}, nil, err
		}
		if !empty {
			return Spec{}, nil, errNotEmpty(&sp)
		}
	}
	var warnings []string
	if warn {
		warnings = s.warnings(ctx, &sp, in)
	}
	return sp, warnings, nil
}

// errNotEmpty is the error for a target range that holds data when
// onExisting is "fail".
func errNotEmpty(sp *Spec) error {
	alt := `"merge"`
	if sp.mirror() {
		alt = `"merge" or "adopt"`
	}
	return conflictf("the target %s is not empty (it has objects, versions, delete markers or multipart uploads); "+
		"choose onExisting %s, or an empty target", sp.dstRange(), alt)
}

// inspect reads what the job needs to know about its buckets. A missing
// source, or a missing target without create, is an invalid spec; so is a
// create the backing cannot honour. owner is the owner of a new target.
func (s *Service) inspect(ctx context.Context, sp *Spec, owner string) (*inspection, error) {
	in := &inspection{}
	var err error
	if in.srcInfo, err = s.router.GetBucketIdentity(ctx, sp.Source.Bucket); err != nil {
		if errors.Is(err, storage.ErrBucketNotFound) {
			return nil, invalidf("the source bucket %q does not exist", sp.Source.Bucket)
		}
		return nil, fmt.Errorf("reading the source bucket %q: %w", sp.Source.Bucket, err)
	}
	if in.srcCaps, err = s.router.BucketCaps(ctx, sp.Source.Bucket); err != nil {
		return nil, fmt.Errorf("reading the source bucket %q: %w", sp.Source.Bucket, err)
	}
	in.dstInfo, err = s.router.GetBucketIdentity(ctx, sp.Target.Bucket)
	switch {
	case err == nil:
		in.dstExists = true
		if in.dstCaps, err = s.router.BucketCaps(ctx, sp.Target.Bucket); err != nil {
			return nil, fmt.Errorf("reading the target bucket %q: %w", sp.Target.Bucket, err)
		}
		return in, nil
	case !errors.Is(err, storage.ErrBucketNotFound):
		return nil, fmt.Errorf("reading the target bucket %q: %w", sp.Target.Bucket, err)
	case sp.Target.Create == nil:
		return nil, invalidf("the target bucket %q does not exist; set target.create to create it", sp.Target.Bucket)
	case !validBucketName(sp.Target.Bucket):
		return nil, invalidf("%q is not a valid bucket name", sp.Target.Bucket)
	}
	in.create = s.createOptions(sp, owner, false)
	if in.dstCaps, err = s.router.NewBucketCaps(ctx, in.create); err != nil {
		if errors.Is(err, storage.ErrInvalidBacking) || errors.Is(err, storage.ErrInvalidTarget) {
			return nil, invalidf("target.create: %v", err)
		}
		return nil, fmt.Errorf("checking target.create: %w", err)
	}
	lock := sp.Target.Create.ObjectLock
	if lock == nil {
		// Locked data keeps its locks by default.
		v := in.srcInfo.ObjectLockEnabled && in.dstCaps.ObjectLock
		lock = &v
		sp.Target.Create.ObjectLock = lock
	}
	if *lock && !in.dstCaps.ObjectLock {
		return nil, invalidf("target.create.objectLock: the new bucket cannot have object lock")
	}
	in.create.ObjectLockEnabled = *lock
	return in, nil
}

// createOptions returns the options the target is created with. A new
// target is created where it can import versions (a wrapped bucket on an
// s3 backing) unless the spec asks for a native bucket.
func (s *Service) createOptions(sp *Spec, owner string, lock bool) storage.CreateBucketOptions {
	c := sp.Target.Create
	opts := storage.CreateBucketOptions{Backing: c.Backing, Owner: owner, ObjectLockEnabled: lock}
	if s.region != nil {
		opts.Region = s.region()
	}
	if c.Kind != "" || c.Container != "" || c.RemoteBucket != "" {
		opts.Target = &storage.BucketTarget{Kind: storage.BucketKind(c.Kind), Container: c.Container, RemoteBucket: c.RemoteBucket}
	}
	opts.Import = c.Kind != string(storage.BucketKindNative) && c.RemoteBucket == ""
	return opts
}

// checkRules applies the rules that depend on the buckets and fills in
// the versions default.
func (s *Service) checkRules(ctx context.Context, sp *Spec, in *inspection) error {
	o := &sp.Options
	if !in.srcCaps.Clone {
		return invalidf("the bucket %q cannot be the source of a copy", sp.Source.Bucket)
	}
	keeps := in.dstCaps.Import || in.dstExists && in.dstInfo.Versioning == storage.VersioningEnabled
	switch {
	case o.Versions == "" && keeps:
		o.Versions = VersionsAll
	case o.Versions == "":
		o.Versions = VersionsCurrent
	case o.Versions == VersionsAll && !keeps && (in.dstExists || !in.dstCaps.Versioning):
		if in.dstExists && in.dstCaps.Versioning {
			return invalidf("versions \"all\": the target bucket %q cannot import versions and its versioning is not enabled; "+
				"enable versioning on it or copy the current versions only", sp.Target.Bucket)
		}
		return invalidf("versions \"all\": the target bucket %q cannot keep versions; copy the current versions only", sp.Target.Bucket)
	}
	if in.srcInfo.ObjectLockEnabled && !in.dstLock() && !o.DropRetention {
		return invalidf("the source bucket %q has object lock and the target has none: its retention and legal holds cannot be kept; "+
			"create the target with object lock, or set dropRetention", sp.Source.Bucket)
	}
	if o.AllowPlaintext {
		return nil
	}
	encs, err := s.sourceEncryption(ctx, sp)
	if err != nil {
		return err
	}
	for _, p := range encs {
		switch {
		case in.dstCaps.Kind == storage.BucketKindNative:
			return invalidf("the source holds %s-encrypted data and the target is a native bucket, which stores data as it is; "+
				"set allowPlaintext to store it unencrypted, or choose another target", p)
		case o.Encryption == EncryptionPreserve && !s.canStore(ctx, sp, in, p):
			return invalidf("the source holds %s-encrypted data the target cannot store encrypted that way; "+
				"choose encryption \"target\", another target, or set allowPlaintext", p)
		}
	}
	return nil
}

// sourceEncryption returns the encryption providers the source's data may
// be stored with: its default encryption's provider, restic when every
// write is archived through restic, and those of a sample of its versions.
func (s *Service) sourceEncryption(ctx context.Context, sp *Spec) ([]string, error) {
	var out []string
	add := func(p string) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	set := storage.EncryptionSettings{Provider: storage.EncryptionAES256}
	raw, err := s.router.GetBucketConfig(ctx, sp.Source.Bucket, storage.ConfigEncryptionProvider)
	switch {
	case err == nil:
		if parsed, perr := storage.ParseEncryptionSettings(raw); perr == nil {
			set = parsed
		}
	case !errors.Is(err, storage.ErrConfigNotFound):
		return nil, fmt.Errorf("reading the source's encryption settings: %w", err)
	}
	if set.Scope == storage.EncryptionScopeAll {
		add(set.Provider)
	}
	if _, err := s.router.GetBucketConfig(ctx, sp.Source.Bucket, storage.ConfigEncryption); err == nil {
		add(set.Provider)
	} else if !errors.Is(err, storage.ErrConfigNotFound) {
		return nil, fmt.Errorf("reading the source's default encryption: %w", err)
	}
	n := 0
	err = s.router.WalkVersions(ctx, sp.Source.Bucket, sp.Source.Prefix, "", "", func(v storage.ObjectInfo) error {
		add(v.Encryption)
		if n++; n >= sampleEntries {
			return storage.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing the source: %w", err)
	}
	return out, nil
}

// canStore reports whether the target can store data encrypted with
// provider. For a target to be created it asks the backing (where it
// cannot tell, the copy itself finds out).
func (s *Service) canStore(ctx context.Context, sp *Spec, in *inspection, provider string) bool {
	if in.dstCaps.Kind == storage.BucketKindNative {
		return false
	}
	if in.dstExists {
		ps, err := s.router.EncryptionProviders(ctx, sp.Target.Bucket)
		return err != nil || slices.Contains(ps, provider)
	}
	name := sp.Target.Create.Backing
	if name == "" {
		name = s.router.DefaultBacking()
	}
	b, ok := s.router.BackingBackend(name)
	if !ok {
		return true
	}
	ep, ok := b.(storage.EncryptionProvidersBackend)
	if !ok {
		return false
	}
	ps, err := ep.EncryptionProviders(ctx, sp.Target.Bucket)
	return err != nil || slices.Contains(ps, provider)
}

// rangeEmpty reports whether the keys of bucket under prefix have no
// version, delete marker or multipart upload.
func rangeEmpty(ctx context.Context, b storage.Backend, bucket, prefix string) (bool, error) {
	found := false
	stop := func() error { found = true; return storage.ErrStopWalk }
	if err := b.WalkVersions(ctx, bucket, prefix, "", "", func(storage.ObjectInfo) error { return stop() }); err != nil {
		return false, fmt.Errorf("listing the target: %w", err)
	}
	if found {
		return false, nil
	}
	err := storage.WalkMultipartUploads(ctx, b, bucket, prefix, storage.UploadPosition{}, func(storage.UploadInfo) error { return stop() })
	if err != nil {
		return false, fmt.Errorf("listing the target's multipart uploads: %w", err)
	}
	return !found, nil
}

// otherJob is an active copy or mirror job, as the checks between jobs see
// it.
type otherJob struct {
	id       string
	kind     string
	src, dst keyRange
}

// activeJobs returns the unfinished copy and mirror jobs of the attached
// Manager for which keep (nil: every one) returns true.
func (s *Service) activeJobs(keep func(jobs.Job) bool) []otherJob {
	m := s.mgr.Load()
	if m == nil {
		return nil
	}
	list := m.List(jobs.Filter{Match: func(j jobs.Job) bool {
		return (j.Kind == KindCopy || j.Kind == KindMirror) && !j.State.Finished() && (keep == nil || keep(j))
	}})
	out := make([]otherJob, 0, len(list))
	for _, j := range list {
		var sp Spec
		if err := json.Unmarshal(j.Spec, &sp); err != nil {
			continue
		}
		out = append(out, otherJob{id: j.ID, kind: j.Kind, src: sp.srcRange(), dst: sp.dstRange()})
	}
	return out
}

// olderJobs returns the active copy and mirror jobs created before j.
func (s *Service) olderJobs(j jobs.Job) []otherJob {
	return s.activeJobs(func(o jobs.Job) bool {
		if o.ID == j.ID {
			return false
		}
		if c := o.CreatedAt.Compare(j.CreatedAt); c != 0 {
			return c < 0
		}
		return o.ID < j.ID
	})
}

// checkJobs checks a job against the other active jobs: nothing may be
// copied into the target of an active mirror, a mirror's target may not be
// the target of another active job, nor may the target of a job that
// needs an empty target (onExisting "fail"), and a mirror may not close a
// cycle of mirrors (chains are fine). Errors wrap jobs.ErrConflict.
func checkJobs(sp *Spec, others []otherJob) error {
	src, dst := sp.srcRange(), sp.dstRange()
	for _, o := range others {
		switch {
		case !o.dst.overlaps(dst):
		case o.kind == KindMirror:
			return conflictf("the target %s is the target of the active mirror job %s", dst, o.id)
		case sp.mirror():
			return conflictf("the target %s is the target of the active %s job %s; a mirror needs a target of its own", dst, o.kind, o.id)
		case sp.Options.OnExisting == OnExistingFail:
			return conflictf("the target %s is the target of the active %s job %s, so it will not stay empty; "+
				"wait for that job, or choose onExisting \"merge\"", dst, o.kind, o.id)
		}
	}
	if !sp.mirror() {
		return nil
	}
	// Follow the mirrors from the new target: reaching the new source
	// would make changes go round for ever.
	var mirrors []otherJob
	for _, o := range others {
		if o.kind == KindMirror {
			mirrors = append(mirrors, o)
		}
	}
	seen := make([]bool, len(mirrors))
	frontier := []keyRange{dst}
	for len(frontier) > 0 {
		cur := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for i, o := range mirrors {
			if seen[i] || !o.src.overlaps(cur) {
				continue
			}
			seen[i] = true
			if o.dst.overlaps(src) {
				return conflictf("the mirror would close a cycle of mirrors (through the active mirror job %s from %s to %s)", o.id, o.src, o.dst)
			}
			frontier = append(frontier, o.dst)
		}
	}
	return nil
}

// warnings explains what a job would do that its submitter may not
// expect.
func (s *Service) warnings(ctx context.Context, sp *Spec, in *inspection) []string {
	var w []string
	add := func(format string, args ...any) { w = append(w, fmt.Sprintf(format, args...)) }
	o := &sp.Options
	has := func(bucket, key string) bool {
		_, err := s.router.GetBucketConfig(ctx, bucket, key)
		return err == nil
	}
	if in.dstExists {
		if sp.Target.Create != nil {
			add("the target bucket %q exists; target.create is ignored", sp.Target.Bucket)
		}
		if has(sp.Target.Bucket, storage.ConfigLifecycle) {
			add("the target bucket %q has a lifecycle configuration: its rules apply to the copies and may expire or delete them", sp.Target.Bucket)
		}
		if o.OnExisting != OnExistingFail {
			if empty, err := rangeEmpty(ctx, s.router, sp.Target.Bucket, sp.Target.Prefix); err == nil && !empty {
				switch o.OnExisting {
				case OnExistingMerge:
					add("the target is not empty: its keys are kept, and keys the source also has are overwritten with the source's")
				default:
					add("the target is not empty: the mirror takes it over: keys the source also has are made identical to the source's " +
						"(their other versions are deleted), and keys the source does not have are deleted")
				}
			}
		}
	}
	if has(sp.Source.Bucket, storage.ConfigLifecycle) && o.Configs != ConfigsNone {
		if sp.mirror() {
			add("the source's lifecycle configuration is not mirrored")
		} else {
			add("the source's lifecycle configuration is copied: its rules then apply to the target too")
		}
	}
	for _, key := range configKeys(sp) {
		srcDoc, _ := getConfig(ctx, s.router, sp.Source.Bucket, key)
		if srcDoc == nil {
			continue
		}
		var dstDoc []byte
		if in.dstExists {
			dstDoc, _ = getConfig(ctx, s.router, sp.Target.Bucket, key)
		}
		if !selected(sp, key) {
			if storage.ACLGuard(key, srcDoc) && !storage.ACLGuard(key, dstDoc) {
				add("the source's %s configuration is copied although configs does not select it: "+
					"it keeps the ACLs the copied objects carry from granting access", key)
			}
			continue
		}
		if key == storage.ConfigPolicy {
			if _, reason := rewritePolicy(srcDoc, sp); reason != "" {
				add("the bucket policy is not copied: %s", reason)
			}
		}
		if dstDoc != nil && !bytes.Equal(srcDoc, dstDoc) {
			add("the target's %s configuration is replaced by the source's", key)
		}
	}
	if !in.identity() {
		if in.dstCaps.Import {
			add("the source keeps no version history that can be copied exactly: changes are found by comparing listings, and version IDs that the target cannot use are replaced")
		} else {
			add("the target cannot import versions: the copies get the target's own version IDs and modification times")
		}
	}
	if sp.mirror() && in.nativeSource() {
		add("the source is a native bucket: changes made to it outside gos3 are found every %s (pollInterval)", o.PollInterval)
	}
	if o.Versions == VersionsCurrent && in.srcInfo.Versioning != storage.VersioningUnversioned {
		add("only the current versions are copied: older versions and delete markers are not")
	}
	if in.srcInfo.ObjectLockEnabled && !in.dstLock() {
		add("the target has no object lock: retention and legal holds are dropped from the copies")
	}
	if o.AllowPlaintext {
		if encs, err := s.sourceEncryption(ctx, sp); err == nil {
			for _, p := range encs {
				if o.Encryption == EncryptionTarget || !s.canStore(ctx, sp, in, p) {
					add("allowPlaintext: %s-encrypted data may be stored unencrypted on the target", p)
				}
			}
		}
	}
	if sp.mirror() {
		if o.deleteExtraneous() {
			add("deleteExtraneous: target keys the source does not have are deleted")
		}
		if !o.propagateDeletes() || o.OnExisting == OnExistingMerge {
			add("deletes in the source are not applied to the target")
		}
	}
	if sp.Source.Prefix != sp.Target.Prefix {
		add("keys are renamed: %q becomes %q", sp.Source.Prefix+"…", sp.Target.Prefix+"…")
	}
	slices.Sort(w)
	return slices.Compact(w)
}

// describe is a short description of a job's ranges for logs.
func describe(sp *Spec) string {
	return strings.Join([]string{sp.srcRange().String(), sp.dstRange().String()}, " -> ")
}
