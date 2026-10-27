package config

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestUpdateSettingsPrecedence(t *testing.T) {
	c, err := load("", env(map[string]string{"GOS3_UPDATES_ENABLED": "true", "GOS3_UPDATES_NOTIFY_FOR": "patch,minor"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Updates.Enabled || !slices.Equal(c.Updates.NotifyFor, []string{"patch", "minor"}) {
		t.Fatalf("env %+v", c.Updates)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := LoadStore(SettingsFrom(c), path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(func(s *Settings) error {
		s.Updates.Enabled = false
		s.Updates.NotifyFor = []string{"major"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err = LoadStore(SettingsFrom(c), path)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if got.Updates.Enabled || !slices.Equal(got.Updates.NotifyFor, []string{"major"}) {
		t.Fatalf("saved settings did not override env: %+v", got.Updates)
	}
	got.Updates.NotifyFor[0] = "patch"
	if s.Get().Updates.NotifyFor[0] != "major" {
		t.Fatal("Get leaked updates slice")
	}
	for _, types := range [][]string{{"patch", "patch"}, {"security"}, {"MAJOR"}} {
		_, err := s.Update(func(s *Settings) error { s.Updates.NotifyFor = types; return nil })
		if err == nil {
			t.Fatalf("accepted %v", types)
		}
	}
}
