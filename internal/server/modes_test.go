package server

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDeploymentModes(t *testing.T) {
	for _, mode := range []struct {
		name      string
		admin, ui bool
	}{
		{"embedded", true, true}, {"managed", true, false}, {"s3-only", false, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Admin.Enabled, cfg.UI.Enabled = mode.admin, mode.ui
			srv := start(t, cfg, WithUIFS(fakeUI()))
			for _, method := range []string{"GET", "PUT", "OPTIONS"} {
				r := do(t, method, srv.url+"/_gos3/admin/v1/config", nil, &rootCreds, nil)
				if !mode.admin && r.status != 404 {
					t.Fatalf("%s admin: %d", method, r.status)
				}
				if mode.admin && method == "GET" && r.status != 200 {
					t.Fatalf("admin GET: %d %s", r.status, r.body)
				}
			}
			wantUI := mode.admin && mode.ui
			if (srv.updates != nil) != wantUI {
				t.Fatal("release checker must exist only when UI is enabled")
			}
			r := do(t, "GET", srv.url+"/_gos3/ui/", nil, nil, nil)
			if (r.status == 200) != wantUI {
				t.Fatalf("UI: %d", r.status)
			}
			r = do(t, "GET", srv.url+"/", nil, nil, map[string]string{"Accept": "text/html"})
			if (r.status == 302) != wantUI {
				t.Fatalf("redirect: %d", r.status)
			}
			r = do(t, "GET", srv.url+"/", nil, &rootCreds, nil)
			if r.status != 200 || !strings.Contains(string(r.body), "ListAllMyBucketsResult") {
				t.Fatalf("S3: %d %s", r.status, r.body)
			}
			info := srv.Info()
			for _, op := range []struct {
				method, path, body string
				status             int
			}{
				{"PUT", "/mode-bucket", "", 200},
				{"PUT", "/mode-bucket/object", "object-data", 200},
				{"GET", "/mode-bucket/object", "", 200},
				{"DELETE", "/mode-bucket/object", "", 204},
				{"DELETE", "/mode-bucket", "", 204},
			} {
				r := do(t, op.method, srv.url+op.path, []byte(op.body), &rootCreds, nil)
				if r.status != op.status {
					t.Fatalf("%s %s: %d %s", op.method, op.path, r.status, r.body)
				}
				if op.method == "GET" && string(r.body) != "object-data" {
					t.Fatalf("wrong data: %s", r.body)
				}
			}
			if (info.Endpoints.Admin != "") != mode.admin || slices.Contains(info.Features, "admin") != mode.admin ||
				(info.Endpoints.UI != "") != wantUI {
				t.Fatalf("info: %+v", info)
			}
		})
	}
}

func TestS3OnlyIgnoresBrokenSavedSettings(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Enabled = false
	cfg.Storage.Path = t.TempDir()
	dir := filepath.Join(cfg.Storage.Path, ".gos3")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("broken settings"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := start(t, cfg)
	if srv.Info().Region != cfg.Server.Region {
		t.Fatal("static settings not used")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "broken settings" {
		t.Fatal("saved settings modified")
	}
}

func TestMinIOHealthAliases(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Enabled = false
	cfg.Server.Domains = []string{"s3.test"}
	srv := start(t, cfg)
	for _, path := range []string{"/minio/health/live", "/minio/health/ready"} {
		for _, method := range []string{"GET", "HEAD"} {
			r := do(t, method, srv.url+path, nil, nil, nil)
			if r.status != 200 {
				t.Fatalf("%s %s: %d", method, path, r.status)
			}
		}
		for _, headers := range []map[string]string{nil, {"Host": "bucket.s3.test"}} {
			r := do(t, "GET", srv.url+path, nil, &rootCreds, headers)
			if r.status != 404 || !strings.Contains(string(r.body), "NoSuchBucket") {
				t.Fatalf("object masked by probe: %d %s", r.status, r.body)
			}
		}
		r := do(t, "GET", srv.url+path+"?X-Amz-Signature=invalid", nil, nil, nil)
		if r.status == 200 {
			t.Fatal("presigned request masked by probe")
		}
	}
	srv.draining.Store(true)
	if r := do(t, "GET", srv.url+"/minio/health/ready", nil, nil, nil); r.status != 503 {
		t.Fatalf("draining: %d", r.status)
	}
	if r := do(t, "GET", srv.url+"/minio/health/live", nil, nil, nil); r.status != 200 {
		t.Fatalf("liveness: %d", r.status)
	}
}
