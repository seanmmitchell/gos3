package copyjob

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

	"github.com/seanmmitchell/gos3/internal/compression"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
)

// buildPolicy maps the job's options to the copier's policy for this run:
//
//   - versions as the spec says;
//   - Merge (never delete) with onExisting "merge", and for mirrors that do
//     not propagate deletes;
//   - a target key may be deleted when the source no longer has it if the
//     job wrote it (the owned set of the job's state) or, for mirrors, with
//     deleteExtraneous or onExisting "adopt";
//   - encryption "target" encrypts as the target bucket's settings say
//     (targetEncryption), compression "target" compresses as the target
//     bucket's setting and the server's policy say;
//   - retention is dropped only when the target has no object lock.
func (r *run) buildPolicy(ctx context.Context) error {
	o := &r.sp.Options
	pol := copier.Policy{
		Versions:       copier.VersionsAll,
		Merge:          o.OnExisting == OnExistingMerge || r.sp.mirror() && !o.propagateDeletes(),
		Encryption:     copier.EncryptionMode(o.Encryption),
		AllowPlaintext: o.AllowPlaintext,
		DropRetention:  o.DropRetention && !r.in.dstLock(),
		Compression:    copier.CompressionMode(o.Compression),
	}
	if !r.sp.all() {
		pol.Versions = copier.VersionsCurrent
	}
	// A mirror that adopted its target owns every key there, as if it had
	// written them.
	extraneous := r.sp.mirror() && (o.deleteExtraneous() || o.OnExisting == OnExistingAdopt)
	st, log := r.st, r.log
	pol.OwnsKey = func(key string) bool {
		if extraneous {
			return true
		}
		owned, err := st.Owned(key)
		if err != nil {
			log.Warn("copy job: reading the job database", "key", key, "error", err)
			return false
		}
		return owned
	}
	if pol.Encryption == copier.EncryptionTarget {
		f, err := r.targetEncryption(ctx)
		if err != nil {
			return err
		}
		pol.TargetEncryption = f
	}
	if pol.Compression == copier.CompressionTarget {
		f, err := r.targetCompression(ctx)
		if err != nil {
			return err
		}
		pol.TargetCompression = f
	}
	if err := pol.Validate(); err != nil {
		return err
	}
	r.pol = pol
	return nil
}

// targetEncryption returns how the target stores a version with
// encryption "target", as the S3 layer would store a client's write:
// encrypted with the target's provider when the version was written with
// SSE or the target has default encryption, with restic when the target
// archives every write through restic, else unencrypted.
func (r *run) targetEncryption(ctx context.Context) (func(storage.ObjectInfo) string, error) {
	dst := r.sp.Target.Bucket
	set := storage.EncryptionSettings{Provider: storage.EncryptionAES256}
	raw, err := getConfig(ctx, r.s.router, dst, storage.ConfigEncryptionProvider)
	if err != nil {
		return nil, fmt.Errorf("reading the target's encryption settings: %w", err)
	}
	if raw != nil {
		if set, err = storage.ParseEncryptionSettings(raw); err != nil {
			return nil, fmt.Errorf("the target bucket %q: %w", dst, err)
		}
	}
	def, err := getConfig(ctx, r.s.router, dst, storage.ConfigEncryption)
	if err != nil {
		return nil, fmt.Errorf("reading the target's default encryption: %w", err)
	}
	byDefault := defaultSSE(def)
	return func(v storage.ObjectInfo) string {
		switch {
		case set.Provider == storage.EncryptionRestic && set.Scope == storage.EncryptionScopeAll:
			return storage.EncryptionRestic
		case v.SSE != "" || byDefault:
			return set.Provider
		}
		return ""
	}, nil
}

// defaultSSE reports whether a stored default encryption document
// (storage.ConfigEncryption) asks for encryption (AES256, or a KMS
// algorithm, which gos3 applies as SSE-S3).
func defaultSSE(doc []byte) bool {
	if len(doc) == 0 {
		return false
	}
	var c struct {
		Rules []struct {
			Default *struct {
				SSEAlgorithm string `xml:"SSEAlgorithm"`
			} `xml:"ApplyServerSideEncryptionByDefault"`
		} `xml:"Rule"`
	}
	if err := xml.Unmarshal(doc, &c); err != nil {
		return false
	}
	for _, rule := range c.Rules {
		if rule.Default == nil {
			continue
		}
		if a := strings.TrimSpace(rule.Default.SSEAlgorithm); a == "AES256" || strings.HasPrefix(a, "aws:kms") {
			return true
		}
	}
	return false
}

// targetCompression returns how the target stores a version with
// compression "target": as the S3 layer decides for a new object of the
// target bucket (compression.Decide).
func (r *run) targetCompression(ctx context.Context) (func(storage.ObjectInfo) storage.Compression, error) {
	setting, err := compression.Load(ctx, r.s.router, r.sp.Target.Bucket)
	if err != nil && !errors.Is(err, storage.ErrConfigNotFound) {
		if errors.Is(err, storage.ErrBackingUnavailable) {
			return nil, err
		}
		r.log.Warn("copy job: ignoring the target's unreadable compression setting", "bucket", r.sp.Target.Bucket, "error", err)
		setting = compression.BucketSetting{}
	}
	p := r.s.compressionPolicy()
	enabled := compression.Effective(p, setting)
	return func(v storage.ObjectInfo) storage.Compression {
		return compression.Decide(p, enabled, compression.Object{ContentType: v.ContentType, ContentEncoding: v.ContentEncoding, Size: v.Size})
	}, nil
}

// compressionPolicy returns the server's compression policy.
func (s *Service) compressionPolicy() config.CompressionSettings {
	if s.compression != nil {
		return s.compression()
	}
	return config.CompressionSettings{
		Mode:    config.CompressionOptional,
		Level:   config.DefaultCompressionLevel,
		MinSize: config.DefaultCompressionMinSize,
	}
}
