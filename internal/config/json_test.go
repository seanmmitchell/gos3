package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJSONConfigRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Server.ShutdownTimeout = 47 * time.Second
	cfg.Storage.Backings = []BackingConfig{{Name: "archive", Backend: "memory"}}
	cfg.Storage.Replication.HealthInterval = 15 * time.Second
	cfg.Discovery.BeaconInterval = 13 * time.Second
	cfg.CORS.AllowedOrigins = []string{"https://console.example"}
	cfg.Auth.RootSecretKey = "a-private-secret-not-for-output"
	data, err := cfg.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"shutdown_timeout": "47s"`)) ||
		!bytes.Contains(data, []byte(`"health_interval": "15s"`)) {
		t.Fatalf("wrong duration representation: %s", data)
	}
	got, err := load(writeFile(t, "config.json", string(data)), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	gotY, _ := got.YAML()
	wantY, _ := cfg.YAML()
	if string(gotY) != string(wantY) {
		t.Fatalf("round trip differs:\n%s\n%s", gotY, wantY)
	}
	redacted, err := cfg.Redacted().JSON()
	if err != nil || bytes.Contains(redacted, []byte(cfg.Auth.RootSecretKey)) {
		t.Fatal("secret leaked")
	}
}

func TestJSONConfigValidation(t *testing.T) {
	for _, input := range []string{
		`{"server":{"adress":":9000"}}`,
		`{"admin":{"enabled":true,"enabled":false}}`,
		`{"storage":{"backings":[{"name":"archive","typo":true}]}}`,
		`{"server":{"shutdown_timeout":"invalid"}}`,
		`{"admin":{"enabled":"false"}}`,
		`{"admin":{"enabled":1}}`,
		`{"server":{"max_connections":1.5}}`,
		`{"server":{"shutdown_timeout":30}}`,
		`{"admin":null}`,
		`{"server":{"domains":[12]}}`,
		`{"admin":{"enabled":false}} {}`,
		`{"admin":{"enabled":false},}`,
		"admin: {enabled: false}", "[]", "null", "",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := load(writeFile(t, "config.json", input), env(nil), nil); err == nil {
				t.Fatal("invalid JSON configuration accepted")
			}
		})
	}
}

func TestJSONLayersAndMinIOAliases(t *testing.T) {
	path := writeFile(t, "config.json", `{"admin":{"enabled":false},"server":{"region":"eu-west-1","address":":9001"},"auth":{"root_access_key":"file"}}`)
	cfg, err := load(path, env(map[string]string{
		"MINIO_ROOT_USER": "minio-root", "MINIO_ROOT_PASSWORD": "minio-private-secret",
		"GOS3_ROOT_ACCESS_KEY": "native-root", "MINIO_ADDRESS": ":9002", "MINIO_BROWSER": "on",
	}), []Override{{Key: "server.address", Value: ":9003"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Enabled || cfg.UI.Enabled || cfg.Server.Address != ":9003" ||
		cfg.Auth.RootAccessKey != "native-root" || cfg.Auth.RootSecretKey != "minio-private-secret" ||
		cfg.Server.Region != "eu-west-1" {
		t.Fatalf("layers: %+v", cfg)
	}
	for _, enabled := range []bool{false, true} {
		cfg.Admin.Enabled = enabled
		cfg.Storage.Path = t.TempDir()
		store := NewStore(SettingsFrom(cfg), cfg.SettingsPath())
		if _, err := store.Update(func(s *Settings) error { s.Region = "ap-south-1"; return nil }); err != nil {
			t.Fatal(err)
		}
		loaded, err := cfg.LoadSettings()
		if err != nil {
			t.Fatal(err)
		}
		want := "eu-west-1"
		if enabled {
			want = "ap-south-1"
		}
		if loaded.Get().Region != want {
			t.Fatalf("admin=%v: region=%s", enabled, loaded.Get().Region)
		}
		if !enabled && loaded.Path() != "" {
			t.Fatal("S3-only store can persist changes")
		}
	}
}

func TestMinIOAliasesDoNotOverrideNativeEmptyValues(t *testing.T) {
	cfg, err := load("", env(map[string]string{
		"MINIO_ROOT_USER": "alias", "GOS3_ROOT_ACCESS_KEY": "",
		"MINIO_ROOT_PASSWORD": "alias-secret", "GOS3_ROOT_SECRET_KEY": "",
		"MINIO_BROWSER": "off",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.RootAccessKey != "" || cfg.Auth.RootSecretKey != "" || cfg.UI.Enabled {
		t.Fatal("native values lost")
	}
	_, err = load("", env(map[string]string{"MINIO_BROWSER": "bad"}), nil)
	if err == nil || !strings.Contains(err.Error(), "MINIO_BROWSER") {
		t.Fatalf("alias error: %v", err)
	}
	defaults := Default()
	if !reflect.DeepEqual(cfg.Auth, defaults.Auth) {
		t.Fatal("credential defaults changed")
	}
}
