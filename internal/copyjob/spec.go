package copyjob

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
)

// Spec is a copy or mirror job's specification (CopyJobSpec of the admin
// API). Validate returns it normalized: every option set (those of the
// other kind left out), durations in canonical form.
type Spec struct {
	// Kind is KindCopy or KindMirror.
	Kind    string      `json:"kind"`
	Source  SourceSpec  `json:"source"`
	Target  TargetSpec  `json:"target"`
	Options SpecOptions `json:"options"`
	DryRun  bool        `json:"dryRun,omitempty"`
	// BucketBindings are set by validation and pin the bucket incarnations
	// selected when the job was submitted. They are not client options.
	BucketBindings bucketBindings `json:"bucketBindings,omitempty"`
}

type bucketBindings struct {
	Source bucketIdentity `json:"source"`
	Target bucketIdentity `json:"target,omitempty"`
}

type bucketIdentity struct {
	InstanceID string             `json:"instanceID"`
	CreatedAt  time.Time          `json:"createdAt"`
	Owner      string             `json:"owner,omitempty"`
	Backing    string             `json:"backing,omitempty"`
	Kind       storage.BucketKind `json:"kind,omitempty"`
	Location   string             `json:"location,omitempty"`
}

func identityOf(info storage.BucketInfo) bucketIdentity {
	return bucketIdentity{InstanceID: info.InstanceID, CreatedAt: info.CreatedAt, Owner: info.Owner, Backing: info.Backing, Kind: info.Kind, Location: info.Location}
}

func (id bucketIdentity) empty() bool {
	return id.InstanceID == "" && id.CreatedAt.IsZero() && id.Owner == "" && id.Backing == "" && id.Kind == "" && id.Location == ""
}

func (id bucketIdentity) matches(info storage.BucketInfo) bool {
	got := identityOf(info)
	if id.InstanceID == "" || id.InstanceID != got.InstanceID {
		return false
	}
	// A router placement can move or fail over without replacing the
	// logical bucket. Native S3 buckets can also be replaced behind the
	// router, so pin their remote creation time and location as well.
	if id.Kind == storage.BucketKindNative || got.Kind == storage.BucketKindNative {
		return id.Kind == got.Kind && id.CreatedAt.Equal(got.CreatedAt) && id.Location == got.Location
	}
	return true
}

// SourceSpec is the range of keys copied.
type SourceSpec struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
}

// TargetSpec is where the keys are copied: source key Prefix+k becomes
// target key Target.Prefix+k.
type TargetSpec struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
	// Create creates the target bucket when it does not exist.
	Create *CreateSpec `json:"create,omitempty"`
}

// CreateSpec says how a missing target bucket is created.
type CreateSpec struct {
	// Backing names the backing ("" = the default backing).
	Backing string `json:"backing,omitempty"`
	// Kind is the bucket kind on backings that offer several ("native" or
	// "wrapped"; "" = wrapped where that is possible, so that versions
	// keep their identity).
	Kind string `json:"kind,omitempty"`
	// Container is the container of a wrapped bucket and RemoteBucket the
	// remote bucket of a native one.
	Container    string `json:"container,omitempty"`
	RemoteBucket string `json:"remoteBucket,omitempty"`
	// ObjectLock enables object lock on the new bucket (normalized: by
	// default when the source has it and the new bucket can).
	ObjectLock *bool `json:"objectLock,omitempty"`
}

// Option values.
const (
	VersionsAll     = "all"
	VersionsCurrent = "current"

	ConfigsSafe = "safe"
	ConfigsAll  = "all"
	ConfigsNone = "none"

	OnExistingFail  = "fail"
	OnExistingMerge = "merge"
	OnExistingAdopt = "adopt"

	EncryptionPreserve = "preserve"
	EncryptionTarget   = "target"

	CompressionPreserve = "preserve"
	CompressionTarget   = "target"
)

// Option defaults and bounds.
const (
	DefaultConcurrency    = 4
	MaxSpecConcurrency    = 32
	DefaultPollInterval   = 5 * time.Minute
	MinPollInterval       = 30 * time.Second
	DefaultVerifyInterval = 24 * time.Hour
	MinVerifyInterval     = time.Minute
)

