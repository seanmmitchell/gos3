package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bucket kinds of an s3 backing (S3Config.DefaultKind), the values of
// storage.BucketKind.
const (
	// BucketKindWrapped buckets are stored in gos3's own format inside a
	// container, by default the backing's home (the shared container).
	BucketKindWrapped = "wrapped"
	// BucketKindNative buckets map to one remote bucket each, whose
	// objects are stored as-is.
	BucketKindNative = "native"
)

// S3Config is the connection to a remote S3 service of an s3 backing: the
// default backing (StorageConfig.S3, with storage.backend s3) or an
// additional one (BackingConfig.S3, with backend s3). See docs/backings.md.
//
// The backing holds native buckets (one remote bucket each) and wrapped
// buckets (gos3's format inside a container). Its optional home, HomePrefix
// in HomeBucket, holds gos3's records for the backing and the shared container
// of its wrapped buckets; without a home the records live in the server's
// system directory (Config.SystemDir), which is then required.
type S3Config struct {
	// Endpoint is the service URL, http(s)://host[:port] (no path).
	Endpoint string `yaml:"endpoint"`
	// Region is the signing region ("" = us-east-1).
	Region string `yaml:"region"`
	// HomeBucket is the remote bucket holding the backing's home ("" = no
	// home: the records live in the system directory and there is no
	// shared container).
	HomeBucket string `yaml:"home_bucket"`
	// HomePrefix is prepended to the home's remote keys: "" or a key
	// prefix ending in "/" (Normalize appends the slash). It requires
	// HomeBucket.
	HomePrefix string `yaml:"home_prefix"`
	// Addressing is "path" (<endpoint>/<bucket>/<key>), "virtual"
	// (<bucket>.<host>/<key>) or "auto"/"" (virtual for amazonaws.com
	// endpoints, path otherwise; Normalize turns "auto" into "").
	Addressing string `yaml:"addressing"`
	// PartSize is the multipart part size for uploads to the service and
	// the largest object sent with one PUT (0 = default 16 MiB; 5 MiB-5 GiB).
	PartSize int64 `yaml:"part_size"`
	// AccessKey / SecretKey authenticate to the service. When empty they
	// are read from the environment (see BackingEnv).
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// EncryptionKey is the key-encryption key (KEK) wrapping the per-bucket
	// master keys of the backing's wrapped buckets: standard base64 of
	// exactly 32 bytes. Without it, neither AES256 nor restic encryption is
	// offered on the backing. When empty it is read from the environment
	// (see BackingEnv).
	EncryptionKey string `yaml:"encryption_key"`
	// DefaultKind is the kind of the buckets created on the backing without
	// an explicit kind: BucketKindWrapped (requires a home) or
	// BucketKindNative. Normalize fills an empty value from the home:
	// wrapped with a home, native without (see EffectiveDefaultKind).
	DefaultKind string `yaml:"default_kind"`

	// LegacyBucket, LegacyPrefix and LegacyGateway are the deprecated keys
	// bucket, prefix and gateway, accepted on input only. Normalize moves
	// bucket and prefix to HomeBucket and HomePrefix, and gateway: true to
	// DefaultKind native, and records it for Config.Deprecations; a value
	// that conflicts with the new key is left in place, and Validate
	// refuses it.
	LegacyBucket  string `yaml:"bucket,omitempty"`
	LegacyPrefix  string `yaml:"prefix,omitempty"`
	LegacyGateway bool   `yaml:"gateway,omitempty"`

	// fromEnv records the fields whose values were taken from the
	// environment (bit i = s3EnvFields[i]); such values are never written
	// to backings.json.
	fromEnv uint16
	// legacy records the deprecated keys that were mapped to their
	// replacements (legacyBucket, ...), for Config.Deprecations.
	legacy uint8
}

// Bits of S3Config.legacy.
const (
	legacyBucket uint8 = 1 << iota
	legacyPrefix
	legacyGateway
)

// Limits of S3Config.PartSize.
const (
	MinBackingPartSize = 5 << 20
	MaxBackingPartSize = 5 << 30
)

