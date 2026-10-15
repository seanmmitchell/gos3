package multi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// The optional storage interface implemented in this file.
var _ storage.EncryptionBackend = (*Router)(nil)

// PrepareEncryption prepares mode's keys or repository for the bucket on
// its primary, which receives every write first (see
// storage.EncryptionBackend). Every other backing holding or receiving a
// copy must offer mode too (see membersOffer), else the write is refused
// (errors.ErrUnsupported): its copy could never be replicated. Replicas
// prepare their own keys when they receive data.
func (r *Router) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	members, err := r.placementBackings(bucket)
	if err != nil {
		return err
	}
	if err := membersOffer(ctx, members, bucket, mode); err != nil {
		return err
	}
	e, ok := members[0].Backend.(storage.EncryptionBackend)
	if !ok {
		return errors.ErrUnsupported
	}
	return e.PrepareEncryption(ctx, bucket, mode)
}

// ValidateEncryptionSettings checks cfg on the bucket's primary (see
// storage.EncryptionBackend), after checking that every backing holding or
// receiving a copy of the bucket offers the provider (see membersOffer): a
// copy it cannot store would never be replicated.
func (r *Router) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	members, err := r.placementBackings(bucket)
	if err != nil {
		return err
	}
	if err := membersOffer(ctx, members, bucket, cfg.Provider); err != nil {
		return err
	}
	e, ok := members[0].Backend.(storage.EncryptionBackend)
	if !ok {
		return errors.ErrUnsupported
	}
	return e.ValidateEncryptionSettings(ctx, bucket, cfg)
}

// membersOffer checks that every backing of members can store bucket's
// data encrypted with provider (see providersOf), returning a wrapped
// errors.ErrUnsupported naming the first that cannot.
func membersOffer(ctx context.Context, members []*backing, bucket, provider string) error {
	for _, b := range members {
		if !slices.Contains(providersOf(ctx, b, bucket), provider) {
			return fmt.Errorf("backing %q holding a copy of bucket %q cannot store %s-encrypted data: %w",
				b.Name, bucket, provider, errors.ErrUnsupported)
		}
	}
	return nil
}

// checkWriteEncryption refuses (fail closed) an encrypted write to bucket
// when a backing holding or receiving a copy of it cannot store the
// encryption: the version would never reach that copy. Writes through the
// S3 layer are refused earlier, by PrepareEncryption.
func (r *Router) checkWriteEncryption(ctx context.Context, bucket string, opts storage.WriteOptions) error {
	if opts.Encryption == "" {
		return nil
	}
	members, err := r.placementBackings(bucket)
	if err != nil {
		return err
	}
	return membersOffer(ctx, members, bucket, opts.Encryption)
}

// providersOf returns the SSE-S3 implementations backing b can store for
// bucket (see storage.EncryptionProvidersBackend). A backend without that
// interface, or one that fails to answer, supports none.
func providersOf(ctx context.Context, b *backing, bucket string) []string {
	e, ok := b.Backend.(storage.EncryptionProvidersBackend)
	if !ok {
		return nil
	}
	p, err := e.EncryptionProviders(ctx, bucket)
	if err != nil {
		return nil
	}
	return p
}

// placementBackings returns the backings holding or receiving a copy of
// bucket: the primary first, then the replicas and a move target.
func (r *Router) placementBackings(bucket string) ([]*backing, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	rec := bs.rec
	r.mu.RUnlock()
	// Live backing configuration never replaces or removes a backing that
	// holds or receives a copy, so the members resolve in any topology.
	topology := r.topology.Load()
	names := memberNames(rec.Members)
	if rec.Move != nil {
		names = append(names, rec.Move.To)
	}
	out := make([]*backing, 0, len(names))
	for _, name := range names {
		b := topology.backings[name]
		if b == nil {
			return nil, fmt.Errorf("%w %q of bucket %q", ErrUnknownBacking, name, bucket)
		}
		out = append(out, b)
	}
	return out, nil
}

