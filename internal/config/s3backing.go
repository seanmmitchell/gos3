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

// S3Config is the remote S3 service holding an s3 backing: the default
// backing (StorageConfig.S3, with storage.backend s3) or an additional one
// (BackingConfig.S3, with backend s3). See docs/backings.md.
//
// All of the backing's data and metadata live under Prefix in Bucket; the
// backing needs no local directory.
type S3Config struct {
	// Endpoint is the service URL, http(s)://host[:port] (no path).
	Endpoint string `yaml:"endpoint"`
	// Region is the signing region ("" = us-east-1).
	Region string `yaml:"region"`
	// Bucket is the remote bucket holding the backing (required).
	Bucket string `yaml:"bucket"`
	// Prefix is prepended to every remote key: "" or a key prefix ending
	// in "/" (Normalize appends the slash).
	Prefix string `yaml:"prefix"`
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
	// EncryptionKey is the key-encryption key (KEK) wrapping the backing's
	// per-bucket master keys: standard base64 of exactly 32 bytes. Without
	// it, neither AES256 nor restic encryption is offered on the backing.
	// When empty it is read from the environment (see BackingEnv).
	EncryptionKey string `yaml:"encryption_key"`
	// Gateway stores objects natively, in remote buckets named like the
	// gos3 buckets (readable by other S3 clients). Gateway backings support
	// neither versioning nor object lock and cannot be replicas or move
	// targets.
	Gateway bool `yaml:"gateway"`

	// fromEnv records the fields whose values were taken from the
	// environment (bit i = s3EnvFields[i]); such values are never written
	// to backings.json.
	fromEnv uint16
}

// Limits of S3Config.PartSize.
const (
	MinBackingPartSize = 5 << 20
	MaxBackingPartSize = 5 << 30
)

// MaxS3PrefixLen bounds S3Config.Prefix (bytes).
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
	return s.Endpoint == "" && s.Region == "" && s.Bucket == "" && s.Prefix == "" && s.Addressing == "" &&
		s.PartSize == 0 && s.AccessKey == "" && s.SecretKey == "" && s.EncryptionKey == "" && !s.Gateway
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

// Location describes where the backing's data lives, without secrets or
// the endpoint: "s3://<bucket>/<prefix>".
func (s S3Config) Location() string {
	return "s3://" + s.Bucket + "/" + s.Prefix
}

// identity is the part of Identity after "s3:": the remote bucket, prefix
// and gateway flag, which together define what data the backing holds.
func (s S3Config) identity() string {
	id := strings.ToLower(s.Bucket) + "/" + s.Prefix
	if s.Gateway {
		id += "#gateway"
	}
	return id
}

// s3EnvFields are the S3Config YAML keys that BackingEnv variables supply.
var s3EnvFields = []string{"endpoint", "region", "bucket", "prefix", "addressing", "part_size",
	"access_key", "secret_key", "encryption_key", "gateway"}

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
	case "bucket":
		return s.Bucket
	case "prefix":
		return s.Prefix
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
	case "gateway":
		return strconv.FormatBool(s.Gateway)
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
	case "bucket":
		s.Bucket = ""
	case "prefix":
		s.Prefix = ""
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
	case "gateway":
		s.Gateway = false
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

// fillFromEnv sets the empty fields of s from the BackingEnv variables of
// backing name.
func (s *S3Config) fillFromEnv(name string, lookup func(string) (string, bool)) error {
	for _, field := range s3EnvFields {
		env := BackingEnv(name, field)
		raw, ok := lookup(env)
		if !ok {
			continue
		}
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		var err error
		filled := true
		switch field {
		case "endpoint":
			filled = fillString(&s.Endpoint, v)
		case "region":
			filled = fillString(&s.Region, v)
		case "bucket":
			filled = fillString(&s.Bucket, v)
		case "prefix":
			filled = fillString(&s.Prefix, v)
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
		case "gateway":
			if filled = !s.Gateway; filled {
				s.Gateway, err = ParseBool(v)
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", env, err)
		}
		if filled {
			s.setEnv(field, true)
		}
	}
	return nil
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

// normalize trims the values, lower-cases enumerations ("auto" addressing
// becomes ""), drops trailing slashes from the endpoint and makes a
// non-empty prefix end in "/".
func (s *S3Config) normalize() {
	s.Endpoint = strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	s.Region = strings.TrimSpace(s.Region)
	s.Bucket = strings.TrimSpace(s.Bucket)
	s.Prefix = strings.TrimSpace(s.Prefix)
	if s.Prefix != "" && !strings.HasSuffix(s.Prefix, "/") {
		s.Prefix += "/"
	}
	s.Addressing = strings.ToLower(strings.TrimSpace(s.Addressing))
	if s.Addressing == "auto" {
		s.Addressing = ""
	}
	s.AccessKey = strings.TrimSpace(s.AccessKey)
	s.SecretKey = strings.TrimSpace(s.SecretKey)
	s.EncryptionKey = strings.TrimSpace(s.EncryptionKey)
}

// normalizeS3 normalizes storage.s3 and the s3 block of every backing.
func (c *Config) normalizeS3() {
	if c.Storage.S3 != nil {
		c.Storage.S3.normalize()
	}
	for i := range c.Storage.Backings {
		if s := c.Storage.Backings[i].S3; s != nil {
			s.normalize()
		}
	}
}

// validateS3 checks the s3 settings of backing name, configured at key
// (e.g. "storage.s3"). Errors start with the offending key.
func validateS3(key, name string, s S3Config) error {
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
	if s.Bucket == "" {
		return fmt.Errorf("%s.bucket: required (or set %s)", key, BackingEnv(name, "bucket"))
	}
	if !remoteBucketRe.MatchString(s.Bucket) {
		return fmt.Errorf("%s.bucket: %q is not a valid bucket name", key, s.Bucket)
	}
	if err := validateS3Prefix(s.Prefix); err != nil {
		return fmt.Errorf("%s.prefix: %w", key, err)
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

// s3Place is where an s3 backing's data lives, for the overlap check.
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

// checkS3Overlap refuses two s3 backings on the same service and bucket
// whose prefixes are equal or nested.
func checkS3Overlap(places []s3Place) error {
	for i, a := range places {
		for _, b := range places[:i] {
			if a.server == b.server && strings.EqualFold(a.bucket, b.bucket) &&
				(strings.HasPrefix(a.prefix, b.prefix) || strings.HasPrefix(b.prefix, a.prefix)) {
				return fmt.Errorf("%s.prefix: %q in bucket %q overlaps %s (prefix %q); every s3 backing needs its own bucket or a separate prefix",
					a.key, a.prefix, a.bucket, b.key, b.prefix)
			}
		}
	}
	return nil
}

// errNoS3 is returned for an s3 default backing without storage.s3.
var errNoS3 = errors.New("storage.s3: required for the s3 backend (endpoint, bucket and credentials)")
