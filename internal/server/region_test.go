package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
)

// TestStrictRegionFollowsSettings: with auth.strict_region, a region change
// through the settings (admin PUT /config) applies to signature checks
// right away (regression: the startup region was enforced until restart).
func TestStrictRegionFollowsSettings(t *testing.T) {
	cfg := testConfig()
	cfg.Auth.StrictRegion = true
	srv := start(t, cfg)
	get := func(region string) int {
		req, err := http.NewRequest(http.MethodGet, srv.url+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.SignRequest(req, rootCreds, region, "s3", "", time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get("us-east-1"); got != http.StatusOK {
		t.Fatalf("startup region: %d", got)
	}
	if got := get("eu-central-1"); got != http.StatusBadRequest {
		t.Fatalf("other region before the change: %d", got)
	}
	if _, err := srv.Settings().Update(func(s *config.Settings) error { s.Region = "eu-central-1"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := get("eu-central-1"); got != http.StatusOK {
		t.Errorf("new region after the change: %d", got)
	}
	if got := get("us-east-1"); got != http.StatusBadRequest {
		t.Errorf("old region after the change: %d", got)
	}
}
