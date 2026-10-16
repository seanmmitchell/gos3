// Package copier copies object versions between buckets at the storage
// level: from a bucket (and key prefix) of one storage.Backend to a bucket
// (and key prefix) of another, or of the same one. Clone and mirror jobs
// and restores of restic snapshots use it. In production both sides are
// the server's multi router, but the package only relies on the storage
// contract and its optional extensions.
//
// A copy proceeds key by key. ZipWalk merges the sorted listings of the
// source and the target into KeyPairs, and each pair is synchronised in one
// of two modes:
//
//   - Identity mode (SyncKey) is for sources whose listings describe
//     versions exactly as reads do (buckets gos3 stores itself). Both
//     sides' versions are compared directly, as the multi router compares a
//     replica with its primary, and the target keeps the source's version
//     IDs, modification times and metadata (storage.VersionImporter).
//   - Fingerprint mode (SyncKeyFingerprint) is for native sources and
//     targets (remote buckets stored as-is): their listings may report
//     other data than reads (the ETag of an object a foreign tool uploaded
//     in parts, for one), and they may not import versions. Each side's
//     listing of a key is reduced to a fingerprint and compared with the
//     fingerprints recorded after the key was last copied
//     (FingerprintStore), never with the other side.
//
// WriteVersion is the write path of both modes (and of restores): an
// import keeping the version's identity where the target supports it, else,
// where the caller allows it, a plain write.
//
// A Policy decides which versions are copied, what may be deleted on the
// target, and how encryption, compression and object lock carry over.
// Target versions are only deleted (or replaced) conditionally
// (storage.ConditionalDeleter): a version under unexpired retention or a
// legal hold is never deleted or replaced, it is reported as a Conflict.
//
// Every function is safe for concurrent use on different keys; a key must
// not be synchronised by two calls at once.
package copier

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// MaxKeyLen is the longest object key, in bytes, S3 allows. A source key
// that maps to a longer target key cannot be copied (see KeyPair).
const MaxKeyLen = 1024

// Errors returned by the copy functions (wrapped, with context). They
// describe settings or targets that cannot work, so a job retrying them
// unchanged would fail again.
var (
	// ErrPlaintext: an encrypted version would be stored unencrypted
	// (Policy.AllowPlaintext permits that).
	ErrPlaintext = errors.New("copier: the copy would store encrypted data unencrypted; allow plaintext copies or choose a target that can encrypt it")
	// ErrEncryptionUnsupported: the target cannot store data encrypted as
	// asked (it wraps errors.ErrUnsupported).
	ErrEncryptionUnsupported = fmt.Errorf("copier: the target cannot store the data encrypted: %w", errors.ErrUnsupported)
	// ErrRetention: a version has object lock retention or a legal hold
	// and the target bucket has no object lock (Policy.DropRetention copies
	// versions without them).
	ErrRetention = errors.New("copier: the source has object lock retention or legal holds and the target bucket has no object lock; drop retention to copy without them")
	// ErrNoImport: the target cannot import versions (it wraps
	// errors.ErrUnsupported). Identity mode needs a target that imports;
	// fingerprint mode writes plainly instead.
	ErrNoImport = fmt.Errorf("copier: the target cannot import versions: %w", errors.ErrUnsupported)
	// ErrNotVersioned: copying every version of a key with plain writes
	// needs a target bucket whose versioning is enabled (each write would
	// replace the previous one otherwise).
	ErrNotVersioned = errors.New("copier: copying every version with plain writes needs a target bucket with versioning enabled")
)

// Endpoint is one side of a copy: the keys of Bucket on Backend that start
// with Prefix. Source key Prefix+k is copied to target key
// target.Prefix+k.
type Endpoint struct {
	Backend storage.Backend
	Bucket  string
	Prefix  string
}

// String names the endpoint in errors.
func (e Endpoint) String() string { return e.Bucket + "/" + e.Prefix }

// MapKey returns the target key of source key srcKey ("" and false when
// srcKey lacks the source prefix).
func MapKey(src, dst Endpoint, srcKey string) (string, bool) {
	rel, ok := strings.CutPrefix(srcKey, src.Prefix)
	if !ok {
		return "", false
	}
	return dst.Prefix + rel, true
}

