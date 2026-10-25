//go:build noui

package server

import (
	"strings"
	"testing"
)

func TestHeadlessBuild(t *testing.T) {
	cfg := testConfig()
	srv := start(t, cfg)
	if srv.UIEnabled() || srv.UIBuilt() || srv.Info().Endpoints.UI != "" {
		t.Fatal("headless binary enabled or advertised UI")
	}
	if srv.updates != nil {
		t.Fatal("headless binary created a release checker")
	}
	cfg.UI.Enabled = true
	if s, err := New(cfg, nil); err == nil {
		s.Close()
		t.Fatal("enabled UI without assets")
	} else if !strings.Contains(err.Error(), "without UI assets") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