// SpecOptions are a job's options (see the admin API). Pointers tell an
// absent option from false.
type SpecOptions struct {
	Versions       string `json:"versions"`
	Configs        string `json:"configs"`
	OnExisting     string `json:"onExisting"`
	Encryption     string `json:"encryption"`
	AllowPlaintext bool   `json:"allowPlaintext"`
	DropRetention  bool   `json:"dropRetention"`
	Compression    string `json:"compression"`
	// Mirror options.
	PropagateDeletes *bool  `json:"propagateDeletes,omitempty"`
	DeleteExtraneous *bool  `json:"deleteExtraneous,omitempty"`
	PollInterval     string `json:"pollInterval,omitempty"`
	VerifyInterval   string `json:"verifyInterval,omitempty"`
	Concurrency      int    `json:"concurrency"`
	// Copy option.
	FinalPass *bool `json:"finalPass,omitempty"`
}

func (o SpecOptions) propagateDeletes() bool { return o.PropagateDeletes == nil || *o.PropagateDeletes }
func (o SpecOptions) deleteExtraneous() bool { return o.DeleteExtraneous != nil && *o.DeleteExtraneous }
func (o SpecOptions) finalPass() bool        { return o.FinalPass == nil || *o.FinalPass }

func (o SpecOptions) pollInterval() time.Duration {
	if d, err := time.ParseDuration(o.PollInterval); err == nil && d > 0 {
		return d
	}
	return DefaultPollInterval
}

func (o SpecOptions) verifyInterval() time.Duration {
	if d, err := time.ParseDuration(o.VerifyInterval); err == nil && d > 0 {
		return d
	}
	return DefaultVerifyInterval
}

// all reports whether every version is copied.
func (sp *Spec) all() bool { return sp.Options.Versions != VersionsCurrent }

func (sp *Spec) mirror() bool { return sp.Kind == KindMirror }

func (sp *Spec) srcRange() keyRange { return keyRange{sp.Source.Bucket, sp.Source.Prefix} }
func (sp *Spec) dstRange() keyRange { return keyRange{sp.Target.Bucket, sp.Target.Prefix} }

// endpoints returns the copier endpoints of the spec on the router.
func (s *Service) endpoints(sp *Spec) (src, dst copier.Endpoint) {
	return copier.Endpoint{Backend: s.router, Bucket: sp.Source.Bucket, Prefix: sp.Source.Prefix},
		copier.Endpoint{Backend: s.router, Bucket: sp.Target.Bucket, Prefix: sp.Target.Prefix}
}

// invalidf returns an error wrapping jobs.ErrInvalidSpec.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", jobs.ErrInvalidSpec, fmt.Sprintf(format, args...))
}

// conflictf returns an error wrapping jobs.ErrConflict.
func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", jobs.ErrConflict, fmt.Sprintf(format, args...))
}

// decodeSpec decodes raw strictly: unknown fields and trailing data are
// errors.
func decodeSpec(raw []byte) (Spec, error) {
	var sp Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sp); err != nil {
		return Spec{}, invalidf("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Spec{}, invalidf("unexpected data after the spec")
	}
	return sp, nil
}