// KeyPair is one key of a copy as both listings report it. SrcKey and
// DstKey correspond through the endpoints' prefixes; either side may have
// no entries.
//
// A walk over versions fills Src and Dst with every version and delete
// marker of the key, newest first (Backend.WalkVersions); a walk over
// objects with the key's latest version when it is not a delete marker
// (Backend.WalkObjects), so with at most one entry. The entries are
// listing data, which the sync functions use to tell cheaply whether a key
// needs work; they list the key again before changing anything.
//
// A source key that maps to an empty target key or to one longer than
// MaxKeyLen cannot be copied: the sync functions report it as a Conflict.
type KeyPair struct {
	SrcKey, DstKey string
	Src, Dst       []storage.ObjectInfo
}

// Versions selects the versions a copy keeps.
type Versions string

const (
	// VersionsAll copies every version and delete marker of a key: the
	// target's history of the key becomes the source's (the default).
	VersionsAll Versions = "all"
	// VersionsCurrent copies the current version of each key only: the
	// target holds it as its null version (with plain writes: as the
	// target's own new version). Keys whose current version is a delete
	// marker count as absent.
	VersionsCurrent Versions = "current"
)

// EncryptionMode selects how a copy stores encrypted data.
type EncryptionMode string

const (
	// EncryptionPreserve stores each version encrypted as its source
	// version is (the same provider, storage.ObjectMeta.Encryption, with
	// the target's own keys): the default. A target that cannot fails the
	// copy with ErrEncryptionUnsupported, unless Policy.AllowPlaintext lets
	// it store the data unencrypted.
	EncryptionPreserve EncryptionMode = "preserve"
	// EncryptionTarget stores each version as Policy.TargetEncryption says,
	// whatever its source encryption. Copies are compared ignoring their
	// encryption, so versions already copied are not encrypted again when
	// the target's choice changes.
	EncryptionTarget EncryptionMode = "target"
)

// CompressionMode selects how a copy stores data compressed.
type CompressionMode string

const (
	// CompressionPreserve compresses the copies of versions stored
	// compressed on the source (at compress.DefaultLevel: the level the
	// source used is not recorded): the default.
	CompressionPreserve CompressionMode = "preserve"
	// CompressionTarget compresses as Policy.TargetCompression says.
	CompressionTarget CompressionMode = "target"
)

// Policy controls a copy. The zero value copies every version, deletes on
// the target what the source does not have (except keys the source never
// had, see OwnsKey), keeps encryption, compression, retention and legal
// holds, and refuses to decrypt.
type Policy struct {
	// Versions: VersionsAll ("" too) or VersionsCurrent.
	Versions Versions
	// Merge never deletes on the target: versions (and keys) the source
	// does not have stay. A target version with the ID of a source version
	// is still made identical to it (a null version is overwritten, as any
	// copy overwrites objects), and with VersionsAll the order of the
	// target's versions is not repaired.
	Merge bool
	// OwnsKey reports whether the copy may delete target key dstKey when
	// the source has no version (VersionsAll) or no current version
	// (VersionsCurrent) of it: whether the copy wrote the key, or the
	// caller adopted every key. nil owns no key. Merge overrides it. In
	// fingerprint mode, keys the store records as owned are owned too.
	//
	// With VersionsAll an owned key loses every target version; with
	// VersionsCurrent its current object (a null version is deleted,
	// another version gets a delete marker). Versions of keys the source
	// does have are deleted unless Merge, whoever wrote them.
	OwnsKey func(dstKey string) bool

	// Encryption: EncryptionPreserve ("" too) or EncryptionTarget.
	Encryption EncryptionMode
	// TargetEncryption returns, for EncryptionTarget, the SSE-S3 provider
	// (storage.EncryptionAES256, storage.EncryptionRestic) to store version
	// v with, or "" to store it unencrypted. The storage layer does not
	// apply bucket default encryption (the S3 layer does), so the caller
	// decides from the target bucket's configuration documents
	// (storage.ConfigEncryption, storage.ConfigEncryptionProvider). nil
	// stores every version unencrypted.
	//
	// The SSE fields of a version's metadata (what clients requested) are
	// copied as they are, and cleared when an encrypted version is stored
	// unencrypted.
	TargetEncryption func(v storage.ObjectInfo) string
	// AllowPlaintext permits storing encrypted versions unencrypted: with
	// EncryptionPreserve when the target cannot store the source's
	// encryption (a native bucket, a backing without the provider), with
	// EncryptionTarget when TargetEncryption returns "". Without it these
	// copies fail with ErrPlaintext or ErrEncryptionUnsupported.
	AllowPlaintext bool

	// DropRetention copies versions without their object lock retention
	// and legal hold. Without it a version with either fails the copy with
	// ErrRetention when the target bucket has no object lock.
	DropRetention bool

	// Compression: CompressionPreserve ("" too) or CompressionTarget.
	Compression CompressionMode
	// TargetCompression returns, for CompressionTarget, how to store
	// version v (internal/compression.Decide for the target bucket's
	// setting, for instance). nil stores every version uncompressed. A
	// target may store data uncompressed anyway.
	TargetCompression func(v storage.ObjectInfo) storage.Compression

	// Now returns the time against which retention is judged (default
	// time.Now).
	Now func() time.Time
}

