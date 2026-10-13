package config

import (
	"reflect"
	"testing"
	"time"
)

// leafFields counts the non-struct fields of t, recursively.
func leafFields(t reflect.Type) int {
	n := 0
	for i := range t.NumField() {
		f := t.Field(i).Type
		if f.Kind() == reflect.Struct && f != reflect.TypeFor[time.Time]() {
			n += leafFields(f)
		} else {
			n++
		}
	}
	return n
}

func TestSettingFieldsComplete(t *testing.T) {
	if got, want := len(settingFields), leafFields(reflect.TypeFor[Settings]()); got != want {
		t.Fatalf("settingFields lists %d settings, Settings has %d", got, want)
	}
}

// TestPersistedOverrides checks the detection of static settings that
// settings.json overrides (previously ignored without any warning).
func TestPersistedOverrides(t *testing.T) {
	cfg := Default()
	cfg.Discovery.Enabled = true                                 // explicit opt-in
	cfg.CORS.AllowedOrigins = []string{"https://ui.example.com"} // explicit (--cors-origin)
	cfg.Normalize()

	persisted := SettingsFrom(Default())
	persisted.Name = "renamed in the UI"
	persisted.Logging.Level = "debug"

	got := map[string]SettingOverride{}
	for _, o := range PersistedOverrides(cfg, persisted) {
		got[o.Key] = o
	}
	want := map[string]SettingOverride{
		"server.name":          {Key: "server.name", Configured: cfg.Server.Name, Persisted: "renamed in the UI", Explicit: false},
		"discovery.enabled":    {Key: "discovery.enabled", Configured: "true", Persisted: "false", Explicit: true},
		"cors.allowed_origins": {Key: "cors.allowed_origins", Configured: `["https://ui.example.com"]`, Persisted: `["*"]`, Explicit: true},
		"logging.level":        {Key: "logging.level", Configured: "info", Persisted: "debug", Explicit: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overrides:\n got %+v\nwant %+v", got, want)
	}
	if o := PersistedOverrides(cfg, SettingsFrom(cfg)); len(o) != 0 {
		t.Errorf("identical settings reported as overrides: %+v", o)
	}
}

func TestApplySettingsRoundTrip(t *testing.T) {
	s := validSettings()
	s.Name = "applied"
	s.Discovery.Enabled = false
	s.Discovery.PeerTTL = 5 * time.Minute
	s.Lifecycle.IntervalSeconds = 7200
	s.MetricsPublic = true
	s.UIRedirect = false
	cfg := Default()
	ApplySettings(&cfg, s)
	if got := SettingsFrom(cfg); !reflect.DeepEqual(got, normalizeSettings(s)) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, s)
	}
}
