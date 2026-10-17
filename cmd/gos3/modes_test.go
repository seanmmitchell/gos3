package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
)

func TestMinIOStartupArguments(t *testing.T) {
	clearEnv(t)
	for _, args := range [][]string{
		{"/data", "--address", ":9100", "--no-admin"},
		{"--address", ":9100", "/data", "--no-admin"},
		{"--no-admin", "--address", ":9100", "/data"},
	} {
		fs := newFlagSet("server", "server", &bytes.Buffer{})
		cf := addConfigFlags(fs)
		if err := parseServerFlags(fs, args); err != nil {
			t.Fatal(err)
		}
		cfg, err := cf.load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Address != ":9100" || cfg.Storage.Path != filepath.Clean("/data") || cfg.Admin.Enabled || cfg.UI.Enabled {
			t.Fatalf("incorrect startup config: %+v", cfg)
		}
	}
	for _, args := range [][]string{
		{"/one", "/two"}, {"http://host/disk"}, {"/disk{1...4}"}, {"--data", "/one", "/two"}, {"--unknown"},
	} {
		fs := newFlagSet("server", "server", &bytes.Buffer{})
		addConfigFlags(fs)
		if err := parseServerFlags(fs, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if code, _, errout := runCmd(t, "server", "/data", "--console-address", ":9001"); code == 0 || !strings.Contains(errout, "UI on --address") {
		t.Fatalf("console diagnostic: %d %s", code, errout)
	}
}

func TestJSONCommandsAndSettingsPrecedence(t *testing.T) {
	clearEnv(t)
	file := filepath.Join(t.TempDir(), "gos3.json")
	if code, _, stderr := runCmd(t, "config", "init", "-o", file); code != 0 {
		t.Fatal(stderr)
	}
	data, err := os.ReadFile(file)
	if err != nil || !json.Valid(data) {
		t.Fatalf("init JSON: %v %s", err, data)
	}
	if _, err := config.Load(file); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCmd(t, "config", "init", "-o", file); code == 0 {
		t.Fatal("overwrote config")
	}
	if code, out, stderr := runCmd(t, "config", "init", "--json"); code != 0 || !json.Valid([]byte(out)) {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	t.Setenv("GOS3_ROOT_ACCESS_KEY", "private-root")
	t.Setenv("GOS3_ROOT_SECRET_KEY", "private-root-secret-0123456789")
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.Path = dir
	store := config.NewStore(config.SettingsFrom(cfg), cfg.SettingsPath())
	if _, err := store.Update(func(s *config.Settings) error { s.Region = "eu-west-1"; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, admin := range []bool{false, true} {
		args := []string{"config", "print", "--json", "--data", dir, "--region", "ap-south-1"}
		want := "eu-west-1"
		if !admin {
			args = append(args, "--no-admin")
			want = "ap-south-1"
		}
		code, out, stderr := runCmd(t, args...)
		if code != 0 || !json.Valid([]byte(out)) || strings.Contains(out, "private-root-secret") {
			t.Fatalf("print: %d %s %s", code, out, stderr)
		}
		var doc struct {
			Server struct{ Region string }
			Admin  struct{ Enabled bool }
			UI     struct{ Enabled bool }
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Server.Region != want || doc.Admin.Enabled != admin || (!admin && doc.UI.Enabled) {
			t.Fatalf("effective config: %s", out)
		}
	}
}

func TestJSONExamples(t *testing.T) {
	clearEnv(t)
	for _, name := range []string{"s3-only", "managed", "embedded"} {
		path := filepath.Join("..", "..", "deploy", "config", name+".json")
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Admin.Enabled != (name != "s3-only") || cfg.UI.Enabled != (name == "embedded") {
			t.Fatalf("%s has incorrect mode", name)
		}
		d := cfg.Discovery
		if d.Enabled || d.Advertise || d.MDNS || d.Beacon || d.AllowScan {
			t.Fatalf("%s enabled discovery", name)
		}
	}
}
