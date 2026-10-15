package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

func TestLiveBackingConfigurationSurvivesRestart(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	srv := start(t, cfg)
	const endpoint = "/_gos3/admin/v1/backings/config"
	var in multi.BackingConfiguration
	do(t, "GET", srv.url+endpoint, nil, &rootCreds, nil).json(t, &in)
	if !in.Persistent {
		t.Fatal("filesystem configuration is not persistent")
	}
	in.Backings = []config.BackingDefinition{{Name: "archive", Backend: "fs", Path: filepath.Join(t.TempDir(), "archive")}}
	in.DefaultBacking = "archive"
	raw, _ := json.Marshal(in)
	res := do(t, "PUT", srv.url+endpoint, raw, &rootCreds, nil)
	if res.status != 200 {
		t.Fatalf("configure: %d %s", res.status, res.body)
	}
	res.json(t, &in)
	res = do(t, "PUT", srv.url+"/photos", nil, &rootCreds, nil)
	if res.status != 200 {
		t.Fatalf("create bucket: %d %s", res.status, res.body)
	}
	res = do(t, "PUT", srv.url+"/photos/hello", []byte("live backing"), &rootCreds, nil)
	if res.status != 200 {
		t.Fatalf("write: %d %s", res.status, res.body)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	srv = start(t, cfg)
	res = do(t, "GET", srv.url+"/photos/hello", nil, &rootCreds, nil)
	if res.status != 200 || string(res.body) != "live backing" {
		t.Fatalf("read after restart: %d %s", res.status, res.body)
	}
	var restored multi.BackingConfiguration
	do(t, "GET", srv.url+endpoint, nil, &rootCreds, nil).json(t, &restored)
	// Revisions are not comparable across processes (they include a
	// per-process nonce), so only the settings are compared.
	if restored.DefaultBacking != "archive" || len(restored.Backings) != 1 || restored.Backings[0].Path != in.Backings[0].Path {
		t.Fatalf("restored: %+v", restored)
	}
	// An unchanged form must keep the existing backend open, even while in use.
	raw, _ = json.Marshal(restored)
	res = do(t, "PUT", srv.url+endpoint, raw, &rootCreds, nil)
	if res.status != 200 {
		t.Fatalf("unchanged save: %d %s", res.status, res.body)
	}
	// An empty filesystem backing can be added, edited and removed live.
	res.json(t, &restored)
	restored.Backings = append(restored.Backings, config.BackingDefinition{Name: "spare", Backend: "fs", Path: t.TempDir()})
	for step := range 3 {
		if step == 1 {
			restored.Backings[1].Path = t.TempDir()
		}
		if step == 2 {
			restored.Backings = restored.Backings[:1]
		}
		raw, _ = json.Marshal(restored)
		res = do(t, "PUT", srv.url+endpoint, raw, &rootCreds, nil)
		if res.status != 200 {
			t.Fatalf("step %d: %d %s", step, res.status, res.body)
		}
		res.json(t, &restored)
	}
}