// MaxS3PrefixLen bounds S3Config.HomePrefix (bytes).
const MaxS3PrefixLen = 512

// KEKSize is the length of the decoded S3Config.EncryptionKey.
const KEKSize = 32

// remoteBucketRe accepts the bucket names of S3 services (stricter rules
// are the service's business).
var remoteBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-_]{1,254}$`)

// PathStyle reports whether requests to the service use path-style
// addressing (see Addressing).
func (s S3Config) PathStyle() bool {
	switch s.Addressing {
	case "path":
		return true
	case "virtual":
		return false
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return host != "amazonaws.com" && !strings.HasSuffix(host, ".amazonaws.com")
}

// IsZero reports whether no setting is configured.
func (s S3Config) IsZero() bool {
	return s.Endpoint == "" && s.Region == "" && s.HomeBucket == "" && s.HomePrefix == "" && s.Addressing == "" &&
		s.PartSize == 0 && s.AccessKey == "" && s.SecretKey == "" && s.EncryptionKey == "" && s.DefaultKind == "" &&
		s.LegacyBucket == "" && s.LegacyPrefix == "" && !s.LegacyGateway
}

// HasHome reports whether the backing has a home (HomeBucket is set).
func (s S3Config) HasHome() bool { return s.HomeBucket != "" }

// EffectiveDefaultKind returns DefaultKind or, when it is empty, the
// default for the backing's home: BucketKindWrapped with a home,
// BucketKindNative without.
func (s S3Config) EffectiveDefaultKind() string {
	if s.DefaultKind != "" {
		return s.DefaultKind
	}
	return defaultKindFor(s.HomeBucket)
}

// defaultKindFor is the default kind of a backing whose home bucket is
// homeBucket ("" = no home).
func defaultKindFor(homeBucket string) string {
	if homeBucket != "" {
		return BucketKindWrapped
	}
	return BucketKindNative
}

// Clone returns a copy of s (nil for nil), provenance included.
func (s *S3Config) Clone() *S3Config {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// redacted returns a copy of s (nil for nil) with the secret and
// encryption keys replaced by RedactedValue when set.
func (s *S3Config) redacted() *S3Config {
	cp := s.Clone()
	if cp != nil {
		if cp.SecretKey != "" {
			cp.SecretKey = RedactedValue
		}
		if cp.EncryptionKey != "" {
			cp.EncryptionKey = RedactedValue
		}
	}
	return cp
}

// KEK decodes EncryptionKey: nil, nil when it is empty.
func (s S3Config) KEK() ([]byte, error) {
	if s.EncryptionKey == "" {
		return nil, nil
	}
	k, err := base64.StdEncoding.DecodeString(s.EncryptionKey)
	if err != nil || len(k) != KEKSize {
		return nil, fmt.Errorf("encryption_key: must be standard base64 of exactly %d bytes", KEKSize)
	}
	return k, nil
}

// Location describes where the backing keeps its records, without secrets
// or the endpoint: "s3://<home_bucket>/<home_prefix>" with a home, "local:"
// (the server's system directory) without.
func (s S3Config) Location() string {
	if !s.HasHome() {
		return "local:"
	}
	return "s3://" + s.HomeBucket + "/" + s.HomePrefix
}

// identity is the part of Identity after "s3:" for the backing name:
// "home:<home bucket, lower-cased>/<home prefix>" with a home, which
// defines the data the backing holds, or "local:<name>" for a backing
// whose records live in the system directory.
func (s S3Config) identity(name string) string {
	if !s.HasHome() {
		return "local:" + name
	}
	return "home:" + strings.ToLower(s.HomeBucket) + "/" + s.HomePrefix
}

// s3EnvFields are the S3Config YAML keys that BackingEnv variables supply.
var s3EnvFields = []string{"endpoint", "region", "home_bucket", "home_prefix", "addressing", "part_size",
	"access_key", "secret_key", "encryption_key", "default_kind"}

// s3EnvAliases maps s3EnvFields entries to the deprecated BackingEnv field
// still accepted for them (gateway: true means default_kind native).
var s3EnvAliases = map[string]string{"home_bucket": "bucket", "home_prefix": "prefix", "default_kind": "gateway"}

// s3FieldBit returns the fromEnv bit of an s3EnvFields entry.
func s3FieldBit(field string) uint16 {
	for i, f := range s3EnvFields {
		if f == field {
			return 1 << i
		}
	}
	panic("config: unknown s3 field " + field)
}

// isEnv reports whether field (an s3EnvFields entry) was taken from the
// environment.
func (s S3Config) isEnv(field string) bool { return s.fromEnv&s3FieldBit(field) != 0 }

// setEnv records whether field was taken from the environment.
func (s *S3Config) setEnv(field string, on bool) {
	if on {
		s.fromEnv |= s3FieldBit(field)
	} else {
		s.fromEnv &^= s3FieldBit(field)
	}
}

// EnvFields returns the YAML keys of the fields whose values were taken
// from the environment (BackingEnv), in s3EnvFields order.
func (s S3Config) EnvFields() []string {
	var out []string
	for _, f := range s3EnvFields {
		if s.isEnv(f) {
			out = append(out, f)
		}
	}
	return out
}

// fieldValue returns the value of field (an s3EnvFields entry) in a
// comparable string form.
func (s S3Config) fieldValue(field string) string {
	switch field {
	case "endpoint":
		return s.Endpoint
	case "region":
		return s.Region
	case "home_bucket":
		return s.HomeBucket
	case "home_prefix":
		return s.HomePrefix
	case "addressing":
		return s.Addressing
	case "part_size":
		return strconv.FormatInt(s.PartSize, 10)
	case "access_key":
		return s.AccessKey
	case "secret_key":
		return s.SecretKey
	case "encryption_key":
		return s.EncryptionKey
	case "default_kind":
		return s.DefaultKind
	}
	panic("config: unknown s3 field " + field)
}

// clearField sets field (an s3EnvFields entry) to its zero value.
func (s *S3Config) clearField(field string) {
	switch field {
	case "endpoint":
		s.Endpoint = ""
	case "region":
		s.Region = ""
	case "home_bucket":
		s.HomeBucket = ""
	case "home_prefix":
		s.HomePrefix = ""
	case "addressing":
		s.Addressing = ""
	case "part_size":
		s.PartSize = 0
	case "access_key":
		s.AccessKey = ""
	case "secret_key":
		s.SecretKey = ""
	case "encryption_key":
		s.EncryptionKey = ""
	case "default_kind":
		s.DefaultKind = ""
	}
}

// BackingEnv returns the name of the environment variable supplying the
// S3Config field (its YAML key, e.g. "secret_key") of the s3 backing name
// when the field is not configured: GOS3_BACKING_<NAME>_<FIELD> (NAME and
// FIELD upper-cased, '-' as '_'). For DefaultBackingName it is the
// GOS3_S3_<FIELD> variable of storage.s3.
func BackingEnv(name, field string) string {
	f := strings.ToUpper(strings.ReplaceAll(field, "-", "_"))
	if name == DefaultBackingName {
		return "GOS3_S3_" + f
	}
	return "GOS3_BACKING_" + backingEnvName(name) + "_" + f
}

// backingEnvName is the NAME part of the GOS3_BACKING_<NAME>_* variables.
func backingEnvName(name string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
}

// applyBackingEnv fills the empty fields of every s3 backing in
// storage.backings from its GOS3_BACKING_<NAME>_* variables (see
// BackingEnv), allocating the s3 block of an s3 backing without one, and
// records which fields came from the environment. For the default backing
// (storage.s3, set through the ordinary GOS3_S3_* table entries) it only
// records that provenance for the credentials.
func (s *StorageConfig) applyBackingEnv(lookup func(string) (string, bool)) error {
	if d := s.S3; d != nil {
		for _, f := range []string{"access_key", "secret_key", "encryption_key"} {
			e, ok := lookup(BackingEnv(DefaultBackingName, f))
			v := d.fieldValue(f)
			d.setEnv(f, ok && v != "" && strings.TrimSpace(e) == v)
		}
	}
	for i := range s.Backings {
		b := &s.Backings[i]
		if !strings.EqualFold(strings.TrimSpace(b.Backend), BackendS3) {
			continue
		}
		if b.S3 == nil {
			b.S3 = &S3Config{}
		}
		if err := b.S3.fillFromEnv(strings.ToLower(strings.TrimSpace(b.Name)), lookup); err != nil {
			return err
		}
	}
	return nil
}

// envValue returns the trimmed value of the environment variable name,
// and whether it is set to a non-empty value.
func envValue(lookup func(string) (string, bool), name string) (string, bool) {
	raw, ok := lookup(name)
	v := strings.TrimSpace(raw)
	return v, ok && v != ""
}

// fillFromEnv sets the empty fields of s from the BackingEnv variables of
// backing name. The deprecated variables of s3EnvAliases fill the same
// fields as their replacements (an alias that disagrees with a set
// replacement is an error), and a field counts as configured when its
// deprecated key is set.
func (s *S3Config) fillFromEnv(name string, lookup func(string) (string, bool)) error {
	for _, field := range s3EnvFields {
		env := BackingEnv(name, field)
		v, ok := envValue(lookup, env)
		alias := false // the deprecated variable is set (and agrees)
		if old, has := s3EnvAliases[field]; has {
			oldEnv := BackingEnv(name, old)
			if ov, set := envValue(lookup, oldEnv); set {
				if field == "default_kind" {
					// gateway: true is default_kind native; false says nothing.
					gw, err := ParseBool(ov)
					if err != nil {
						return fmt.Errorf("%s: %w", oldEnv, err)
					}
					ov, set = BucketKindNative, gw
				}
				switch {
				case set && ok && !sameS3Setting(field, v, ov):
					return fmt.Errorf("%s: conflicts with %s (deprecated); set only %s", env, oldEnv, env)
				case set && !ok:
					v, ok, env = ov, true, oldEnv
				}
				alias = set
			}
		}
		if !ok {
			continue
		}
		var err error
		filled := true
		switch field {
		case "endpoint":
			filled = fillString(&s.Endpoint, v)
		case "region":
			filled = fillString(&s.Region, v)
		case "home_bucket":
			filled = s.LegacyBucket == "" && fillString(&s.HomeBucket, v)
		case "home_prefix":
			filled = s.LegacyPrefix == "" && fillString(&s.HomePrefix, v)
		case "addressing":
			filled = fillString(&s.Addressing, v)
		case "part_size":
			if filled = s.PartSize == 0; filled {
				s.PartSize, err = ParseSize(v)
			}
		case "access_key":
			filled = fillString(&s.AccessKey, v)
		case "secret_key":
			filled = fillString(&s.SecretKey, v)
		case "encryption_key":
			filled = fillString(&s.EncryptionKey, v)
		case "default_kind":
			filled = !s.LegacyGateway && fillString(&s.DefaultKind, v)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", env, err)
		}
		if filled {
			s.setEnv(field, true)
			if alias {
				s.legacy |= legacyBits[field]
			}
		}
	}
	return nil
}

// legacyBits maps s3EnvAliases keys to their S3Config.legacy bit.
var legacyBits = map[string]uint8{"home_bucket": legacyBucket, "home_prefix": legacyPrefix, "default_kind": legacyGateway}

// sameS3Setting reports whether two raw values of field (an s3EnvFields
// entry) are the same once normalized.
func sameS3Setting(field, a, b string) bool {
	switch field {
	case "home_prefix":
		return normalizePrefix(a) == normalizePrefix(b)
	case "default_kind":
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

// fillString sets *dst to v when *dst is empty, reporting whether it did.
func fillString(dst *string, v string) bool {
	if *dst != "" {
		return false
	}
	*dst = v
	return true
}

// redactEndpoint returns endpoint with any user information (which may
// hold credentials) replaced by RedactedValue, for error messages.
func redactEndpoint(endpoint string) string {
	start := 0
	if i := strings.Index(endpoint, "://"); i >= 0 {
		start = i + 3
	}
	authority := endpoint[start:]
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return endpoint
	}
	return endpoint[:start] + RedactedValue + endpoint[start+at:]
}

// normalizePrefix trims p and makes a non-empty prefix end in "/".
func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// normalize trims the values, lower-cases enumerations ("auto" addressing
// becomes ""), drops trailing slashes from the endpoint, makes non-empty
// prefixes end in "/" and maps the deprecated keys (see mapLegacy). It
// leaves an empty DefaultKind alone: see Config.normalizeS3.
func (s *S3Config) normalize() {
	s.Endpoint = strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	s.Region = strings.TrimSpace(s.Region)
	s.HomeBucket = strings.TrimSpace(s.HomeBucket)
	s.HomePrefix = normalizePrefix(s.HomePrefix)
	s.Addressing = strings.ToLower(strings.TrimSpace(s.Addressing))
	if s.Addressing == "auto" {
		s.Addressing = ""
	}
	s.AccessKey = strings.TrimSpace(s.AccessKey)
	s.SecretKey = strings.TrimSpace(s.SecretKey)
	s.EncryptionKey = strings.TrimSpace(s.EncryptionKey)
	s.DefaultKind = strings.ToLower(strings.TrimSpace(s.DefaultKind))
	s.LegacyBucket = strings.TrimSpace(s.LegacyBucket)
	s.LegacyPrefix = normalizePrefix(s.LegacyPrefix)
	s.mapLegacy()
}

// mapLegacy moves the deprecated keys to their replacements (bucket to
// HomeBucket, prefix to HomePrefix, gateway: true to DefaultKind native)
// when these are empty or equal, recording each move in s.legacy. A
// conflicting value stays in place for validateS3 to refuse.
func (s *S3Config) mapLegacy() {
	if s.LegacyBucket != "" && (s.HomeBucket == "" || s.HomeBucket == s.LegacyBucket) {
		s.HomeBucket, s.LegacyBucket = s.LegacyBucket, ""
		s.legacy |= legacyBucket
	}
	if s.LegacyPrefix != "" && (s.HomePrefix == "" || s.HomePrefix == s.LegacyPrefix) {
		s.HomePrefix, s.LegacyPrefix = s.LegacyPrefix, ""
		s.legacy |= legacyPrefix
	}
	if s.LegacyGateway && (s.DefaultKind == "" || s.DefaultKind == BucketKindNative) {
		s.DefaultKind, s.LegacyGateway = BucketKindNative, false
		s.legacy |= legacyGateway
	}
}

// fillDefaultKind sets an empty DefaultKind to the default for the home
// (see EffectiveDefaultKind).
func (s *S3Config) fillDefaultKind() {
	if s.DefaultKind == "" {
		s.DefaultKind = defaultKindFor(s.HomeBucket)
	}
}

// normalizeS3 normalizes storage.s3 and the s3 block of every backing, and
// fills the empty default kinds of s3 backings (storage.s3 only with
// storage.backend s3). It runs once every layer is in: the environment
// only fills empty fields, so a kind derived earlier would hide
// GOS3_BACKING_<NAME>_DEFAULT_KIND and ignore a home taken from there.
func (c *Config) normalizeS3() {
	if s := c.Storage.S3; s != nil {
		s.normalize()
		if c.Storage.Backend == BackendS3 {
			s.fillDefaultKind()
		}
	}
	for i := range c.Storage.Backings {
		b := &c.Storage.Backings[i]
		if b.S3 != nil {
			b.S3.normalize()
			if strings.EqualFold(strings.TrimSpace(b.Backend), BackendS3) {
				b.S3.fillDefaultKind()
			}
		}
	}
}

// Deprecation is a deprecated s3 setting that was mapped to its
// replacement (see Config.Deprecations).
type Deprecation struct {
	// Backing is the name of the s3 backing (DefaultBackingName for
	// storage.s3).
	Backing string
	// Key is the deprecated S3Config key: "bucket", "prefix" or "gateway".
	Key string
	// Use is the replacement: "home_bucket", "home_prefix" or
	// "default_kind: native".
	Use string
}

// String describes d for logs, naming the environment variables too.
func (d Deprecation) String() string {
	var newEnv string
	if d.Key == "gateway" {
		newEnv = BackingEnv(d.Backing, "default_kind") + "=" + BucketKindNative
	} else {
		newEnv = BackingEnv(d.Backing, d.Use)
	}
	return fmt.Sprintf("s3 backing %q: %s (%s) is deprecated, use %s (%s)",
		d.Backing, d.Key, BackingEnv(d.Backing, d.Key), d.Use, newEnv)
}

// deprecations returns the deprecated keys mapped by normalize or taken
// from deprecated variables, for backing name.
func (s *S3Config) deprecations(name string) []Deprecation {
	if s == nil {
		return nil
	}
	var out []Deprecation
	for _, d := range []struct {
		bit      uint8
		key, use string
	}{
		{legacyBucket, "bucket", "home_bucket"},
		{legacyPrefix, "prefix", "home_prefix"},
		{legacyGateway, "gateway", "default_kind: " + BucketKindNative},
	} {
		if s.legacy&d.bit != 0 {
			out = append(out, Deprecation{Backing: name, Key: d.key, Use: d.use})
		}
	}
	return out
}

// Deprecations lists the deprecated s3 settings (bucket, prefix and
// gateway, in the configuration file, backings.json or the environment)
// that Normalize or LoadBackings mapped to their replacements, storage.s3
// first. They keep working; servers log each one at startup, so that the
// configuration can be updated (gos3 config print shows the new keys).
func (c Config) Deprecations() []Deprecation {
	out := c.Storage.S3.deprecations(DefaultBackingName)
	for _, b := range c.Storage.Backings {
		out = append(out, b.S3.deprecations(b.Name)...)
	}
	return out
}

// validateS3 checks the s3 settings of backing name, configured at key
// (e.g. "storage.s3"); systemDir is the server's system directory ("" =
// none), which holds the records of a backing without a home. Errors
// start with the offending key.
func validateS3(key, name string, s S3Config, systemDir string) error {
	u, err := url.Parse(s.Endpoint)
	switch {
	case s.Endpoint == "":
		return fmt.Errorf("%s.endpoint: required (or set %s)", key, BackingEnv(name, "endpoint"))
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil:
		return fmt.Errorf("%s.endpoint: %q must be an absolute http:// or https:// URL without a path or user information", key, redactEndpoint(s.Endpoint))
	}
	if s.Region != "" && !regionRe.MatchString(s.Region) {
		return fmt.Errorf("%s.region: %q must be lowercase letters, digits and '-'", key, s.Region)
	}
	// Deprecated keys left in place by mapLegacy conflict with the new ones.
	if s.LegacyBucket != "" {
		return fmt.Errorf("%s.bucket: the deprecated key bucket (%q) conflicts with home_bucket (%q); set only home_bucket (%s)",
			key, s.LegacyBucket, s.HomeBucket, BackingEnv(name, "home_bucket"))
	}
	if s.LegacyPrefix != "" {
		return fmt.Errorf("%s.prefix: the deprecated key prefix (%q) conflicts with home_prefix (%q); set only home_prefix (%s)",
			key, s.LegacyPrefix, s.HomePrefix, BackingEnv(name, "home_prefix"))
	}
	if s.LegacyGateway {
		return fmt.Errorf("%s.gateway: the deprecated key gateway (native buckets) conflicts with default_kind %q; set only default_kind (%s)",
			key, s.DefaultKind, BackingEnv(name, "default_kind"))
	}
	if s.HomeBucket != "" && !remoteBucketRe.MatchString(s.HomeBucket) {
		return fmt.Errorf("%s.home_bucket: %q is not a valid bucket name", key, s.HomeBucket)
	}
	if s.HomePrefix != "" && s.HomeBucket == "" {
		return fmt.Errorf("%s.home_prefix: requires home_bucket (or set %s)", key, BackingEnv(name, "home_bucket"))
	}
	if err := validateS3Prefix(s.HomePrefix); err != nil {
		return fmt.Errorf("%s.home_prefix: %w", key, err)
	}
	if !s.HasHome() && systemDir == "" {
		return fmt.Errorf("%s: an s3 backing without home_bucket keeps its records in the system directory; set storage.path or home_bucket", key)
	}
	switch s.DefaultKind {
	case "", BucketKindNative:
	case BucketKindWrapped:
		if !s.HasHome() {
			return fmt.Errorf("%s.default_kind: wrapped buckets need a container: set home_bucket (or %s)", key, BackingEnv(name, "home_bucket"))
		}
	default:
		return fmt.Errorf("%s.default_kind: %q is invalid (expected %s or %s)", key, s.DefaultKind, BucketKindWrapped, BucketKindNative)
	}
	switch s.Addressing {
	case "", "path", "virtual":
	default:
		return fmt.Errorf("%s.addressing: %q is invalid (expected auto, path or virtual)", key, s.Addressing)
	}
	if s.PartSize != 0 && (s.PartSize < MinBackingPartSize || s.PartSize > MaxBackingPartSize) {
		return fmt.Errorf("%s.part_size: must be between %d (5 MiB) and %d (5 GiB) bytes, got %d", key, MinBackingPartSize, MaxBackingPartSize, s.PartSize)
	}
	if s.AccessKey == "" || s.SecretKey == "" {
		return fmt.Errorf("%s.access_key: access_key and secret_key are required (or set %s and %s)",
			key, BackingEnv(name, "access_key"), BackingEnv(name, "secret_key"))
	}
	if _, err := s.KEK(); err != nil {
		return fmt.Errorf("%s.%w (%s; e.g. \"openssl rand -base64 32\")", key, err, BackingEnv(name, "encryption_key"))
	}
	return nil
}

// validateS3Prefix checks a normalized prefix: at most MaxS3PrefixLen
// bytes of UTF-8, not starting with "/", without empty, "." or ".."
// segments.
func validateS3Prefix(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > MaxS3PrefixLen || !utf8.ValidString(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("must be valid UTF-8 of at most %d bytes, not starting with '/'", MaxS3PrefixLen)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%q has an empty, \".\" or \"..\" segment", p)
		}
	}
	return nil
}

// s3Place is the home of an s3 backing, for the overlap check.
type s3Place struct {
	key    string // configuration key, e.g. "storage.backings[1].s3"
	server string // lower-case host:port of the endpoint
	bucket string
	prefix string
}

// s3Server returns the lower-case host:port of an endpoint, with the
// scheme's default port when none is given.
func s3Server(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return strings.ToLower(endpoint)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(host, port)
}

// checkS3Overlap refuses two s3 backings whose homes are on the same
// service and bucket with equal or nested prefixes. Backings without a
// home are not listed: they have nothing remote of their own to overlap.
func checkS3Overlap(places []s3Place) error {
	for i, a := range places {
		for _, b := range places[:i] {
			if a.server == b.server && strings.EqualFold(a.bucket, b.bucket) &&
				(strings.HasPrefix(a.prefix, b.prefix) || strings.HasPrefix(b.prefix, a.prefix)) {
				return fmt.Errorf("%s.home_prefix: %q in bucket %q overlaps the home of %s (home_prefix %q); every s3 backing needs its own home: another home_bucket or a separate home_prefix",
					a.key, a.prefix, a.bucket, b.key, b.prefix)
			}
		}
	}
	return nil
}

// errNoS3 is returned for an s3 default backing without storage.s3.
var errNoS3 = errors.New("storage.s3: required for the s3 backend (endpoint and credentials)")
