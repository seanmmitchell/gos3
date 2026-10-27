package updates

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
)

func TestVersionSelection(t *testing.T) {
	for _, tc := range []struct {
		installed   string
		types, tags []string
		want, kind  string
	}{
		{"v0.2.2", []string{"patch", "minor", "major"}, []string{"v0.2.10", "v0.3.0", "v1.0.0", "v2.0.0-rc.1"}, "v1.0.0", "major"},
		{"0.2.2+build.2", []string{"patch"}, []string{"v1.0.0", "v0.3.0", "v0.2.10", "v0.2.3"}, "v0.2.10", "patch"},
		{"v0.2.2", []string{"minor"}, []string{"v1.0.0", "v0.3.0"}, "v0.3.0", "minor"},
		{"1.2.3", []string{"patch"}, []string{"v1.2.3+new", "v1.2.2", "v1.02.4", "v1.2.4+", "v1.2.4.1", "v1.2.4-rc.1"}, "", ""},
		{"dev", []string{"major"}, []string{"v9.0.0"}, "", ""},
		{"unknown", []string{"major"}, []string{"v9.0.0"}, "", ""},
		{"v0.2.2-3-g123abc", []string{"minor"}, []string{"v0.3.0"}, "", ""},
		{"1.2.3", nil, []string{"v1.2.4"}, "", ""},
		{"1.2.3", []string{"major"}, []string{"v999999999999999999999.0.0"}, "v999999999999999999999.0.0", "major"},
	} {
		t.Run(tc.installed+tc.want+tc.kind, func(t *testing.T) {
			got := newest(tc.installed, tc.tags, tc.types)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("unexpected %+v", got)
				}
				return
			}
			if got == nil || got.Version != tc.want || got.Type != tc.kind {
				t.Fatalf("got %+v, want %s %s", got, tc.want, tc.kind)
			}
		})
	}
}

func TestDailyCacheFailuresAndRestart(t *testing.T) {
	var calls atomic.Int32
	body := `[{"tag_name":"v0.2.3","published_at":"2026-01-01T00:00:00Z"},{"tag_name":"v0.3.0","published_at":"2026-01-01T00:00:00Z"},{"tag_name":"v8.0.0","draft":true,"published_at":"2026-01-01T00:00:00Z"},{"tag_name":"v9.0.0","prerelease":true,"published_at":"2026-01-01T00:00:00Z"},{"tag_name":"v10.0.0"}]`
	code := http.StatusOK
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected GitHub credentials")
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer remote.Close()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "updates.json")
	s := New("v0.2.2", path)
	s.endpoint, s.now = remote.URL, func() time.Time { return now }
	s.check(context.Background())
	if calls.Load() != 0 {
		t.Fatal("disabled checker contacted GitHub")
	}
	s.Configure(config.UpdatesConfig{Enabled: true, NotifyFor: []string{"patch"}})
	s.check(context.Background())
	if got := s.Status(); got.Available == nil || got.Available.Version != "v0.2.3" || got.Error != "" {
		t.Fatalf("status %+v", got)
	}
	s.Configure(config.UpdatesConfig{Enabled: true, NotifyFor: []string{"minor"}})
	s.check(context.Background())
	if s.Status().Available.Version != "v0.3.0" || calls.Load() != 1 {
		t.Fatal("settings did not reselect cache without checking")
	}
	s = New("v0.2.2", path)
	s.endpoint, s.now = remote.URL, func() time.Time { return now }
	s.Configure(config.UpdatesConfig{Enabled: true, NotifyFor: []string{"patch"}})
	s.check(context.Background())
	if calls.Load() != 1 || s.Status().Available == nil {
		t.Fatal("restart lost cache or daily budget")
	}
	for _, failure := range []struct {
		code int
		body string
	}{{429, ""}, {200, "oops"}, {200, "null"}, {200, "{}"}} {
		now = now.Add(interval)
		code, body = failure.code, failure.body
		s.check(context.Background())
		got := s.Status()
		if got.Error == "" || got.Available == nil || got.LastSuccess == nil || !got.LastCheck.Equal(now) {
			t.Fatalf("failure lost cached result: %+v", got)
		}
		before := calls.Load()
		s.check(context.Background())
		if calls.Load() != before {
			t.Fatal("failed checks exceeded daily limit")
		}
	}
	s.Configure(config.UpdatesConfig{})
	if s.Status().Available != nil {
		t.Fatal("disabled checker still advertises")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTimeoutOfflineAndDisableInFlight(t *testing.T) {
	for _, offline := range []bool{false, true} {
		s := New("1.2.3", "")
		s.Configure(config.UpdatesConfig{Enabled: true})
		s.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if offline {
				return nil, errors.New("network offline")
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		s.check(ctx)
		cancel()
		if s.Status().Error == "" {
			t.Fatal("failure not reported")
		}
	}
	s := New("1.2.3", "")
	s.Configure(config.UpdatesConfig{Enabled: true})
	started := make(chan struct{})
	s.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	done := make(chan struct{})
	go func() { s.check(context.Background()); close(done) }()
	<-started
	s.Configure(config.UpdatesConfig{})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disable did not cancel outbound check")
	}
}

func TestReleasePagination(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			list := make([]map[string]any, 100)
			for i := range list {
				list[i] = map[string]any{"tag_name": "v2.0.0", "published_at": "2026-01-01T00:00:00Z"}
			}
			_ = json.NewEncoder(w).Encode(list)
		} else {
			_, _ = w.Write([]byte(`[{"tag_name":"v1.2.4","published_at":"2026-01-01T00:00:00Z"}]`))
		}
	}))
	defer remote.Close()
	s := New("1.2.3", "")
	s.endpoint = remote.URL
	s.Configure(config.UpdatesConfig{Enabled: true, NotifyFor: []string{"patch"}})
	s.check(context.Background())
	if got := s.Status(); got.Available == nil || got.Available.Version != "v1.2.4" {
		t.Fatalf("older eligible release not found: %+v", got)
	}
}

func TestWorkerEnablesLiveAndStops(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`[{"tag_name":"v1.2.4","published_at":"2026-01-01T00:00:00Z"}]`))
	}))
	defer remote.Close()
	s := New("1.2.3", "")
	s.endpoint = remote.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	s.Configure(config.UpdatesConfig{Enabled: true, NotifyFor: []string{"patch"}})
	deadline := time.After(time.Second)
	for s.Status().LastSuccess == nil {
		select {
		case <-deadline:
			t.Fatal("live enable did not check")
		case <-time.After(time.Millisecond):
		}
	}
	if calls.Load() != 1 {
		t.Fatal("enable checked more than once")
	}
	s.Configure(config.UpdatesConfig{})
	if s.Status().Available != nil {
		t.Fatal("disable did not immediately hide notice")
	}
	s.Configure(config.UpdatesConfig{Enabled: true, NotifyFor: []string{"patch"}})
	if s.Status().Available == nil {
		t.Fatal("re-enable lost cache")
	}
	if calls.Load() != 1 {
		t.Fatal("re-enable bypassed daily budget")
	}
}
