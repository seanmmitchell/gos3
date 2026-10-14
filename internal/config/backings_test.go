package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateBackings(t *testing.T) {
	dir := t.TempDir()
	base := func() Config {
		c := Default()
		c.Storage.Path = filepath.Join(dir, "data")
		c.Storage.Backings = []BackingConfig{
			{Name: "fast", Backend: BackendFS, Path: filepath.Join(dir, "fast")},
			{Name: "scratch", Backend: BackendMemory},
		}
		c.Normalize()
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid backings: %v", err)
	}
	ok := base()
	ok.Storage.DefaultBacking = "fast"
	ok.Storage.Replication.DefaultMode = ReplicationActiveActive
	ok.Storage.Replication.DefaultReplicas = []string{"default", "scratch"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid replication: %v", err)
	}

	cases := []struct {
		name string
		mod  func(c *Config)
		want string
	}{
		{"reserved name", func(c *Config) { c.Storage.Backings[0].Name = "default" }, "storage.backings[0].name"},
		{"bad name", func(c *Config) { c.Storage.Backings[0].Name = "Fast!" }, "storage.backings[0].name"},
		{"duplicate name", func(c *Config) { c.Storage.Backings[1].Name = "fast" }, "used twice"},
		{"no path", func(c *Config) { c.Storage.Backings[0].Path = "" }, "storage.backings[0].path"},
		{"memory path", func(c *Config) { c.Storage.Backings[1].Path = dir }, "storage.backings[1].path"},
		{"bad kind", func(c *Config) { c.Storage.Backings[0].Backend = "tape" }, "storage.backings[0].backend"},
		{"same path as data", func(c *Config) { c.Storage.Backings[0].Path = c.Storage.Path }, "storage.path"},
		{"inside system dir", func(c *Config) {
			c.Storage.Backings[0].Path = filepath.Join(c.Storage.Path, SystemDirName, "x")
		}, "system directory"},
		{"system dir itself", func(c *Config) {
			c.Storage.Backings[0].Path = filepath.Join(c.Storage.Path, SystemDirName)
		}, "system directory"},
		{"memory system dir", func(c *Config) {
			c.Storage.Backend = BackendMemory
			c.Storage.Backings[0].Path = filepath.Join(c.Storage.Path, SystemDirName)
		}, "system directory"},
		{"unknown default", func(c *Config) { c.Storage.DefaultBacking = "nope" }, "storage.default_backing"},
		{"bad mode", func(c *Config) { c.Storage.Replication.DefaultMode = "mirror" }, "default_mode"},
		{"mode without replicas", func(c *Config) { c.Storage.Replication.DefaultMode = ReplicationActivePassive }, "default_replicas"},
		{"single with replicas", func(c *Config) {
			c.Storage.Replication.DefaultMode = ReplicationSingle
			c.Storage.Replication.DefaultReplicas = []string{"fast"}
		}, "single"},
		{"replica is default", func(c *Config) { c.Storage.Replication.DefaultReplicas = []string{"default"} }, "default_replicas[0]"},
		{"unknown replica", func(c *Config) { c.Storage.Replication.DefaultReplicas = []string{"nope"} }, "default_replicas[0]"},
		{"workers", func(c *Config) { c.Storage.Replication.Workers = 1000 }, "workers"},
		{"health interval", func(c *Config) { c.Storage.Replication.HealthInterval = 1 }, "health_interval"},
	}
	for _, tc := range cases {
		c := base()
		tc.mod(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want an error mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestBackingsEnv(t *testing.T) {
	c := Default()
	if err := c.Set("GOS3_STORAGE_BACKINGS", "fast=/mnt/nvme, slow=fs:/mnt/hdd,tmp=memory"); err != nil {
		t.Fatal(err)
	}
	want := []BackingConfig{
		{Name: "fast", Backend: BackendFS, Path: "/mnt/nvme"},
		{Name: "slow", Backend: BackendFS, Path: "/mnt/hdd"},
		{Name: "tmp", Backend: BackendMemory},
	}
	if len(c.Storage.Backings) != len(want) {
		t.Fatalf("backings = %+v", c.Storage.Backings)
	}
	for i := range want {
		if c.Storage.Backings[i] != want[i] {
			t.Errorf("backing %d = %+v, want %+v", i, c.Storage.Backings[i], want[i])
		}
	}
	if err := c.Set("storage.replication.default_replicas", "fast,slow"); err != nil || len(c.Storage.Replication.DefaultReplicas) != 2 {
		t.Fatalf("default replicas: %v %v", err, c.Storage.Replication.DefaultReplicas)
	}
	if got := c.BackingNames(); strings.Join(got, ",") != "default,fast,slow,tmp" {
		t.Errorf("BackingNames() = %v", got)
	}
}

func TestBackingsYAML(t *testing.T) {
	c := Default()
	err := decodeYAML([]byte(`
storage:
  backings:
    - name: fast
      path: /mnt/nvme
  replication:
    default_mode: active-passive
    default_replicas: [fast]
    auto_failover: true
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	c.Normalize()
	if len(c.Storage.Backings) != 1 || c.Storage.Backings[0].Backend != BackendFS || !c.Storage.Replication.AutoFailover {
		t.Fatalf("decoded %+v", c.Storage)
	}
	err = decodeYAML([]byte("storage:\n  backings:\n    - name: x\n      pth: /a\n"), &c)
	if err == nil || !strings.Contains(err.Error(), `unknown key "pth" in section "storage.backings"`) {
		t.Fatalf("unknown backing key: %v", err)
	}
}
