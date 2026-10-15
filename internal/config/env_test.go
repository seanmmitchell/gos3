package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestEnvVarsTable(t *testing.T) {
	// The variables documented for operators must all exist.
	required := []string{"GOS3_CONFIG", "GOS3_ADDRESS", "GOS3_NAME", "GOS3_REGION", "GOS3_PUBLIC_URL",
		"GOS3_DOMAINS", "GOS3_TLS_CERT", "GOS3_TLS_KEY", "GOS3_DATA", "GOS3_STORAGE_BACKEND", "GOS3_FSYNC",
		"GOS3_ROOT_ACCESS_KEY", "GOS3_ROOT_SECRET_KEY", "GOS3_UI_ENABLED", "GOS3_UI_REDIRECT",
		"GOS3_CORS_ORIGINS", "GOS3_DISCOVERY_ENABLED", "GOS3_DISCOVERY_ADVERTISE", "GOS3_DISCOVERY_SEEDS",
		"GOS3_DISCOVERY_ALLOW_SCAN", "GOS3_LOG_LEVEL", "GOS3_LOG_FORMAT", "GOS3_TRUST_PROXY_HEADERS",
		"GOS3_MAX_OBJECT_SIZE", "GOS3_METRICS_PUBLIC", "GOS3_PUBLIC_INFO"}
	vars := EnvVars()
	byName := map[string]EnvVar{}
	keys := map[string]bool{}
	for _, v := range vars {
		if _, dup := byName[v.Name]; dup {
			t.Errorf("duplicate variable %s", v.Name)
		}
		byName[v.Name] = v
		if v.Description == "" {
			t.Errorf("%s has no description", v.Name)
		}
		if v.Name != EnvConfig {
			if v.Key == "" || keys[v.Key] {
				t.Errorf("%s: empty or duplicate key %q", v.Name, v.Key)
			}
			keys[v.Key] = true
		}
	}
	for _, n := range required {
		if _, ok := byName[n]; !ok {
			t.Errorf("missing %s", n)
		}
	}
	// EnvVars returns a copy.
	vars[0].Name = "X"
	if EnvVars()[0].Name == "X" {
		t.Error("EnvVars exposes the internal table")
	}
	if got := Keys(); len(got) != len(keys) {
		t.Errorf("Keys() has %d entries, want %d", len(got), len(keys))
	}

	var buf bytes.Buffer
	if err := WriteEnvTable(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"VARIABLE", "GOS3_ADDRESS", "server.address", "GOS3_UI_REDIRECT ", "ui.redirect_root"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

// TestEveryKeySettable sets every key through Config.Set with a valid value
// of its type and checks that the configuration changed, so the table's
// keys and setters agree.
func TestEveryKeySettable(t *testing.T) {
	samples := []string{"true", "false", "42", "1h", "2h", "2GiB", "x.example", "a.example,b.example", `[{"name":"x"}]`}
	for _, key := range Keys() {
		changed := false
		for _, v := range samples {
			c := Default()
			before, _ := c.YAML()
			if c.Set(key, v) != nil {
				continue
			}
			after, _ := c.YAML()
			if !bytes.Equal(before, after) {
				changed = true
				break
			}
		}
		if !changed {
			t.Errorf("key %q cannot be set", key)
		}
	}
}

func TestSet(t *testing.T) {
	c := Default()
	if err := c.Set("server.address", ":1234"); err != nil || c.Server.Address != ":1234" {
		t.Fatalf("by key: %v %q", err, c.Server.Address)
	}
	if err := c.Set("GOS3_DISCOVERY_SEEDS", "http://a:9000, http://b:9000"); err != nil || len(c.Discovery.Seeds) != 2 {
		t.Fatalf("by env name: %v %v", err, c.Discovery.Seeds)
	}
	if err := c.Set("discovery.beacon_interval", "15s"); err != nil || c.Discovery.BeaconInterval.String() != "15s" {
		t.Fatalf("duration: %v", err)
	}
	if err := c.Set("cors.max_age_seconds", "x"); err == nil || !strings.Contains(err.Error(), "cors.max_age_seconds") {
		t.Errorf("bad int: %v", err)
	}
	if err := c.Set("GOS3_CONFIG", "x.yaml"); err == nil {
		t.Error("GOS3_CONFIG is not a configuration key")
	}
	if err := c.Set("nope", "1"); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestParseBool(t *testing.T) {
	for _, v := range []string{"1", "t", "TRUE", "yes", " on ", "Y"} {
		if b, err := ParseBool(v); err != nil || !b {
			t.Errorf("ParseBool(%q) = %v, %v", v, b, err)
		}
	}
	for _, v := range []string{"0", "f", "False", "no", "off", "N"} {
		if b, err := ParseBool(v); err != nil || b {
			t.Errorf("ParseBool(%q) = %v, %v", v, b, err)
		}
	}
	for _, v := range []string{"", "2", "enable"} {
		if _, err := ParseBool(v); err == nil {
			t.Errorf("ParseBool(%q) accepted", v)
		}
	}
}

func TestSplitList(t *testing.T) {
	if got := SplitList(" a, ,b ,"); strings.Join(got, "|") != "a|b" {
		t.Errorf("got %q", got)
	}
	if got := SplitList(""); got == nil || len(got) != 0 {
		t.Errorf("empty: %#v", got)
	}
}
