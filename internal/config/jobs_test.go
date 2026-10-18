package config

import (
	"strings"
	"testing"
)

func TestJobsDefaults(t *testing.T) {
	c := Default()
	if c.Jobs.Workers != DefaultJobsWorkers || DefaultJobsWorkers != 2 {
		t.Errorf("jobs.workers = %d", c.Jobs.Workers)
	}
	if c.Restic.RestoreSpoolBytes != DefaultRestoreSpoolBytes {
		t.Errorf("restic.restore_spool_bytes = %d", c.Restic.RestoreSpoolBytes)
	}
	s := c.Restic.Snapshots
	if s.Workers != 1 || s.StageDir != "" || s.SpoolBytes != 8<<30 || !s.Incremental {
		t.Errorf("restic.snapshots = %+v", s)
	}
	// Zero values from a file mean the defaults.
	var z Config
	z.Normalize()
	if z.Jobs.Workers != DefaultJobsWorkers || z.Restic.Snapshots.Workers != DefaultSnapshotWorkers ||
		z.Restic.Snapshots.SpoolBytes != DefaultSnapshotSpoolBytes || z.Restic.RestoreSpoolBytes != DefaultRestoreSpoolBytes {
		t.Errorf("normalized zero config: %+v %+v", z.Jobs, z.Restic)
	}
}

func TestJobsKeysFromLayers(t *testing.T) {
	file := writeFile(t, "gos3.yaml", `
jobs:
  workers: 4
restic:
  restore_spool_bytes: 536870912
  snapshots:
    workers: 2
    stage_dir: " /var/tmp/stage "
    spool_bytes: 1073741824
    incremental: false
`)
	c, err := load(file, env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s := c.Restic.Snapshots
	if c.Jobs.Workers != 4 || c.Restic.RestoreSpoolBytes != 512<<20 || s.Workers != 2 || s.StageDir != "/var/tmp/stage" || s.SpoolBytes != 1<<30 || s.Incremental {
		t.Fatalf("from file: %+v %+v", c.Jobs, s)
	}

	c, err = load(file, env(map[string]string{
		"GOS3_JOBS_WORKERS":                 "8",
		"GOS3_RESTIC_RESTORE_SPOOL_BYTES":   "256MiB",
		"GOS3_RESTIC_SNAPSHOTS_WORKERS":     "3",
		"GOS3_RESTIC_SNAPSHOTS_STAGE_DIR":   "/mnt/stage",
		"GOS3_RESTIC_SNAPSHOTS_SPOOL_BYTES": "512MiB",
		"GOS3_RESTIC_SNAPSHOTS_INCREMENTAL": "yes",
	}), []Override{{Key: "jobs.workers", Value: "16"}})
	if err != nil {
		t.Fatal(err)
	}
	s = c.Restic.Snapshots
	if c.Jobs.Workers != 16 || c.Restic.RestoreSpoolBytes != 256<<20 || s.Workers != 3 || s.StageDir != "/mnt/stage" || s.SpoolBytes != 512<<20 || !s.Incremental {
		t.Fatalf("env and override: %+v %+v", c.Jobs, s)
	}

	// JSON uses the same keys.
	jf := writeFile(t, "gos3.json", `{"jobs":{"workers":3},"restic":{"snapshots":{"spool_bytes":134217728,"incremental":false}}}`)
	c, err = load(jf, env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Jobs.Workers != 3 || c.Restic.Snapshots.SpoolBytes != 128<<20 || c.Restic.Snapshots.Incremental ||
		c.Restic.Snapshots.Workers != DefaultSnapshotWorkers {
		t.Fatalf("from JSON: %+v %+v", c.Jobs, c.Restic)
	}

	// Unknown keys name their section.
	bad := writeFile(t, "bad.yaml", "restic:\n  snapshots:\n    spool: 1\n")
	if _, err := load(bad, env(nil), nil); err == nil || !strings.Contains(err.Error(), `unknown key "spool" in section "restic.snapshots"`) {
		t.Fatalf("unknown key: %v", err)
	}
	bad = writeFile(t, "bad.yaml", "jobs:\n  concurrency: 1\n")
	if _, err := load(bad, env(nil), nil); err == nil || !strings.Contains(err.Error(), `in section "jobs"`) {
		t.Fatalf("unknown jobs key: %v", err)
	}
}

func TestJobsValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // "" = valid
	}{
		{"defaults", func(*Config) {}, ""},
		{"workers max", func(c *Config) { c.Jobs.Workers = 64 }, ""},
		{"workers high", func(c *Config) { c.Jobs.Workers = 65 }, "jobs.workers"},
		{"workers negative", func(c *Config) { c.Jobs.Workers = -1 }, "jobs.workers"},
		{"snapshot workers high", func(c *Config) { c.Restic.Snapshots.Workers = 100 }, "restic.snapshots.workers"},
		{"snapshot workers negative", func(c *Config) { c.Restic.Snapshots.Workers = -2 }, "restic.snapshots.workers"},
		{"spool min", func(c *Config) { c.Restic.Snapshots.SpoolBytes = 64 << 20 }, ""},
		{"spool low", func(c *Config) { c.Restic.Snapshots.SpoolBytes = 64<<20 - 1 }, "restic.snapshots.spool_bytes"},
		{"spool negative", func(c *Config) { c.Restic.Snapshots.SpoolBytes = -1 }, "restic.snapshots.spool_bytes"},
		{"restore spool min", func(c *Config) { c.Restic.RestoreSpoolBytes = 64 << 20 }, ""},
		{"restore spool low", func(c *Config) { c.Restic.RestoreSpoolBytes = 64<<20 - 1 }, "restic.restore_spool_bytes"},
		{"stage dir", func(c *Config) { c.Restic.Snapshots.StageDir = "/srv/stage" }, ""},
		{"stage dir nul", func(c *Config) { c.Restic.Snapshots.StageDir = "a\x00b" }, "restic.snapshots.stage_dir"},
	}
	for _, tc := range tests {
		c := Default()
		tc.mutate(&c)
		err := c.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want+":")):
			t.Errorf("%s: got %v, want an error about %s", tc.name, err, tc.want)
		}
	}
	// Sizes from the environment are parsed and validated.
	c := Default()
	if err := c.Set("GOS3_RESTIC_SNAPSHOTS_SPOOL_BYTES", "8 GiB"); err != nil || c.Restic.Snapshots.SpoolBytes != 8<<30 {
		t.Fatalf("size: %v %d", err, c.Restic.Snapshots.SpoolBytes)
	}
	if err := c.Set("restic.snapshots.spool_bytes", "lots"); err == nil || !strings.Contains(err.Error(), "restic.snapshots.spool_bytes") {
		t.Fatalf("bad size: %v", err)
	}
}