// Validate checks the policy's modes.
func (p Policy) Validate() error {
	switch p.Versions {
	case "", VersionsAll, VersionsCurrent:
	default:
		return fmt.Errorf("copier: invalid versions mode %q", p.Versions)
	}
	switch p.Encryption {
	case "", EncryptionPreserve, EncryptionTarget:
	default:
		return fmt.Errorf("copier: invalid encryption mode %q", p.Encryption)
	}
	switch p.Compression {
	case "", CompressionPreserve, CompressionTarget:
	default:
		return fmt.Errorf("copier: invalid compression mode %q", p.Compression)
	}
	return nil
}

// all reports whether the policy copies every version.
func (p Policy) all() bool { return p.Versions != VersionsCurrent }

// strictEncryption reports whether copies must be encrypted exactly as
// their sources: only then is the encryption part of a version's identity.
func (p Policy) strictEncryption() bool {
	return p.Encryption != EncryptionTarget && !p.AllowPlaintext
}

func (p Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p Policy) owns(dstKey string) bool { return p.OwnsKey != nil && p.OwnsKey(dstKey) }

// Conflict is something a copy could not do and will not retry by itself:
// a target version it must not delete or change, a key it cannot map.
type Conflict struct {
	// Key is the source key of the pair; Target its target key.
	Key    string
	Target string
	// VersionID is the target version concerned ("" for the whole key).
	VersionID string
	Reason    string
}

func (c Conflict) String() string {
	s := c.Key
	if c.VersionID != "" {
		s += " (target version " + c.VersionID + ")"
	}
	return s + ": " + c.Reason
}

// Result counts what one sync did.
type Result struct {
	VersionsCopied  int64 // versions and delete markers written
	BytesCopied     int64 // content bytes written
	VersionsDeleted int64 // target versions deleted or hidden by a delete marker
	MetaUpdated     int64 // target versions whose metadata was updated
	// PlainWrites counts the copied versions stored with plain writes
	// (with the target's own version IDs and modification times) because
	// the target does not import versions.
	PlainWrites int64
	Conflicts   []Conflict
	// Deferred: the key changed on the source (or the target) during every
	// attempt to copy it; it was left for a later pass.
	Deferred bool
}

// Add adds o's counts and conflicts to r.
func (r *Result) Add(o Result) {
	r.VersionsCopied += o.VersionsCopied
	r.BytesCopied += o.BytesCopied
	r.VersionsDeleted += o.VersionsDeleted
	r.MetaUpdated += o.MetaUpdated
	r.PlainWrites += o.PlainWrites
	r.Conflicts = append(r.Conflicts, o.Conflicts...)
	r.Deferred = r.Deferred || o.Deferred
}

// Changed reports whether the sync changed the target.
func (r Result) Changed() bool {
	return r.VersionsCopied > 0 || r.VersionsDeleted > 0 || r.MetaUpdated > 0
}

// errChanged: the source or the target changed under a sync; the key is
// listed and compared again.
var errChanged = errors.New("copier: the key changed during the copy")

// maxAttempts is the number of times a key that keeps changing is tried
// (the first attempt and up to three retries) before it is deferred.
const maxAttempts = 4

// keyConflict reports a pair whose source key cannot be copied.
func keyConflict(p KeyPair) (Conflict, bool) {
	c := Conflict{Key: p.SrcKey, Target: p.DstKey}
	switch {
	case p.DstKey == "":
		c.Reason = "the key maps to an empty target key"
	case len(p.DstKey) > MaxKeyLen:
		c.Reason = fmt.Sprintf("the target key is longer than %d bytes", MaxKeyLen)
	default:
		return Conflict{}, false
	}
	return c, true
}
