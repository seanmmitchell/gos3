package discovery

import (
	"context"
	"testing"
)

// enabledConfig is explicit opt-in for tests exercising discovery mechanisms.
func enabledConfig() Config {
	c := DefaultConfig()
	c.Enabled, c.Advertise, c.MDNS, c.Beacon, c.AllowScan = true, true, true, true, true
	return c
}

func TestDiscoveryDefaultsOff(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Enabled || cfg.Advertise || cfg.MDNS || cfg.Beacon || cfg.AllowScan || len(cfg.Seeds) != 0 || len(cfg.DNSSRV) != 0 {
		t.Fatalf("discovery defaults must all be off: %+v", cfg)
	}
	s := New(cfg, nil, testLogger(t))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if len(mechSnapshot(s)) != 0 {
		t.Fatal("default configuration started discovery mechanisms")
	}
}
