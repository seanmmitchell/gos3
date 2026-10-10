package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/discovery"
)

// validSettings is a complete, valid Settings document.
func validSettings() Settings {
	return Settings{
		Name:       "gos3-test",
		Region:     "us-east-1",
		PublicURL:  "http://127.0.0.1:9000",
		Domains:    []string{"s3.local"},
		PublicInfo: true,
		UIRedirect: true,
		CORS: CORSConfig{
			AllowedOrigins: []string{"https://ui.example.com", "https://*.apps.example.com"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"*", "ETag", "x-amz-request-id"},
			MaxAgeSeconds:  600,
		},
		Discovery: discovery.DefaultConfig(),
		Logging:   LoggingConfig{Level: "info", Format: "text", AccessLog: true},
		Lifecycle: LifecycleSettings{Enabled: true, IntervalSeconds: 3600},
	}
}

func TestValidateSettings(t *testing.T) {
	if err := ValidateSettings(validSettings()); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Settings)
		field  string // FieldError.Field
	}{
		{"region", func(s *Settings) { s.Region = "US East" }, "region"},
		{"empty region", func(s *Settings) { s.Region = "" }, "region"},
		{"name control", func(s *Settings) { s.Name = "a\x00b" }, "name"},
		{"name long", func(s *Settings) { s.Name = strings.Repeat("x", 129) }, "name"},
		{"name utf8", func(s *Settings) { s.Name = "\xff" }, "name"},
		{"public url scheme", func(s *Settings) { s.PublicURL = "ftp://x" }, "publicUrl"},
		{"public url query", func(s *Settings) { s.PublicURL = "http://x?y=1" }, "publicUrl"},
		{"public url creds", func(s *Settings) { s.PublicURL = "http://u:p@x" }, "publicUrl"},
		{"domain", func(s *Settings) { s.Domains = []string{"s3.local", "bad domain"} }, "domains[1]"},
		{"domain port", func(s *Settings) { s.Domains = []string{"s3.local:9000"} }, "domains[0]"},
		{"domains too many", func(s *Settings) { s.Domains = make([]string, 257) }, "domains"},
		{"cors origin path", func(s *Settings) { s.CORS.AllowedOrigins = []string{"https://a.com/"} }, "cors.allowedOrigins[0]"},
		{"cors origin bare", func(s *Settings) { s.CORS.AllowedOrigins = []string{"a.com"} }, "cors.allowedOrigins[0]"},
		{"cors header", func(s *Settings) { s.CORS.AllowedHeaders = []string{"bad header"} }, "cors.allowedHeaders[0]"},
		{"cors expose pattern", func(s *Settings) { s.CORS.ExposeHeaders = []string{"x-amz-*"} }, "cors.exposeHeaders[0]"},
		{"cors expose bad", func(s *Settings) { s.CORS.ExposeHeaders = []string{"ETag", "a b"} }, "cors.exposeHeaders[1]"},
		{"cors max age low", func(s *Settings) { s.CORS.MaxAgeSeconds = -1 }, "cors.maxAgeSeconds"},
		{"cors max age high", func(s *Settings) { s.CORS.MaxAgeSeconds = MaxCORSMaxAge + 1 }, "cors.maxAgeSeconds"},
		{"beacon group not multicast", func(s *Settings) { s.Discovery.BeaconGroup = "10.0.0.1:7373" }, "discovery.beaconGroup"},
		{"beacon group ipv6", func(s *Settings) { s.Discovery.BeaconGroup = "[ff02::1]:7373" }, "discovery.beaconGroup"},
		{"beacon group no port", func(s *Settings) { s.Discovery.BeaconGroup = "239.255.73.83" }, "discovery.beaconGroup"},
		{"beacon group bad port", func(s *Settings) { s.Discovery.BeaconGroup = "239.255.73.83:0" }, "discovery.beaconGroup"},
		{"beacon group empty", func(s *Settings) { s.Discovery.BeaconGroup = "" }, "discovery.beaconGroup"},
		{"beacon interval zero", func(s *Settings) { s.Discovery.BeaconInterval = 0 }, "discovery.beaconInterval"},
		{"beacon interval high", func(s *Settings) { s.Discovery.BeaconInterval = 2 * time.Hour }, "discovery.beaconInterval"},
		{"peer ttl", func(s *Settings) { s.Discovery.PeerTTL = -time.Second }, "discovery.peerTtl"},
		{"peer ttl zero", func(s *Settings) { s.Discovery.PeerTTL = 0 }, "discovery.peerTtl"},
		{"seed", func(s *Settings) { s.Discovery.Seeds = []string{"not a url"} }, "discovery.seeds[0]"},
		{"seed path", func(s *Settings) { s.Discovery.Seeds = []string{"http://h:9000/x"} }, "discovery.seeds[0]"},
		{"seed scheme", func(s *Settings) { s.Discovery.Seeds = []string{"ftp://h"} }, "discovery.seeds[0]"},
		{"dns srv", func(s *Settings) { s.Discovery.DNSSRV = []string{"-bad.example"} }, "discovery.dnsSrv[0]"},
		{"dns srv too many", func(s *Settings) { s.Discovery.DNSSRV = make([]string, discovery.MaxDNSSRV+1) }, "discovery.dnsSrv"},
		{"max scan hosts low", func(s *Settings) { s.Discovery.MaxScanHosts = 0 }, "discovery.maxScanHosts"},
		{"max scan hosts high", func(s *Settings) { s.Discovery.MaxScanHosts = 65537 }, "discovery.maxScanHosts"},
		{"log level", func(s *Settings) { s.Logging.Level = "verbose" }, "logging.level"},
		{"log level case", func(s *Settings) { s.Logging.Level = "INFO" }, "logging.level"},
		{"log format", func(s *Settings) { s.Logging.Format = "xml" }, "logging.format"},
		{"lifecycle interval low", func(s *Settings) { s.Lifecycle.IntervalSeconds = 59 }, "lifecycle.intervalSeconds"},
		{"lifecycle interval high", func(s *Settings) { s.Lifecycle.IntervalSeconds = MaxLifecycleSeconds + 1 }, "lifecycle.intervalSeconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSettings()
			tt.mutate(&s)
			err := ValidateSettings(s)
			var fe *FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v, want *FieldError", err)
			}
			if fe.Field != tt.field {
				t.Errorf("field = %q, want %q (%v)", fe.Field, tt.field, err)
			}
			if !strings.HasPrefix(err.Error(), tt.field+": ") || fe.Msg == "" {
				t.Errorf("message %q", err)
			}
		})
	}
	t.Run("edge values", func(t *testing.T) {
		s := validSettings()
		s.Name = ""
		s.PublicURL = ""
		s.Domains = nil
		s.CORS = CORSConfig{}
		s.Discovery.MaxScanHosts = MaxScanHosts
		s.Discovery.Seeds = []string{"https://peer.example.com:9443", "nas.local:9000", "10.0.0.5"}
		s.Discovery.DNSSRV = []string{"example.com."}
		s.Discovery.BeaconGroup = "239.1.2.3:7000"
		s.Discovery.BeaconInterval = time.Hour
		s.Lifecycle.IntervalSeconds = MinLifecycleSeconds
		if err := ValidateSettings(s); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("agrees with discovery", func(t *testing.T) {
		// Anything ValidateSettings accepts must be accepted by discovery.
		s := validSettings()
		s.Discovery.Seeds = []string{"nas.local:9000", "https://[::1]:9000"}
		if err := ValidateSettings(s); err != nil {
			t.Fatal(err)
		}
		if err := s.Discovery.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestValidateCORSOrigin(t *testing.T) {
	valid := []string{"*", "https://a.com", "http://localhost:3000", "https://*.example.com", "https://*.example.com:8443",
		"http://10.0.0.1", "http://[::1]:8080", "http://[fe80::1]", "app://localhost", "chrome-extension://abcdef"}
	invalid := []string{"", "a.com", "https://", "https://a.com/", "https://a.com/x", "https://a.com?x", "https://u@a.com",
		"https://a.com:", "https://a.com:0", "https://a.com:+80", "https://a.com:x", "https://*.com.*", "https://a.*.com",
		"https://*", "http://[::1", "http://[::1]x", "http://[1.2.3.4]", "1ttp://a.com", "https://a b.com"}
	for _, o := range valid {
		if err := ValidateCORSOrigin(o); err != nil {
			t.Errorf("ValidateCORSOrigin(%q) = %v", o, err)
		}
	}
	for _, o := range invalid {
		if err := ValidateCORSOrigin(o); err == nil {
			t.Errorf("ValidateCORSOrigin(%q) accepted", o)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		in        string
		allowPath bool
		ok        bool
	}{
		{"http://h:9000", false, true},
		{"https://h", false, true},
		{"https://h/", false, true},
		{"http://10.0.0.5:9000", false, true},
		{"http://[::1]:9000", false, true},
		{"http://h/base", true, true},
		{"http://h/base", false, false},
		{"h:9000", false, false},
		{"http://", false, false},
		{"http://h:0", false, false},
		{"http://h:", false, false},
		{"http://h#f", false, false},
		{"http://h?", false, false},
		{"http://bad_host!", false, false},
		{"http://" + strings.Repeat("a", MaxURLLen), false, false},
		{"%zz", false, false},
	}
	for _, tt := range tests {
		if err := ValidateBaseURL(tt.in, tt.allowPath); (err == nil) != tt.ok {
			t.Errorf("ValidateBaseURL(%q, %v) = %v", tt.in, tt.allowPath, err)
		}
	}
}

func TestValidHostname(t *testing.T) {
	for _, h := range []string{"a", "s3.local", "my-host.example.com", "x_y.local", strings.Repeat("a", 63) + ".com"} {
		if !ValidHostname(h) {
			t.Errorf("ValidHostname(%q) = false", h)
		}
	}
	for _, h := range []string{"", ".", "a..b", "-a.com", "a-.com", "a b", strings.Repeat("a", 64) + ".com",
		strings.Repeat("a.", 127) + "aa", "a:80", "a/b"} {
		if ValidHostname(h) {
			t.Errorf("ValidHostname(%q) = true", h)
		}
	}
}

func TestSettingsFrom(t *testing.T) {
	c := Default()
	c.Server.Name = "n1"
	c.Server.Domains = []string{"s3.example.com"}
	c.UI.RedirectRoot = false
	c.Metrics.Public = true
	c.Lifecycle.Interval = 90 * time.Minute
	c.Discovery = discovery.Config{Enabled: true, BeaconInterval: 30 * time.Second}
	s := SettingsFrom(c)
	if s.Name != "n1" || s.UIRedirect || !s.MetricsPublic || s.Lifecycle.IntervalSeconds != 5400 ||
		s.Discovery.BeaconGroup != discovery.DefaultBeaconGroup || s.Discovery.PeerTTL != 90*time.Second ||
		s.Discovery.MaxScanHosts != discovery.DefaultMaxScanHosts || s.Discovery.Seeds == nil {
		t.Errorf("settings = %+v", s)
	}
	if err := ValidateSettings(s); err != nil {
		t.Fatal(err)
	}
	// The result does not alias the config.
	s.Domains[0] = "changed"
	if c.Server.Domains[0] != "s3.example.com" {
		t.Error("SettingsFrom aliases Domains")
	}
}
