package config

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// deprecationKeys renders Deprecations as "backing.key" entries.
func deprecationKeys(c Config) []string {
	var out []string
	for _, d := range c.Deprecations() {
		out = append(out, d.Backing+"."+d.Key)
	}
	return out
}

// The deprecated keys bucket, prefix and gateway keep working in every
// layer, map to home_bucket, home_prefix and default_kind native, are
// reported by Deprecations and are never printed.
func TestS3LegacyKeys(t *testing.T) {
	dir := t.TempDir()
	check := func(t *testing.T, c Config, wantDeps ...string) {
		t.Helper()
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, s := range []*S3Config{c.Storage.S3, c.Storage.Backings[0].S3} {
			if s.HomeBucket != "bk" || s.HomePrefix != "p/" || s.DefaultKind != BucketKindNative ||
				s.LegacyBucket != "" || s.LegacyPrefix != "" || s.LegacyGateway {
				t.Errorf("mapped %+v", *s)
			}
		}
		if got := deprecationKeys(c); !slices.Equal(got, wantDeps) {
			t.Errorf("Deprecations = %v, want %v", got, wantDeps)
		}
		y, err := c.Redacted().YAML()
		if err != nil {
			t.Fatal(err)
		}
		for _, legacy := range []string{"bucket: bk", "prefix: p/", "gateway"} {
			if bytes.Contains(y, []byte("    "+legacy)) || bytes.Contains(y, []byte("      "+legacy)) {
				t.Errorf("printed configuration has %q:\n%s", legacy, y)
			}
		}
		if !bytes.Contains(y, []byte("home_bucket: bk")) || !bytes.Contains(y, []byte("default_kind: native")) {
			t.Errorf("printed configuration lacks the new keys:\n%s", y)
		}
		j, err := c.Redacted().JSON()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(j, []byte(`"bucket"`)) || bytes.Contains(j, []byte(`"gateway"`)) || bytes.Contains(j, []byte(`"prefix"`)) {
			t.Errorf("printed JSON has deprecated keys:\n%s", j)
		}
	}
	all := []string{"default.bucket", "default.prefix", "default.gateway", "cold.bucket", "cold.prefix", "cold.gateway"}

	t.Run("yaml", func(t *testing.T) {
		c, err := load(writeFile(t, "c.yaml", `
storage:
  backend: s3
  path: `+dir+`
  s3: {endpoint: "http://minio:9000", bucket: bk, prefix: p, gateway: true, access_key: a, secret_key: s}
  backings:
    - name: cold
      backend: s3
      s3: {endpoint: "http://other:9000", bucket: " bk ", prefix: p/, gateway: true, access_key: a, secret_key: s}
`), env(nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, c, all...)
	})
	t.Run("json", func(t *testing.T) {
		c, err := load(writeFile(t, "c.json", `{"storage": {"backend": "s3", "path": "`+filepath.ToSlash(dir)+`",
			"s3": {"endpoint": "http://minio:9000", "bucket": "bk", "prefix": "p/", "gateway": true, "access_key": "a", "secret_key": "s"},
			"backings": [{"name": "cold", "backend": "s3", "s3": {"endpoint": "http://other:9000", "bucket": "bk", "prefix": "p",
				"gateway": true, "access_key": "a", "secret_key": "s"}}]}}`), env(nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, c, all...)
	})
	t.Run("env", func(t *testing.T) {
		c, err := load("", env(map[string]string{
			"GOS3_STORAGE_BACKEND": "s3", "GOS3_DATA": dir,
			"GOS3_S3_ENDPOINT": "http://minio:9000", "GOS3_S3_ACCESS_KEY": "a", "GOS3_S3_SECRET_KEY": "s",
			"GOS3_S3_BUCKET": "bk", "GOS3_S3_PREFIX": "p", "GOS3_S3_GATEWAY": "yes",
			"GOS3_STORAGE_BACKINGS":      "cold=s3",
			"GOS3_BACKING_COLD_ENDPOINT": "http://other:9000", "GOS3_BACKING_COLD_ACCESS_KEY": "a", "GOS3_BACKING_COLD_SECRET_KEY": "s",
			"GOS3_BACKING_COLD_BUCKET": "bk", "GOS3_BACKING_COLD_PREFIX": "p", "GOS3_BACKING_COLD_GATEWAY": "true",
		}), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, c, all...)
		// Values from deprecated variables are from the environment too.
		if s := c.Storage.Backings[0].S3; !s.isEnv("home_bucket") || !s.isEnv("home_prefix") || !s.isEnv("default_kind") {
			t.Errorf("provenance %v", s.EnvFields())
		}
	})
	t.Run("same value under both keys", func(t *testing.T) {
		c, err := load(writeFile(t, "c.yaml", `
storage:
  backend: s3
  path: `+dir+`
  s3: {endpoint: "http://minio:9000", bucket: bk, home_bucket: bk, prefix: p/, home_prefix: p, gateway: true, default_kind: native, access_key: a, secret_key: s}
  backings:
    - name: cold
      backend: s3
      s3: {endpoint: "http://other:9000", home_bucket: bk, home_prefix: p, default_kind: native, access_key: a, secret_key: s}
`), env(map[string]string{"GOS3_BACKING_COLD_BUCKET": "bk", "GOS3_BACKING_COLD_HOME_BUCKET": "bk",
			"GOS3_BACKING_COLD_GATEWAY": "1", "GOS3_BACKING_COLD_DEFAULT_KIND": "Native"}), nil)
		if err != nil {
			t.Fatal(err)
		}
		// The environment fills nothing here: the file configures every field.
		check(t, c, "default.bucket", "default.prefix", "default.gateway")
	})
	t.Run("gateway false", func(t *testing.T) {
		c, err := load(writeFile(t, "c.yaml", `
storage:
  backend: s3
  s3: {endpoint: "http://minio:9000", bucket: bk, gateway: false, access_key: a, secret_key: s}
  # GOS3_DATA gives the homeless backing its system directory.
`), env(map[string]string{"GOS3_STORAGE_BACKINGS": "cold=s3", "GOS3_DATA": dir, "GOS3_BACKING_COLD_ENDPOINT": "http://x",
			"GOS3_BACKING_COLD_ACCESS_KEY": "a", "GOS3_BACKING_COLD_SECRET_KEY": "s", "GOS3_BACKING_COLD_GATEWAY": "false"}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if k := c.Storage.S3.DefaultKind; k != BucketKindWrapped {
			t.Errorf("default kind %q", k)
		}
		if s := c.Storage.Backings[0].S3; s.DefaultKind != BucketKindNative || s.isEnv("default_kind") {
			t.Errorf("backing %+v", *s)
		}
		if got := deprecationKeys(c); !slices.Equal(got, []string{"default.bucket"}) {
			t.Errorf("Deprecations = %v", got)
		}
	})
	t.Run("deprecated file key wins over the new variable", func(t *testing.T) {
		c, err := load(writeFile(t, "c.yaml", `
storage:
  backings:
    - name: cold
      backend: s3
      s3: {endpoint: "http://x", bucket: yamlbk, prefix: yp, gateway: true, access_key: a, secret_key: s}
`), env(map[string]string{"GOS3_BACKING_COLD_HOME_BUCKET": "envbk", "GOS3_BACKING_COLD_HOME_PREFIX": "ep",
			"GOS3_BACKING_COLD_DEFAULT_KIND": "wrapped"}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if s := c.Storage.Backings[0].S3; s.HomeBucket != "yamlbk" || s.HomePrefix != "yp/" || s.DefaultKind != BucketKindNative || len(s.EnvFields()) != 0 {
			t.Errorf("backing %+v (env %v)", *s, s.EnvFields())
		}
	})
	t.Run("deprecation message", func(t *testing.T) {
		got := Deprecation{Backing: "cold-tier", Key: "gateway", Use: "default_kind: native"}.String()
		for _, want := range []string{`"cold-tier"`, "gateway", "GOS3_BACKING_COLD_TIER_GATEWAY", "GOS3_BACKING_COLD_TIER_DEFAULT_KIND=native"} {
			if !strings.Contains(got, want) {
				t.Errorf("%q lacks %q", got, want)
			}
		}
		got = Deprecation{Backing: DefaultBackingName, Key: "bucket", Use: "home_bucket"}.String()
		if !strings.Contains(got, "GOS3_S3_BUCKET") || !strings.Contains(got, "GOS3_S3_HOME_BUCKET") {
			t.Errorf("default backing message %q", got)
		}
	})
}

// A deprecated key that disagrees with its replacement is refused.
func TestS3LegacyConflicts(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, yaml string
		want       string
	}{
		{"bucket", "bucket: a, home_bucket: b", `storage.s3.bucket: the deprecated key bucket ("a") conflicts with home_bucket ("b")`},
		{"prefix", "home_bucket: b, prefix: x, home_prefix: y", `storage.s3.prefix: the deprecated key prefix ("x/") conflicts with home_prefix ("y/")`},
		{"gateway", "home_bucket: b, gateway: true, default_kind: wrapped", `storage.s3.gateway: the deprecated key gateway (native buckets) conflicts with default_kind "wrapped"`},
	} {
		c, err := load(writeFile(t, "c.yaml", "storage:\n  backend: s3\n  path: "+dir+"\n  s3: {endpoint: \"http://x\", access_key: a, secret_key: s, "+tc.yaml+"}\n"), env(nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want %q", tc.name, err, tc.want)
		}
	}
	// The table variables of storage.s3 meet in the same check.
	c, err := load("", env(map[string]string{"GOS3_STORAGE_BACKEND": "s3", "GOS3_S3_ENDPOINT": "http://x", "GOS3_S3_ACCESS_KEY": "a",
		"GOS3_S3_SECRET_KEY": "s", "GOS3_S3_BUCKET": "a", "GOS3_S3_HOME_BUCKET": "b"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "storage.s3.bucket") || !strings.Contains(err.Error(), "GOS3_S3_HOME_BUCKET") {
		t.Errorf("GOS3_S3_BUCKET vs GOS3_S3_HOME_BUCKET: %v", err)
	}
	// GOS3_BACKING_<NAME>_* aliases that disagree with their replacement.
	for _, tc := range []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"GOS3_BACKING_C_BUCKET": "a", "GOS3_BACKING_C_HOME_BUCKET": "b"}, "GOS3_BACKING_C_HOME_BUCKET: conflicts with GOS3_BACKING_C_BUCKET"},
		{map[string]string{"GOS3_BACKING_C_PREFIX": "a", "GOS3_BACKING_C_HOME_PREFIX": "b/"}, "GOS3_BACKING_C_HOME_PREFIX: conflicts with GOS3_BACKING_C_PREFIX"},
		{map[string]string{"GOS3_BACKING_C_GATEWAY": "on", "GOS3_BACKING_C_DEFAULT_KIND": "wrapped"}, "GOS3_BACKING_C_DEFAULT_KIND: conflicts with GOS3_BACKING_C_GATEWAY"},
		{map[string]string{"GOS3_BACKING_C_GATEWAY": "maybe"}, "GOS3_BACKING_C_GATEWAY: invalid boolean"},
	} {
		tc.vars["GOS3_STORAGE_BACKINGS"] = "c=s3"
		if _, err := load("", env(tc.vars), nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.vars, err, tc.want)
		}
	}
	// Same through the admin API.
	base := StorageConfig{Backend: BackendMemory, Path: dir}
	in := BackingSettings{DefaultBacking: "default", Backings: []BackingDefinition{s3Def("cold", "b", "")}}
	in.Backings[0].S3.LegacyBucket = "a"
	if _, err := in.Apply(base); err == nil || !strings.Contains(err.Error(), "storage.backings[0].s3.bucket: the deprecated key bucket") {
		t.Errorf("Apply with a conflicting bucket: %v", err)
	}
}

// backings.json written by earlier versions (bucket, prefix, gateway)
// loads into the new fields, and is rewritten with the new keys only.
func TestS3LegacyBackingsJSON(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.Storage.Path = dir
	if err := os.MkdirAll(c.SystemDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{
  "backings": [
    {"name": "cold", "backend": "s3", "path": "", "s3": {"endpoint": "http://minio:9000", "region": "", "bucket": "bk", "prefix": "cold/",
      "addressing": "", "gateway": false, "accessKey": "ak", "secretKey": "sk", "secretKeyConfigured": false, "encryptionKeyConfigured": false}},
    {"name": "gw", "backend": "s3", "path": "", "s3": {"endpoint": "http://minio:9000", "region": "", "bucket": "meta", "prefix": "",
      "addressing": "", "gateway": true, "accessKey": "ak", "secretKey": "sk", "secretKeyConfigured": false, "encryptionKeyConfigured": false}}
  ],
  "defaultBacking": "gw"
}
`
	if err := os.WriteFile(c.BackingsPath(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadBackings(); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	cold, gw := c.Storage.Backings[0].S3, c.Storage.Backings[1].S3
	if cold.HomeBucket != "bk" || cold.HomePrefix != "cold/" || cold.DefaultKind != BucketKindWrapped {
		t.Errorf("cold %+v", *cold)
	}
	if gw.HomeBucket != "meta" || gw.HomePrefix != "" || gw.DefaultKind != BucketKindNative || c.Storage.DefaultBacking != "gw" {
		t.Errorf("gw %+v", *gw)
	}
	if got := deprecationKeys(c); !slices.Equal(got, []string{"cold.bucket", "cold.prefix", "gw.bucket", "gw.gateway"}) {
		t.Errorf("Deprecations = %v", got)
	}
	// The identities follow the home, as they did before for bucket/prefix.
	defs := BackingsFrom(c.Storage)
	if a, b := defs.Backings[0].Identity(), defs.Backings[1].Identity(); a != "s3:home:bk/cold/" || b != "s3:home:meta/" {
		t.Errorf("identities %q, %q", a, b)
	}
	if err := defs.Save(c.BackingsPath()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.BackingsPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{`"bucket"`, `"prefix"`, `"gateway"`} {
		if bytes.Contains(data, []byte(legacy)) {
			t.Errorf("rewritten backings.json has %s:\n%s", legacy, data)
		}
	}
	if !bytes.Contains(data, []byte(`"homeBucket": "meta"`)) || !bytes.Contains(data, []byte(`"defaultKind": "native"`)) {
		t.Errorf("rewritten backings.json:\n%s", data)
	}
	// It loads again to the same backings, without deprecations.
	c2 := Default()
	c2.Storage.Path = dir
	if err := c2.LoadBackings(); err != nil {
		t.Fatal(err)
	}
	if len(c2.Deprecations()) != 0 || !BackingsFrom(c2.Storage).Backings[1].Equal(defs.Backings[1]) || !BackingsFrom(c2.Storage).Backings[0].Equal(defs.Backings[0]) {
		t.Errorf("reloaded %+v %+v", *c2.Storage.Backings[0].S3, *c2.Storage.Backings[1].S3)
	}
}

// An empty default kind is derived from the home once every layer is in,
// and follows the home through live changes unless it was chosen.
func TestS3DefaultKind(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"GOS3_BACKING_A_HOME_BUCKET": "abk", "GOS3_BACKING_C_DEFAULT_KIND": "native"}
	withLookupEnv(t, vars)
	// The environment supplies the home after the file: the kind follows.
	c, err := load(writeFile(t, "c.yaml", `
storage:
  path: `+dir+`
  backings:
    - {name: a, backend: s3, s3: {endpoint: "http://x", access_key: a, secret_key: s}}
    - {name: b, backend: s3, s3: {endpoint: "http://x", access_key: a, secret_key: s}}
    - {name: c, backend: s3, s3: {endpoint: "http://x", home_bucket: cbk, access_key: a, secret_key: s}}
`), env(vars), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	kinds := func(st StorageConfig) string {
		var k []string
		for _, b := range st.Backings {
			k = append(k, b.S3.DefaultKind)
		}
		return strings.Join(k, ",")
	}
	if got := kinds(c.Storage); got != "wrapped,native,native" {
		t.Fatalf("derived kinds %s", got)
	}

	// A live change sending the read kinds back with the homes swapped:
	// derived kinds follow the homes, a chosen one is kept.
	pub := BackingsFrom(c.Storage).Public()
	pub.Backings[0].S3.HomeBucket = "" // was from the environment
	pub.Backings[1].S3.HomeBucket = "bbk"
	st, err := pub.Apply(c.Storage)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(st); got != "wrapped,wrapped,native" {
		// a: the environment still supplies its home (a blank field is filled again).
		t.Fatalf("kinds after the change %s", got)
	}
	pub = BackingsFrom(c.Storage).Public()
	pub.Backings[1].S3.HomeBucket = "bbk"
	pub.Backings[2].S3.HomeBucket = ""
	pub.Backings[2].S3.DefaultKind = "" // c's kind came from the environment, which supplies it again
	st, err = pub.Apply(c.Storage)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(st); got != "wrapped,wrapped,native" || !st.Backings[2].S3.isEnv("default_kind") {
		t.Fatalf("kinds after the change %s", got)
	}

	// A kind chosen in the change is kept; wrapped needs a home.
	pub = BackingsFrom(st).Public()
	pub.Backings[1].S3.DefaultKind = BucketKindNative
	st2, err := pub.Apply(st)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(st2); got != "wrapped,native,native" {
		t.Fatalf("chosen kind %s", got)
	}
	pub = BackingsFrom(st).Public()
	pub.Backings[2].S3.DefaultKind = BucketKindWrapped
	if _, err := pub.Apply(st); err == nil || !strings.Contains(err.Error(), "wrapped buckets need a container") {
		t.Fatalf("wrapped without a home: %v", err)
	}
	// Removing the home of a backing whose kind was derived from it
	// derives it again instead of failing.
	pub = BackingsFrom(st).Public()
	pub.Backings[1].S3.HomeBucket = ""
	st3, err := pub.Apply(st)
	if err != nil {
		t.Fatal(err)
	}
	if got := st3.Backings[1].S3.DefaultKind; got != BucketKindNative {
		t.Fatalf("kind after removing the home %s", got)
	}
	// An explicit native kind with a home survives the round trip.
	pub = BackingsFrom(st2).Public()
	st4, err := pub.Apply(st2)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(st4); got != "wrapped,native,native" {
		t.Fatalf("round trip %s", got)
	}
}

// The default kind does not change the identity, so it can change live;
// the home does.
func TestS3IdentityIgnoresKindAndConnection(t *testing.T) {
	a := s3Def("cold", "bk", "p/")
	b := s3Def("cold", "BK", "p")
	b.S3.DefaultKind = BucketKindNative
	b.S3.Endpoint, b.S3.Region, b.S3.AccessKey, b.S3.SecretKey, b.S3.EncryptionKey = "https://y", "r", "x", "y", ""
	b.S3.Addressing, b.S3.PartSize = "virtual", 32<<20
	if a.Identity() != b.Identity() || a.Identity() != "s3:home:bk/p/" {
		t.Errorf("identities %q, %q", a.Identity(), b.Identity())
	}
	l1, l2 := s3Def("cold", "", ""), s3Def("warm", "", "")
	if l1.Identity() == l2.Identity() || l1.Identity() == a.Identity() {
		t.Errorf("local identities %q, %q", l1.Identity(), l2.Identity())
	}
}