// checkEncryptionTarget refuses to place a copy of bucket on backing b
// when b lacks an SSE-S3 implementation the bucket uses or will use:
//
//   - the provider selected explicitly (storage.ConfigEncryptionProvider);
//   - without one, the implicit storage.EncryptionAES256 when the bucket
//     encrypts new objects by default (storage.ConfigEncryption);
//   - that of any stored version (including versions kept after the
//     provider changed).
//
// Replication to b would never complete. primary is the bucket's primary.
// Finding the providers in use walks every version of the bucket, which is
// skipped when b offers every known provider.
func (r *Router) checkEncryptionTarget(ctx context.Context, bucket string, primary, b *backing) error {
	offered := providersOf(ctx, b, bucket)
	if slices.Contains(offered, storage.EncryptionAES256) && slices.Contains(offered, storage.EncryptionRestic) {
		return nil
	}
	refuse := func(provider string) error {
		return fmt.Errorf("%w: bucket %q stores %s-encrypted data, which backing %q cannot store", ErrInvalidPlacement, bucket, provider, b.Name)
	}
	raw, err := primary.Backend.GetBucketConfig(ctx, bucket, storage.ConfigEncryptionProvider)
	switch {
	case err == nil:
		provider, err := storage.ParseEncryptionProvider(raw)
		if err != nil {
			return err
		}
		if !slices.Contains(offered, provider) {
			return refuse(provider)
		}
	case errors.Is(err, storage.ErrConfigNotFound):
		raw, err := primary.Backend.GetBucketConfig(ctx, bucket, storage.ConfigEncryption)
		switch {
		case err == nil:
			if defaultSSE(raw) && !slices.Contains(offered, storage.EncryptionAES256) {
				return refuse(storage.EncryptionAES256)
			}
		case !errors.Is(err, storage.ErrConfigNotFound):
			return r.observe(primary, err)
		}
	default:
		return r.observe(primary, err)
	}
	missing := ""
	err = primary.Backend.WalkVersions(ctx, bucket, "", "", "", func(oi storage.ObjectInfo) error {
		if oi.Encryption != "" && !slices.Contains(offered, oi.Encryption) {
			missing = oi.Encryption
			return storage.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		return r.observe(primary, err)
	}
	if missing != "" {
		return refuse(missing)
	}
	return nil
}

// sseConfig is the part of a stored storage.ConfigEncryption document
// (ServerSideEncryptionConfiguration XML) that says whether new objects
// are encrypted by default.
type sseConfig struct {
	Rules []struct {
		Default *struct {
			SSEAlgorithm string `xml:"SSEAlgorithm"`
		} `xml:"ApplyServerSideEncryptionByDefault"`
	} `xml:"Rule"`
}

// defaultSSE reports whether the storage.ConfigEncryption document raw
// encrypts new objects by default. The S3 layer applies SSE-S3 for any
// algorithm (a legacy AWS KMS default included) and ignores an invalid
// document.
func defaultSSE(raw []byte) bool {
	var doc sseConfig
	if xml.Unmarshal(raw, &doc) != nil {
		return false
	}
	for _, rule := range doc.Rules {
		if rule.Default != nil && strings.TrimSpace(rule.Default.SSEAlgorithm) != "" {
			return true
		}
	}
	return false
}

// canImport reports whether backing b can receive copies of buckets
// (storage.VersionImporter): an s3 gateway backing cannot.
func canImport(b *backing) bool {
	_, ok := b.Backend.(storage.VersionImporter)
	return ok
}

// errNoImport describes why backing name cannot receive copies.
func errNoImport(name string) string {
	return fmt.Sprintf("backing %q cannot hold replicas or receive moves (S3 gateway backing)", name)
}

// checkNewMembers checks the backings of names that do not hold a copy of
// the bucket (state bs) yet before they receive one: each must import
// versions (see canImport) and store the bucket's encryption (see
// checkEncryptionTarget). A bucket whose primary cannot import versions
// either (an s3 gateway backing, which keeps no version history to copy
// from) gets no new copies at all: replicas of it and moves of it are
// refused.
func (r *Router) checkNewMembers(ctx context.Context, bucket string, bs *bucketState, names []string) error {
	r.mu.RLock()
	rec := bs.rec
	r.mu.RUnlock()
	// The caller holds adminMu, which also serializes live backing
	// configuration, so this topology stays current during the checks.
	topology := r.topology.Load()
	primary := topology.backings[rec.primary()]
	if primary == nil {
		return fmt.Errorf("%w %q of bucket %q", ErrUnknownBacking, rec.primary(), bucket)
	}
	var added []*backing
	for _, name := range names {
		if rec.member(name) >= 0 {
			continue
		}
		// Re-resolved here: the backing may have been reconfigured since
		// the caller validated the name.
		b := topology.backings[name]
		if b == nil {
			return fmt.Errorf("%w %q", ErrUnknownBacking, name)
		}
		if !canImport(b) {
			return fmt.Errorf("%w: %s", ErrInvalidPlacement, errNoImport(name))
		}
		added = append(added, b)
	}
	if len(added) > 0 && !canImport(primary) {
		return fmt.Errorf("%w: bucket %q cannot be replicated or moved: its primary backing %q is an S3 gateway backing, "+
			"which keeps no version history to copy", ErrInvalidPlacement, bucket, primary.Name)
	}
	for _, b := range added {
		if err := r.checkEncryptionTarget(ctx, bucket, primary, b); err != nil {
			return err
		}
	}
	return nil
}
