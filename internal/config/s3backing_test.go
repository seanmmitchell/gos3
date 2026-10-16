package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testKEK = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, KEKSize))

// withLookupEnv replaces the environment seen by BackingSettings.Apply.
func withLookupEnv(t *testing.T, m map[string]string) {
	t.Helper()
	old := lookupEnv
	lookupEnv = env(m)
	t.Cleanup(func() { lookupEnv = old })
}

func TestS3DefaultBackingYAML(t *testing.T) {
	c, err := load(writeFile(t, "c.yaml", `
storage:
  backend: S3
  s3:
    endpoint: " https://s3.example.com:9000/ "
    home_bucket: data
    home_prefix: gos3
    default_kind: NATIVE
    addressing: AUTO
    part_size: 8388608
    access_key: AK
    secret_key: SK
    encryption_key: `+testKEK+`
`), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.Backend != BackendS3 || c.Storage.Path != "" {
		t.Fatalf("backend %q path %q", c.Storage.Backend, c.Storage.Path)
	}
	s := c.Storage.S3
	if s.Endpoint != "https://s3.example.com:9000" || s.HomeBucket != "data" || s.HomePrefix != "gos3/" || s.Addressing != "" ||
		s.PartSize != 8<<20 || s.DefaultKind != BucketKindNative {
		t.Fatalf("normalized %+v", *s)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if k, err := s.KEK(); err != nil || len(k) != KEKSize {
		t.Fatalf("KEK = %v, %v", k, err)
	}
	if got := s.Location(); got != "s3://data/gos3/" {
		t.Errorf("Location = %q", got)
	}
	if c.SystemDir() != "" {
		t.Errorf("system dir %q, want ephemeral", c.SystemDir())
	}
	if d := c.Deprecations(); len(d) != 0 {
		t.Errorf("Deprecations = %v", d)
	}
}

func TestS3DefaultBackingJSON(t *testing.T) {
	c, err := load(writeFile(t, "c.json", `{"storage": {"backend": "s3", "path": "/var/lib/gos3",
		"s3": {"endpoint": "http://minio:9000", "home_bucket": "bk", "access_key": "a", "secret_key": "s", "part_size": 5242880, "default_kind": "wrapped"},
		"backings": [{"name": "cold", "backend": "s3", "s3": {"endpoint": "http://minio:9000", "home_bucket": "bk", "home_prefix": "cold/", "access_key": "a", "secret_key": "s"}},
		             {"name": "tmp", "backend": "memory", "s3": null}]}}`), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.S3 == nil || c.Storage.S3.HomeBucket != "bk" || len(c.Storage.Backings) != 2 || c.Storage.Backings[0].S3.HomePrefix != "cold/" ||
		c.Storage.Backings[0].S3.DefaultKind != BucketKindWrapped {
		t.Fatalf("decoded %+v", c.Storage)
	}
	if c.Storage.Backings[1].S3 != nil {
		t.Errorf("null s3 decoded as %+v", c.Storage.Backings[1].S3)
	}
	// "" (default) and "cold/" overlap in the same bucket.
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "storage.backings[0].s3.home_prefix") {
		t.Fatalf("overlap: %v", err)
	}
	for _, bad := range []string{
		`{"storage": {"s3": {"part_size": "5MiB"}}}`,
		`{"storage": {"s3": {"gateway": "yes"}}}`,
		`{"storage": {"s3": {"bucket": 1}}}`,
		`{"storage": {"s3": {"home_bucket": 1}}}`,
		`{"storage": {"s3": {"default_kind": true}}}`,
		`{"storage": {"s3": {"nope": "x"}}}`,
		`{"storage": {"s3": []}}`,
	} {
		if _, err := load(writeFile(t, "c.json", bad), env(nil), nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestS3DefaultBackingEnv(t *testing.T) {
	c, err := load("", env(map[string]string{
		"GOS3_STORAGE_BACKEND":   "s3",
		"GOS3_S3_ENDPOINT":       "https://s3.eu-west-1.amazonaws.com",
		"GOS3_S3_REGION":         "eu-west-1",
		"GOS3_S3_HOME_BUCKET":    "bkt",
		"GOS3_S3_HOME_PREFIX":    "x/y",
		"GOS3_S3_DEFAULT_KIND":   " Native ",
		"GOS3_S3_ADDRESSING":     "virtual",
		"GOS3_S3_PART_SIZE":      "16MiB",
		"GOS3_S3_ACCESS_KEY":     "AK",
		"GOS3_S3_SECRET_KEY":     "SK",
		"GOS3_S3_ENCRYPTION_KEY": testKEK,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s := *c.Storage.S3
	want := S3Config{Endpoint: "https://s3.eu-west-1.amazonaws.com", Region: "eu-west-1", HomeBucket: "bkt", HomePrefix: "x/y/",
		DefaultKind: BucketKindNative, Addressing: "virtual", PartSize: 16 << 20, AccessKey: "AK", SecretKey: "SK", EncryptionKey: testKEK,
		fromEnv: s3FieldBit("access_key") | s3FieldBit("secret_key") | s3FieldBit("encryption_key")}
	if s != want {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
	if s.PathStyle() {
		t.Error("virtual addressing reports path style")
	}
	// The s3 block only goes with the s3 backend.
	c, err = load("", env(map[string]string{"GOS3_S3_REGION": "eu-west-1"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil || !strings.HasPrefix(err.Error(), "storage.s3:") {
		t.Fatalf("s3 block with fs backend: %v", err)
	}
	// An empty s3 block with another backend is ignored: no default kind
	// is derived for it.
	c, err = load(writeFile(t, "c.yaml", "storage:\n  s3: {}\n"), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil || c.Storage.S3.DefaultKind != "" {
		t.Fatalf("empty s3 block with fs backend: %v %+v", err, *c.Storage.S3)
	}
}

func TestS3BackingsYAMLAndEnvFamily(t *testing.T) {
	vars := map[string]string{
		"GOS3_STORAGE_BACKINGS":                 "fast=/mnt/nvme,cold-tier=s3",
		"GOS3_BACKING_COLD_TIER_ENDPOINT":       "http://minio:9000",
		"GOS3_BACKING_COLD_TIER_HOME_BUCKET":    "cold",
		"GOS3_BACKING_COLD_TIER_PART_SIZE":      "6MiB",
		"GOS3_BACKING_COLD_TIER_DEFAULT_KIND":   "native",
		"GOS3_BACKING_COLD_TIER_ACCESS_KEY":     "envak",
		"GOS3_BACKING_COLD_TIER_SECRET_KEY":     "envsk",
		"GOS3_BACKING_COLD_TIER_ENCRYPTION_KEY": testKEK,
	}
	c, err := load("", env(vars), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	b := c.Storage.Backings[1]
	if b.Backend != BackendS3 || b.Path != "" || b.S3.Endpoint != "http://minio:9000" || b.S3.PartSize != 6<<20 || b.S3.HomeBucket != "cold" ||
		b.S3.DefaultKind != BucketKindNative || !b.S3.isEnv("default_kind") || !b.S3.isEnv("home_bucket") ||
		b.S3.AccessKey != "envak" || !b.S3.isEnv("secret_key") || !b.S3.isEnv("encryption_key") || !b.S3.isEnv("access_key") {
		t.Fatalf("backing %+v %+v", b, b.S3)
	}

	// YAML values win over the environment, which fills the gaps only.
	file := writeFile(t, "c.yaml", `
storage:
  backings:
    - name: cold-tier
      backend: s3
      s3: {endpoint: "http://other:9000", home_bucket: yamlbucket, access_key: yamlak}
`)
	c, err = load(file, env(map[string]string{
		"GOS3_BACKING_COLD_TIER_HOME_BUCKET": "envbucket",
		"GOS3_BACKING_COLD_TIER_ACCESS_KEY":  "envak",
		"GOS3_BACKING_COLD_TIER_SECRET_KEY":  "envsk",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Storage.Backings[0].S3
	if s.HomeBucket != "yamlbucket" || s.isEnv("home_bucket") || s.AccessKey != "yamlak" || s.isEnv("access_key") ||
		s.SecretKey != "envsk" || !s.isEnv("secret_key") {
		t.Fatalf("fill-empty: %+v", *s)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	// An s3 backing without an s3 block is filled from the environment too.
	c, err = load(writeFile(t, "c.yaml", "storage:\n  backings:\n    - {name: c, backend: s3}\n"), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GOS3_BACKING_C_ENDPOINT") {
		t.Fatalf("missing endpoint: %v", err)
	}
	if _, err := load("", env(map[string]string{"GOS3_STORAGE_BACKINGS": "c=s3", "GOS3_BACKING_C_PART_SIZE": "lots"}), nil); err == nil ||
		!strings.Contains(err.Error(), "GOS3_BACKING_C_PART_SIZE") {
		t.Fatalf("bad env size: %v", err)
	}
}

func TestBackingEnv(t *testing.T) {
	if got := BackingEnv("cold-tier", "secret_key"); got != "GOS3_BACKING_COLD_TIER_SECRET_KEY" {
		t.Errorf("BackingEnv = %q", got)
	}
	if got := BackingEnv("cold-tier", "home_bucket"); got != "GOS3_BACKING_COLD_TIER_HOME_BUCKET" {
		t.Errorf("BackingEnv = %q", got)
	}
	if got := BackingEnv(DefaultBackingName, "encryption_key"); got != "GOS3_S3_ENCRYPTION_KEY" {
		t.Errorf("default BackingEnv = %q", got)
	}
}

func TestValidateS3(t *testing.T) {
	dir := t.TempDir()
	good := func() Config {
		c := Default()
		c.Storage.Backend = BackendS3
		c.Storage.Path = ""
		c.Storage.S3 = &S3Config{Endpoint: "http://minio:9000", HomeBucket: "main", HomePrefix: "main/", AccessKey: "a", SecretKey: "s"}
		c.Storage.Backings = []BackingConfig{
			{Name: "cold", Backend: BackendS3, S3: &S3Config{Endpoint: "http://MINIO:9000", HomeBucket: "main", HomePrefix: "cold/", AccessKey: "a", SecretKey: "s"}},
			{Name: "nat", Backend: BackendS3, S3: &S3Config{Endpoint: "https://s3.example.com", HomeBucket: "meta", AccessKey: "a", SecretKey: "s", DefaultKind: BucketKindNative}},
		}
		return c
	}
	// local adds a backing without a home, which needs a system directory.
	local := func(c *Config) {
		c.Storage.Path = dir
		c.Storage.Backings = append(c.Storage.Backings, BackingConfig{Name: "loc", Backend: BackendS3,
			S3: &S3Config{Endpoint: "https://s3.example.com", AccessKey: "a", SecretKey: "s"}})
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, mod := range map[string]func(c *Config){
		"replica":                  func(c *Config) { c.Storage.Replication.DefaultReplicas = []string{"cold"} },
		"replica with native kind": func(c *Config) { c.Storage.Replication.DefaultReplicas = []string{"nat"} },
		"without home, system dir": local,
		"homeless default native":  func(c *Config) { local(c); c.Storage.DefaultBacking = "loc" },
		"homes in other buckets": func(c *Config) {
			c.Storage.Backings[0].S3.HomeBucket, c.Storage.Backings[0].S3.HomePrefix = "other", ""
		},
		"homes on other services": func(c *Config) {
			c.Storage.Backings[0].S3.Endpoint, c.Storage.Backings[0].S3.HomePrefix = "http://minio:9001", ""
		},
		"two backings without homes": func(c *Config) { local(c); local(c); c.Storage.Backings[3].Name = "loc2" },
	} {
		c := good()
		mod(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	cases := []struct {
		name string
		mod  func(c *Config)
		want string
	}{
		{"no endpoint", func(c *Config) { c.Storage.S3.Endpoint = "" }, "storage.s3.endpoint: required (or set GOS3_S3_ENDPOINT)"},
		{"endpoint path", func(c *Config) { c.Storage.S3.Endpoint = "http://minio:9000/x" }, "storage.s3.endpoint"},
		{"endpoint query", func(c *Config) { c.Storage.S3.Endpoint = "http://minio:9000?x" }, "storage.s3.endpoint"},
		{"endpoint userinfo", func(c *Config) { c.Storage.S3.Endpoint = "http://u:p@minio:9000" }, `"http://<redacted>@minio:9000"`},
		{"endpoint scheme", func(c *Config) { c.Storage.S3.Endpoint = "ftp://minio" }, "storage.s3.endpoint"},
		{"endpoint fragment", func(c *Config) { c.Storage.S3.Endpoint = "http://minio#x" }, "storage.s3.endpoint"},
		{"region", func(c *Config) { c.Storage.S3.Region = "EU West" }, "storage.s3.region"},
		{"no home without system dir", func(c *Config) { c.Storage.Backings[0].S3.HomeBucket, c.Storage.Backings[0].S3.HomePrefix = "", "" },
			"storage.backings[0].s3: an s3 backing without home_bucket keeps its records in the system directory; set storage.path or home_bucket"},
		{"default without home or system dir", func(c *Config) { c.Storage.S3.HomeBucket, c.Storage.S3.HomePrefix = "", "" },
			"storage.s3: an s3 backing without home_bucket"},
		{"prefix without bucket", func(c *Config) { local(c); c.Storage.Backings[2].S3.HomePrefix = "x/" },
			"storage.backings[2].s3.home_prefix: requires home_bucket (or set GOS3_BACKING_LOC_HOME_BUCKET)"},
		{"bad bucket", func(c *Config) { c.Storage.S3.HomeBucket = "Bad_Bucket" }, "storage.s3.home_bucket"},
		{"prefix slash", func(c *Config) { c.Storage.S3.HomePrefix = "/x/" }, "storage.s3.home_prefix"},
		{"prefix dots", func(c *Config) { c.Storage.S3.HomePrefix = "a/../b/" }, "storage.s3.home_prefix"},
		{"prefix dot", func(c *Config) { c.Storage.S3.HomePrefix = "./" }, "storage.s3.home_prefix"},
		{"prefix empty segment", func(c *Config) { c.Storage.S3.HomePrefix = "a//" }, "storage.s3.home_prefix"},
		{"prefix long", func(c *Config) { c.Storage.S3.HomePrefix = strings.Repeat("a", 513) + "/" }, "storage.s3.home_prefix"},
		{"prefix utf8", func(c *Config) { c.Storage.S3.HomePrefix = "\xff/" }, "storage.s3.home_prefix"},
		{"bad kind", func(c *Config) { c.Storage.S3.DefaultKind = "gateway" }, `storage.s3.default_kind: "gateway" is invalid (expected wrapped or native)`},
		{"wrapped without home", func(c *Config) { local(c); c.Storage.Backings[2].S3.DefaultKind = BucketKindWrapped },
			"storage.backings[2].s3.default_kind: wrapped buckets need a container: set home_bucket"},
		{"addressing", func(c *Config) { c.Storage.S3.Addressing = "dns" }, "storage.s3.addressing"},
		{"part size small", func(c *Config) { c.Storage.S3.PartSize = 1 << 20 }, "storage.s3.part_size"},
		{"part size big", func(c *Config) { c.Storage.S3.PartSize = 6 << 30 }, "storage.s3.part_size"},
		{"no secret", func(c *Config) { c.Storage.Backings[0].S3.SecretKey = "" }, "GOS3_BACKING_COLD_SECRET_KEY"},
		{"no access", func(c *Config) { c.Storage.S3.AccessKey = "" }, "GOS3_S3_ACCESS_KEY"},
		{"kek not base64", func(c *Config) { c.Storage.S3.EncryptionKey = "not base64!" }, "storage.s3.encryption_key: "},
		{"kek short", func(c *Config) {
			c.Storage.Backings[0].S3.EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 16))
		}, "GOS3_BACKING_COLD_ENCRYPTION_KEY"},
		{"no s3 block", func(c *Config) { c.Storage.S3 = nil }, "storage.s3: required"},
		{"backing no s3 block", func(c *Config) { c.Storage.Backings[0].S3 = nil }, "storage.backings[0].s3: required"},
		{"backing path", func(c *Config) { c.Storage.Backings[0].Path = "/x" }, "storage.backings[0].path: an s3 backing has no path"},
		{"s3 on fs backing", func(c *Config) {
			c.Storage.Backings = append(c.Storage.Backings, BackingConfig{Name: "f", Backend: BackendFS, Path: t.TempDir(), S3: &S3Config{}})
		}, "storage.backings[2].s3: only allowed with backend s3"},
		{"s3 on fs default", func(c *Config) { c.Storage.Backend, c.Storage.Path = BackendFS, t.TempDir() }, "storage.s3: only used with storage.backend s3"},
		{"same prefix", func(c *Config) { c.Storage.Backings[0].S3.HomePrefix = "main/" }, "storage.backings[0].s3.home_prefix"},
		{"nested prefix", func(c *Config) { c.Storage.Backings[0].S3.HomePrefix = "main/sub/" }, "overlaps the home of storage.s3"},
		{"default port", func(c *Config) {
			c.Storage.Backings[1].S3.Endpoint = "https://s3.example.com:443"
			c.Storage.Backings = append(c.Storage.Backings, BackingConfig{Name: "nat2", Backend: BackendS3,
				S3: &S3Config{Endpoint: "https://S3.example.com", HomeBucket: "meta", HomePrefix: "x/", AccessKey: "a", SecretKey: "s"}})
		}, "storage.backings[2].s3.home_prefix"},
		{"env name clash", func(c *Config) {
			c.Storage.Backings = append(c.Storage.Backings, BackingConfig{Name: "c-d", Backend: BackendS3,
				S3: &S3Config{Endpoint: "http://x", HomeBucket: "b1", AccessKey: "a", SecretKey: "s"}},
				BackingConfig{Name: "c_d", Backend: BackendS3, S3: &S3Config{Endpoint: "http://x", HomeBucket: "b2", AccessKey: "a", SecretKey: "s"}})
		}, "GOS3_BACKING_C_D_*"},
		{"homeless replica", func(c *Config) { local(c); c.Storage.Replication.DefaultReplicas = []string{"loc"} },
			`default_replicas[0]: "loc" is an s3 backing without home_bucket`},
		{"native default backing with replicas", func(c *Config) {
			c.Storage.DefaultBacking = "nat"
			c.Storage.Replication.DefaultReplicas = []string{"cold"}
		}, `new buckets on the default backing "nat" are native`},
		{"homeless default backing with replicas", func(c *Config) {
			local(c)
			c.Storage.DefaultBacking = "loc"
			c.Storage.Replication.DefaultReplicas = []string{"cold"}
		}, `default backing "loc" are native`},
		{"native default storage.s3 with replicas", func(c *Config) {
			c.Storage.S3.DefaultKind = BucketKindNative
			c.Storage.Replication.DefaultReplicas = []string{"cold"}
		}, `default backing "default" are native`},
		{"homeless storage.s3 as replica", func(c *Config) {
			c.Storage.Path = dir
			c.Storage.S3.HomeBucket, c.Storage.S3.HomePrefix = "", ""
			c.Storage.DefaultBacking = "cold"
			c.Storage.Replication.DefaultReplicas = []string{"default"}
		}, `"default" is an s3 backing without home_bucket`},
	}
	for _, tc := range cases {
		c := good()
		tc.mod(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want an error mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestS3Redacted(t *testing.T) {
	c := Default()
	c.Storage.S3 = &S3Config{SecretKey: "topsecret", EncryptionKey: testKEK, AccessKey: "AK"}
	c.Storage.Backings = []BackingConfig{{Name: "cold", Backend: BackendS3, S3: &S3Config{SecretKey: "s2", EncryptionKey: "k2"}},
		{Name: "m", Backend: BackendMemory}}
	r := c.Redacted()
	if r.Storage.S3.SecretKey != RedactedValue || r.Storage.S3.EncryptionKey != RedactedValue || r.Storage.S3.AccessKey != "AK" {
		t.Errorf("default s3 %+v", *r.Storage.S3)
	}
	if b := r.Storage.Backings[0].S3; b.SecretKey != RedactedValue || b.EncryptionKey != RedactedValue {
		t.Errorf("backing s3 %+v", *b)
	}
	if c.Storage.S3.SecretKey != "topsecret" || c.Storage.Backings[0].S3.EncryptionKey != "k2" {
		t.Error("Redacted modified the original")
	}
	y, _ := r.YAML()
	for _, secret := range []string{"topsecret", testKEK, "s2", "k2"} {
		if bytes.Contains(y, []byte(secret)) {
			t.Errorf("YAML contains %q", secret)
		}
	}
}

func TestS3ConfigHelpers(t *testing.T) {
	var nilCfg *S3Config
	if nilCfg.Clone() != nil {
		t.Error("nil Clone")
	}
	s := &S3Config{HomeBucket: "b"}
	s.setEnv("secret_key", true)
	cp := s.Clone()
	cp.HomeBucket = "x"
	if s.HomeBucket != "b" || !cp.isEnv("secret_key") {
		t.Error("Clone is not a deep copy with provenance")
	}
	for _, nz := range []S3Config{{DefaultKind: BucketKindNative}, {HomePrefix: "p/"}, {LegacyGateway: true}, {LegacyBucket: "b"}} {
		if nz.IsZero() {
			t.Errorf("IsZero(%+v)", nz)
		}
	}
	if !(S3Config{}).IsZero() {
		t.Error("IsZero")
	}
	if k, err := (S3Config{}).KEK(); k != nil || err != nil {
		t.Errorf("empty KEK = %v, %v", k, err)
	}
	for ep, want := range map[string]bool{"https://s3.amazonaws.com": false, "https://s3.eu-west-1.amazonaws.com": false, "http://minio:9000": true} {
		if got := (S3Config{Endpoint: ep}).PathStyle(); got != want {
			t.Errorf("PathStyle(%s) = %v", ep, got)
		}
	}
	if !(S3Config{Endpoint: "https://s3.amazonaws.com", Addressing: "path"}).PathStyle() {
		t.Error("explicit path addressing")
	}
	home := S3Config{HomeBucket: "bk", HomePrefix: "p/"}
	if !home.HasHome() || home.EffectiveDefaultKind() != BucketKindWrapped || home.Location() != "s3://bk/p/" {
		t.Errorf("home: %v %q %q", home.HasHome(), home.EffectiveDefaultKind(), home.Location())
	}
	var local S3Config
	if local.HasHome() || local.EffectiveDefaultKind() != BucketKindNative || local.Location() != "local:" {
		t.Errorf("local: %v %q %q", local.HasHome(), local.EffectiveDefaultKind(), local.Location())
	}
	if k := (S3Config{HomeBucket: "bk", DefaultKind: BucketKindNative}).EffectiveDefaultKind(); k != BucketKindNative {
		t.Errorf("explicit kind: %q", k)
	}
}

func s3Def(name, bucket, prefix string) BackingDefinition {
	return BackingDefinition{Name: name, Backend: BackendS3, S3: &S3Definition{
		Endpoint: "http://minio:9000", HomeBucket: bucket, HomePrefix: prefix, AccessKey: "ak", SecretKey: "sk", EncryptionKey: testKEK}}
}

func TestBackingDefinitionRoundTrip(t *testing.T) {
	d := s3Def("cold", "Cold", "p/")
	d.S3.DefaultKind, d.S3.PartSize, d.S3.Region, d.S3.Addressing = BucketKindNative, 6<<20, "r1", "path"
	back := DefinitionOf(d.Config())
	if !back.Equal(d) || !back.S3.SecretKeyConfigured || !back.S3.EncryptionKeyConfigured || back.S3.SecretKeySource != "" {
		t.Fatalf("round trip %+v", *back.S3)
	}
	if got := d.Identity(); got != "s3:home:cold/p/" {
		t.Errorf("Identity = %q", got)
	}
	// Neither the connection nor the default kind is part of the identity.
	other := s3Def("cold", "cold", "p")
	other.S3.DefaultKind = BucketKindWrapped
	other.S3.Endpoint, other.S3.SecretKey, other.S3.Region, other.S3.EncryptionKey = "https://elsewhere", "other", "r2", ""
	other.S3.PartSize, other.S3.Addressing, other.S3.AccessKey = 8<<20, "virtual", "ak2"
	if other.Identity() != d.Identity() {
		t.Errorf("connection settings changed the identity: %q", other.Identity())
	}
	if other.Equal(d) {
		t.Error("Equal ignores settings")
	}
	o2 := DefinitionOf(d.Config())
	o2.S3.SecretKey = "changed"
	if o2.Equal(d) {
		t.Error("Equal ignores the secret")
	}
	o3 := DefinitionOf(d.Config())
	o3.S3.DefaultKind = BucketKindWrapped
	if o3.Equal(d) {
		t.Error("Equal ignores the default kind")
	}
	// The home is: another bucket or prefix is other data.
	for _, mod := range []func(*S3Definition){
		func(s *S3Definition) { s.HomeBucket = "other" },
		func(s *S3Definition) { s.HomePrefix = "elsewhere/" },
		func(s *S3Definition) { s.HomeBucket, s.HomePrefix = "", "" },
	} {
		o := DefinitionOf(d.Config())
		mod(o.S3)
		if o.Identity() == d.Identity() {
			t.Errorf("home change kept the identity %q", o.Identity())
		}
	}
	// Without a home the records are local: the name is the identity.
	loc := s3Def("cold", "", "")
	if got := loc.Identity(); got != "s3:local:cold" {
		t.Errorf("local Identity = %q", got)
	}
	loc.S3.Endpoint, loc.S3.DefaultKind = "https://elsewhere", BucketKindNative
	if loc.Identity() != "s3:local:cold" {
		t.Errorf("connection changed the local identity: %q", loc.Identity())
	}
	if got := (BackingDefinition{Name: "warm", Backend: BackendS3}).Identity(); got != "s3:local:warm" {
		t.Errorf("identity without an s3 block = %q", got)
	}
	// Deprecated keys count as their replacements.
	legacy := s3Def("cold", "", "")
	legacy.S3.LegacyBucket, legacy.S3.LegacyPrefix, legacy.S3.LegacyGateway = "Cold", "p/", true
	legacy.S3.PartSize, legacy.S3.Region, legacy.S3.Addressing = 6<<20, "r1", "path"
	if legacy.Identity() != d.Identity() || !legacy.Equal(d) || !d.Equal(legacy) {
		t.Errorf("legacy definition: identity %q, equal %v", legacy.Identity(), legacy.Equal(d))
	}

	if (BackingDefinition{Name: "m", Backend: BackendMemory}).Identity() != "memory:m" {
		t.Error("memory identity")
	}
	abs, _ := filepath.Abs("rel")
	if got := (BackingDefinition{Name: "f", Backend: BackendFS, Path: "rel"}).Identity(); got != "fs:"+abs {
		t.Errorf("fs identity %q", got)
	}
	if (BackingDefinition{Name: "f", Backend: BackendFS, Path: "a"}).Equal(BackingDefinition{Name: "f", Backend: BackendFS, Path: "b"}) {
		t.Error("fs Equal")
	}
}

func TestBackingSettingsPublic(t *testing.T) {
	s := BackingSettings{Backings: []BackingDefinition{s3Def("cold", "bk", "cold/"), {Name: "m", Backend: BackendMemory}}, DefaultBacking: "default"}
	p := s.Public()
	if p.Backings[0].S3.SecretKey != "" || p.Backings[0].S3.EncryptionKey != "" ||
		!p.Backings[0].S3.SecretKeyConfigured || !p.Backings[0].S3.EncryptionKeyConfigured || p.Backings[0].S3.AccessKey != "ak" {
		t.Fatalf("public %+v", *p.Backings[0].S3)
	}
	if s.Backings[0].S3.SecretKey != "sk" {
		t.Error("Public modified the original")
	}
	data, _ := json.Marshal(p)
	if bytes.Contains(data, []byte(`"sk"`)) || bytes.Contains(data, []byte(testKEK)) || !bytes.Contains(data, []byte(`"secretKeyConfigured":true`)) {
		t.Errorf("public JSON %s", data)
	}
	for _, want := range []string{`"homeBucket":"bk"`, `"homePrefix":"cold/"`, `"defaultKind":""`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("public JSON lacks %s: %s", want, data)
		}
	}
	for _, legacy := range []string{`"bucket"`, `"prefix"`, `"gateway"`} {
		if bytes.Contains(data, []byte(legacy)) {
			t.Errorf("public JSON has the deprecated key %s: %s", legacy, data)
		}
	}
}

func TestBackingSettingsApplyS3(t *testing.T) {
	withLookupEnv(t, map[string]string{"GOS3_BACKING_ENVY_SECRET_KEY": "from-env", "GOS3_BACKING_ENVY_ENCRYPTION_KEY": testKEK})
	base := StorageConfig{Backend: BackendMemory}
	in := BackingSettings{DefaultBacking: "default", Backings: []BackingDefinition{s3Def("cold", "bk", "cold/")}}
	envy := s3Def("envy", "bk", "envy/")
	envy.S3.SecretKey, envy.S3.EncryptionKey = "", ""
	in.Backings = append(in.Backings, envy)
	st, err := in.Apply(base)
	if err != nil {
		t.Fatal(err)
	}
	if s := st.Backings[1].S3; s.SecretKey != "from-env" || !s.isEnv("secret_key") || !s.isEnv("encryption_key") || s.EncryptionKey != testKEK {
		t.Fatalf("env fallback %+v", *s)
	}
	if s := st.Backings[0].S3; s.DefaultKind != BucketKindWrapped {
		t.Fatalf("default kind not derived: %+v", *s)
	}

	// Blank secrets keep the stored values (and their provenance).
	pub := BackingsFrom(st).Public()
	pub.Backings[0].S3.Region = "r2"
	pub.Backings[1].S3.SecretKeySource = "" // ignored
	st2, err := pub.Apply(st)
	if err != nil {
		t.Fatal(err)
	}
	if s := st2.Backings[0].S3; s.SecretKey != "sk" || s.EncryptionKey != testKEK || s.Region != "r2" || s.isEnv("secret_key") {
		t.Fatalf("kept %+v", *s)
	}
	if s := st2.Backings[1].S3; s.SecretKey != "from-env" || !s.isEnv("secret_key") {
		t.Fatalf("kept env %+v", *s)
	}
	// A new secret replaces the stored one and is no longer from env.
	pub.Backings[1].S3.SecretKey = "typed"
	st3, err := pub.Apply(st)
	if err != nil {
		t.Fatal(err)
	}
	if s := st3.Backings[1].S3; s.SecretKey != "typed" || s.isEnv("secret_key") {
		t.Fatalf("replaced %+v", *s)
	}
	// Secrets are not carried over to a backing of another kind or name.
	renamed := BackingsFrom(st).Public()
	renamed.Backings = renamed.Backings[:1]
	renamed.Backings[0].Name = "warm"
	if _, err := renamed.Apply(st); err == nil || !strings.Contains(err.Error(), "secret_key") {
		t.Fatalf("renamed backing kept secrets: %v", err)
	}
	// Validation errors are reported.
	bad := BackingSettings{DefaultBacking: "default", Backings: []BackingDefinition{s3Def("x", "bk", "")}}
	bad.Backings[0].Path = "/x"
	if _, err := bad.Apply(base); err == nil || !strings.Contains(err.Error(), "no path") {
		t.Fatalf("path accepted: %v", err)
	}
	// A backing without a home needs the system directory.
	homeless := BackingSettings{DefaultBacking: "default", Backings: []BackingDefinition{s3Def("x", "", "")}}
	if _, err := homeless.Apply(base); err == nil || !strings.Contains(err.Error(), "set storage.path or home_bucket") {
		t.Fatalf("homeless backing without a system directory: %v", err)
	}
	withDir := base
	withDir.Path = t.TempDir()
	st4, err := homeless.Apply(withDir)
	if err != nil {
		t.Fatal(err)
	}
	if s := st4.Backings[0].S3; s.DefaultKind != BucketKindNative || s.Location() != "local:" {
		t.Fatalf("homeless backing %+v", *s)
	}
}

func TestBackingSettingsSaveLoad(t *testing.T) {
	withLookupEnv(t, map[string]string{"GOS3_BACKING_ENVY_SECRET_KEY": "from-env", "GOS3_BACKING_ENVY_ACCESS_KEY": "env-ak"})
	dir := t.TempDir()
	c := Default()
	c.Storage.Path = dir
	c.Storage.Backings = []BackingConfig{{Name: "static", Backend: BackendMemory}, {Name: "two", Backend: BackendMemory}}
	in := BackingSettings{DefaultBacking: "default", Backings: []BackingDefinition{s3Def("cold", "bk", "cold/"), s3Def("envy", "bk", "envy/")}}
	in.Backings[1].S3.SecretKey, in.Backings[1].S3.AccessKey = "", ""
	in.Backings[1].S3.DefaultKind = BucketKindNative
	st, err := in.Apply(c.Storage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.SystemDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := BackingsFrom(st).Save(c.BackingsPath()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.BackingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"sk"`)) || !bytes.Contains(data, []byte(testKEK)) {
		t.Errorf("configured secrets not saved: %s", data)
	}
	if bytes.Contains(data, []byte("from-env")) || bytes.Contains(data, []byte("env-ak")) || bytes.Contains(data, []byte("Source")) {
		t.Errorf("env values saved: %s", data)
	}
	// Only the new keys are written, and the default kind only when it is
	// not the default for the home.
	if !bytes.Contains(data, []byte(`"homeBucket": "bk"`)) || !bytes.Contains(data, []byte(`"homePrefix": "cold/"`)) ||
		bytes.Count(data, []byte(`"defaultKind": "native"`)) != 1 || bytes.Contains(data, []byte(`"wrapped"`)) {
		t.Errorf("saved keys:\n%s", data)
	}
	for _, legacy := range []string{`"bucket"`, `"prefix"`, `"gateway"`} {
		if bytes.Contains(data, []byte(legacy)) {
			t.Errorf("deprecated key %s saved:\n%s", legacy, data)
		}
	}

	// LoadBackings replaces the static backings (decoding into a fresh
	// value: the static "static" memory backing must not leak into the
	// first saved definition) and resolves env secrets again.
	if err := c.LoadBackings(); err != nil {
		t.Fatal(err)
	}
	if len(c.Storage.Backings) != 2 || c.Storage.Backings[0].Name != "cold" || c.Storage.Backings[0].S3.SecretKey != "sk" {
		t.Fatalf("loaded %+v", c.Storage.Backings)
	}
	if s := c.Storage.Backings[1].S3; s.SecretKey != "from-env" || !s.isEnv("secret_key") || s.AccessKey != "env-ak" || !s.isEnv("access_key") {
		t.Fatalf("env secrets after reload %+v", *s)
	}
	if a, b := c.Storage.Backings[0].S3.DefaultKind, c.Storage.Backings[1].S3.DefaultKind; a != BucketKindWrapped || b != BucketKindNative {
		t.Fatalf("default kinds after reload: %q, %q", a, b)
	}
	if d := c.Deprecations(); len(d) != 0 {
		t.Errorf("Deprecations = %v", d)
	}

	// A saved fs definition does not inherit fields of a static s3 one.
	c2 := Default()
	c2.Storage.Path = dir
	c2.Storage.Backings = []BackingConfig{{Name: "cold", Backend: BackendS3, S3: &S3Config{Endpoint: "http://x", HomeBucket: "zz", AccessKey: "a", SecretKey: "s"}}}
	fsDir := filepath.Join(t.TempDir(), "fs")
	if err := (BackingSettings{DefaultBacking: "default", Backings: []BackingDefinition{{Name: "cold", Backend: BackendFS, Path: fsDir}}}).Save(c2.BackingsPath()); err != nil {
		t.Fatal(err)
	}
	if err := c2.LoadBackings(); err != nil {
		t.Fatal(err)
	}
	if b := c2.Storage.Backings[0]; b.Backend != BackendFS || b.S3 != nil || b.Path != fsDir {
		t.Fatalf("fresh decode %+v", b)
	}
}

func TestWriteEnvTableBackingNote(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEnvTable(&buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GOS3_S3_ENCRYPTION_KEY", "GOS3_S3_HOME_BUCKET", "GOS3_S3_DEFAULT_KIND", "GOS3_BACKING_<NAME>_<FIELD>",
		"ENCRYPTION_KEY", "HOME_PREFIX", "DEFAULT_KIND=native"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("env table lacks %q", want)
		}
	}
}

func TestValidateS3EndpointRedactsUserinfo(t *testing.T) {
	for _, ep := range []string{"http://AKIA:hunter2@minio:9000", "http://AKIA:hunter2@minio:9000/path", "http://AKIA:hun%zzter2@minio"} {
		err := validateS3("storage.s3", DefaultBackingName, S3Config{Endpoint: ep, HomeBucket: "bk", AccessKey: "a", SecretKey: "s"}, "")
		if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "hun%zz") || strings.Contains(err.Error(), "AKIA") {
			t.Errorf("%s: %v", ep, err)
		}
	}
	if got := redactEndpoint("http://minio:9000/a@b"); got != "http://minio:9000/a@b" {
		t.Errorf("redactEndpoint without userinfo = %q", got)
	}
}

// Non-secret settings taken from GOS3_BACKING_<NAME>_* follow the
// environment: they are not written to backings.json, also after a live
// change that sends them back unchanged.
func TestBackingSettingsEnvFieldsNotSaved(t *testing.T) {
	vars := map[string]string{
		"GOS3_BACKING_COLD_ENDPOINT":     "http://minio:9000",
		"GOS3_BACKING_COLD_REGION":       "eu-west-1",
		"GOS3_BACKING_COLD_HOME_BUCKET":  "envbucket",
		"GOS3_BACKING_COLD_HOME_PREFIX":  "p",
		"GOS3_BACKING_COLD_ADDRESSING":   "path",
		"GOS3_BACKING_COLD_PART_SIZE":    "8MiB",
		"GOS3_BACKING_COLD_DEFAULT_KIND": "native",
		"GOS3_BACKING_COLD_ACCESS_KEY":   "envak",
		"GOS3_BACKING_COLD_SECRET_KEY":   "envsk",
	}
	withLookupEnv(t, vars)
	dir := t.TempDir()
	all := map[string]string{"GOS3_STORAGE_BACKINGS": "cold=s3", "GOS3_DATA": dir}
	for k, v := range vars {
		all[k] = v
	}
	c, err := load("", env(all), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	def := BackingsFrom(c.Storage).Backings[0]
	if got := strings.Join(def.S3.EnvFields, ","); got != "endpoint,region,home_bucket,home_prefix,addressing,part_size,access_key,secret_key,default_kind" {
		t.Fatalf("EnvFields = %s", got)
	}
	// A live PUT of the public form, unchanged apart from a new region.
	pub := BackingsFrom(c.Storage).Public()
	pub.Backings[0].S3.EnvFields = nil
	pub.Backings[0].S3.AccessKeySource = ""
	pub.Backings[0].S3.Region = "us-west-2"
	st, err := pub.Apply(c.Storage)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Backings[0].S3
	if s.Region != "us-west-2" || s.isEnv("region") || !s.isEnv("endpoint") || !s.isEnv("home_bucket") || !s.isEnv("home_prefix") ||
		!s.isEnv("part_size") || !s.isEnv("default_kind") || !s.isEnv("access_key") || !s.isEnv("secret_key") {
		t.Fatalf("provenance after Apply: %v", s.EnvFields())
	}
	if err := os.MkdirAll(c.SystemDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := BackingsFrom(st).Save(c.BackingsPath()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.BackingsPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"minio", "envbucket", `"p/"`, "envak", "envsk", `"native"`, "8388608", `"addressing": "path"`, "envFields"} {
		if bytes.Contains(data, []byte(leaked)) {
			t.Errorf("backings.json contains %s:\n%s", leaked, data)
		}
	}
	if !bytes.Contains(data, []byte("us-west-2")) {
		t.Errorf("explicit region not saved:\n%s", data)
	}
	// After a restart the environment supplies the values again, and a
	// changed environment value takes effect.
	vars["GOS3_BACKING_COLD_ENDPOINT"] = "http://minio2:9000"
	if err := c.LoadBackings(); err != nil {
		t.Fatal(err)
	}
	s = c.Storage.Backings[0].S3
	if s.Endpoint != "http://minio2:9000" || s.HomeBucket != "envbucket" || s.HomePrefix != "p/" || s.PartSize != 8<<20 ||
		s.DefaultKind != BucketKindNative || s.Region != "us-west-2" || s.SecretKey != "envsk" {
		t.Fatalf("reloaded %+v", *s)
	}
	// Changing an env-supplied value makes it explicit (and saved).
	pub = BackingsFrom(c.Storage).Public()
	pub.Backings[0].S3.HomeBucket = "typed"
	st, err = pub.Apply(c.Storage)
	if err != nil {
		t.Fatal(err)
	}
	if st.Backings[0].S3.isEnv("home_bucket") || !st.Backings[0].S3.isEnv("endpoint") {
		t.Fatalf("provenance after change: %v", st.Backings[0].S3.EnvFields())
	}
}
