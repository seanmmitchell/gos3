package admin

import (
	"encoding/json"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/updates"
	"testing"
)

func TestUpdatesReadableWithoutConfigPermission(t *testing.T) {
	e := newEnv(t)
	user := e.addUser("release-reader", "readonly")
	e.api.d.Updates = updates.New("v0.2.2", "")
	e.api.d.Updates.Configure(config.UpdatesConfig{Enabled: true, NotifyFor: []string{"patch"}})
	e.do(&user, "GET", Prefix+"/config", nil).expectStatus(t, 403)
	var got updates.Status
	e.do(&user, "GET", Prefix+"/updates", nil).json(t, &got)
	if !got.Supported || !got.Enabled || got.InstalledVersion != "v0.2.2" {
		t.Fatalf("status %+v", got)
	}
	e.do(nil, "GET", Prefix+"/updates", nil).expectStatus(t, 401)
}

func TestOldConfigClientPreservesUpdates(t *testing.T) {
	e := newEnv(t)
	s := defaultSettings()
	s.Updates = config.UpdatesConfig{Enabled: true, NotifyFor: []string{"major"}}
	e.root("PUT", "/config", s).expectStatus(t, 200)
	// A pre-updates client sends no updates object.
	data, _ := json.Marshal(s)
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	delete(doc, "updates")
	e.root("PUT", "/config", doc).expectStatus(t, 200)
	if !e.settings.Get().Updates.Enabled || e.settings.Get().Updates.NotifyFor[0] != "major" {
		t.Fatal("old client reset updates")
	}
}