// normalize checks the parts of sp that need no I/O and fills in their
// defaults. kind is the runner's kind ("" for a dry run: the spec's).
// Versions and Create.ObjectLock depend on the buckets: check fills them in.
func normalize(sp *Spec, kind string) error {
	switch {
	case sp.Kind == "" && kind == "":
		return invalidf("kind is required (%q or %q)", KindCopy, KindMirror)
	case sp.Kind == "":
		sp.Kind = kind
	case kind != "" && sp.Kind != kind:
		return invalidf("the spec's kind %q differs from the job's kind %q", sp.Kind, kind)
	}
	if sp.Kind != KindCopy && sp.Kind != KindMirror {
		return invalidf("unknown kind %q (want %q or %q)", sp.Kind, KindCopy, KindMirror)
	}
	if sp.Source.Bucket == "" {
		return invalidf("source.bucket is required")
	}
	if sp.Target.Bucket == "" {
		return invalidf("target.bucket is required")
	}
	for _, p := range []struct{ name, v string }{{"source.prefix", sp.Source.Prefix}, {"target.prefix", sp.Target.Prefix}} {
		if len(p.v) > copier.MaxKeyLen || !utf8.ValidString(p.v) {
			return invalidf("%s must be valid UTF-8 of at most %d bytes", p.name, copier.MaxKeyLen)
		}
	}
	if sp.srcRange().overlaps(sp.dstRange()) {
		return invalidf("the source and the target are the same keys (bucket %q, prefixes %q and %q): a bucket cannot be copied onto itself",
			sp.Source.Bucket, sp.Source.Prefix, sp.Target.Prefix)
	}
	if c := sp.Target.Create; c != nil {
		switch storage.BucketKind(c.Kind) {
		case "", storage.BucketKindNative, storage.BucketKindWrapped:
		default:
			return invalidf("target.create.kind must be %q or %q", storage.BucketKindNative, storage.BucketKindWrapped)
		}
	}

	o := &sp.Options
	var err error
	if o.Versions, err = oneOf("versions", o.Versions, "", VersionsAll, VersionsCurrent); err != nil {
		return err
	}
	if o.Configs, err = oneOf("configs", o.Configs, ConfigsSafe, ConfigsSafe, ConfigsAll, ConfigsNone); err != nil {
		return err
	}
	if o.OnExisting, err = oneOf("onExisting", o.OnExisting, OnExistingFail, OnExistingFail, OnExistingMerge, OnExistingAdopt); err != nil {
		return err
	}
	if o.OnExisting == OnExistingAdopt && !sp.mirror() {
		return invalidf("onExisting %q is for mirrors only (a copy uses %q or %q)", OnExistingAdopt, OnExistingFail, OnExistingMerge)
	}
	if o.Encryption, err = oneOf("encryption", o.Encryption, EncryptionPreserve, EncryptionPreserve, EncryptionTarget); err != nil {
		return err
	}
	if o.Compression, err = oneOf("compression", o.Compression, CompressionPreserve, CompressionPreserve, CompressionTarget); err != nil {
		return err
	}
	switch {
	case o.Concurrency == 0:
		o.Concurrency = DefaultConcurrency
	case o.Concurrency < 1 || o.Concurrency > MaxSpecConcurrency:
		return invalidf("concurrency must be between 1 and %d", MaxSpecConcurrency)
	}

	if !sp.mirror() {
		// Mirror options mean nothing to a copy.
		o.PropagateDeletes, o.DeleteExtraneous, o.PollInterval, o.VerifyInterval = nil, nil, "", ""
		fp := o.finalPass()
		o.FinalPass = &fp
		return nil
	}
	o.FinalPass = nil
	pd, de := o.propagateDeletes(), o.deleteExtraneous()
	o.PropagateDeletes, o.DeleteExtraneous = &pd, &de
	if o.PollInterval, err = durationOption("pollInterval", o.PollInterval, DefaultPollInterval, MinPollInterval); err != nil {
		return err
	}
	if o.VerifyInterval, err = durationOption("verifyInterval", o.VerifyInterval, DefaultVerifyInterval, MinVerifyInterval); err != nil {
		return err
	}
	return nil
}

// oneOf returns v (def when empty) if it is one of allowed ("" stays ""
// when def is "": decided later).
func oneOf(name, v, def string, allowed ...string) (string, error) {
	if v == "" {
		v = def
	}
	if v == "" || slices.Contains(allowed, v) {
		return v, nil
	}
	return "", invalidf("%s must be one of %q", name, allowed)
}

// durationOption parses a duration option (def when empty), at least min,
// in canonical form ("5m", "24h", "1h30m").
func durationOption(name, v string, def, min time.Duration) (string, error) {
	d := def
	if v != "" {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return "", invalidf("%s: %v", name, err)
		}
	}
	if d < min {
		return "", invalidf("%s must be at least %s", name, formatDuration(min))
	}
	return formatDuration(d), nil
}

// formatDuration formats d without trailing zero units ("5m" rather than
// "5m0s").
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// validBucketName reports whether name is a valid name for a new bucket
// (the S3 rules the S3 layer applies to CreateBucket): 3-63 characters of
// lower-case letters, digits, '-' and '.', starting and ending with a
// letter or digit, not an IPv4 address, no "..", ".-" or "-.", and none of
// the prefixes and suffixes AWS reserves.
func validBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '.':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return false
	}
	if looksLikeIPv4(name) {
		return false
	}
	for _, p := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	for _, sfx := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(name, sfx) {
			return false
		}
	}
	return true
}

// looksLikeIPv4 reports whether name has the form of an IPv4 address
// (four dot-separated groups of 1-3 digits).
func looksLikeIPv4(name string) bool {
	parts := strings.Split(name, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// keyRange is the keys of a bucket that start with a prefix.
type keyRange struct {
	bucket, prefix string
}

// overlaps reports whether two ranges share keys.
func (a keyRange) overlaps(b keyRange) bool {
	return a.bucket == b.bucket && (strings.HasPrefix(a.prefix, b.prefix) || strings.HasPrefix(b.prefix, a.prefix))
}

func (a keyRange) String() string {
	if a.prefix == "" {
		return a.bucket
	}
	return a.bucket + "/" + a.prefix
}
