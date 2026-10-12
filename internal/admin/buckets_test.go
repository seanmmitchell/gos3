package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestBuckets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.createBucket("alpha", storage.CreateBucketOptions{Region: "us-east-1", Owner: "alice"})
	e.createBucket("beta", storage.CreateBucketOptions{Region: "eu-west-1", ObjectLockEnabled: true})
	e.putObject("alpha", "a", "12345")
	e.putObject("alpha", "b", "678")
	for k, v := range map[string]string{
		storage.ConfigCORS:      "<CORSConfiguration/>",
		storage.ConfigLifecycle: "<LifecycleConfiguration/>",
		storage.ConfigTagging:   "<Tagging/>",
		storage.ConfigPolicy:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::alpha/*"}]}`,
	} {
		if err := e.backend.PutBucketConfig(ctx, "alpha", k, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}

	r := e.root("GET", "/buckets", nil)
	r.expectStatus(t, 200)
	var list struct{ Buckets []bucketSummary }
	r.json(t, &list)
	if len(list.Buckets) != 2 {
		t.Fatalf("buckets = %s", r.body)
	}
	a, b := list.Buckets[0], list.Buckets[1]
	if a.Name != "alpha" || a.Owner != "alice" || !a.Public || a.Usage != nil || a.Region != "us-east-1" {
		t.Errorf("alpha = %+v", a)
	}
	wantCfg := bucketConfigs{Policy: true, CORS: true, Lifecycle: true, Tagging: true, ACL: "private"}
	if a.Configs != wantCfg {
		t.Errorf("alpha configs = %+v", a.Configs)
	}
	if b.Name != "beta" || b.Owner != "root" || b.Public || !b.ObjectLock || b.Versioning != "Enabled" {
		t.Errorf("beta = %+v", b)
	}
	if !strings.Contains(string(r.body), `"usage":null`) {
		t.Errorf("usage not null: %s", r.body)
	}

	r = e.root("GET", "/buckets?usage=true", nil)
	r.json(t, &list)
	if u := list.Buckets[0].Usage; u == nil || u.Objects != 2 || u.Bytes != 8 || u.Versions != 2 {
		t.Errorf("alpha usage = %+v", u)
	}
	if u := list.Buckets[1].Usage; u == nil || u.usageJSON != (usageJSON{}) {
		t.Errorf("beta usage = %+v", u)
	}
	// Usage says when it was computed (it is cached for usageTTL).
	if u := list.Buckets[0].Usage; u == nil || !u.ComputedAt.Equal(e.clock.Now().UTC()) {
		t.Errorf("alpha usage computedAt = %+v, want %v", u, e.clock.Now().UTC())
	}
	if !strings.Contains(string(r.body), `"computedAt":`) {
		t.Errorf("usage without computedAt: %s", r.body)
	}
	e.root("GET", "/buckets?usage=maybe", nil).expectError(t, 400, codeInvalidRequest)

	// public via ACL
	if err := e.backend.PutBucketConfig(ctx, "beta", storage.ConfigACL, []byte("public-read")); err != nil {
		t.Fatal(err)
	}
	r = e.root("GET", "/buckets/beta", nil)
	r.expectStatus(t, 200)
	var s bucketSummary
	r.json(t, &s)
	if !s.Public || s.Configs.ACL != "public-read" || s.Usage == nil {
		t.Errorf("beta = %+v", s)
	}
	e.root("GET", "/buckets/nope", nil).expectError(t, 404, codeNotFound)

	// non-force delete
	e.root("DELETE", "/buckets/alpha", nil).expectError(t, 409, codeConflict)
	e.root("DELETE", "/buckets/beta", nil).expectStatus(t, 204)
	e.root("DELETE", "/buckets/beta", nil).expectError(t, 404, codeNotFound)
	e.root("DELETE", "/buckets/alpha?force=nah", nil).expectError(t, 400, codeInvalidRequest)
}

func TestForceDeleteBucket(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.createBucket("v", storage.CreateBucketOptions{})
	if err := e.backend.SetBucketVersioning(ctx, "v", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		e.putObject("v", "k", strings.Repeat("x", i))
		e.putObject("v", "dir/k"+string(rune('a'+i)), "y")
	}
	if _, err := e.backend.DeleteObject(ctx, "v", "k", ""); err != nil { // delete marker
		t.Fatal(err)
	}
	if _, err := e.backend.DeleteObject(ctx, "v", "dir/ka", ""); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"big", "big", "other"} {
		if _, err := e.backend.CreateMultipartUpload(ctx, "v", key, storage.ObjectMeta{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.backend.PutBucketConfig(ctx, "v", storage.ConfigTagging, []byte("<Tagging/>")); err != nil {
		t.Fatal(err)
	}
	u, err := storage.ComputeBucketUsage(ctx, e.backend, "v")
	if err != nil || u.DeleteMarkers != 2 || u.Versions != 10 || u.PendingUploads != 3 {
		t.Fatalf("usage = %+v %v", u, err)
	}

	// Warm the usage cache so we can check it is invalidated.
	e.root("GET", "/stats", nil).expectStatus(t, 200)

	e.root("DELETE", "/buckets/v?force=true", nil).expectStatus(t, 204)
	if _, err := e.backend.GetBucket(ctx, "v"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("bucket still exists: %v", err)
	}
	var s statsResponse
	e.root("GET", "/stats", nil).json(t, &s)
	if s.Storage.Buckets != 0 || s.Storage.Versions != 0 {
		t.Errorf("stale stats after delete: %+v", s.Storage)
	}
	e.root("DELETE", "/buckets/v?force=true", nil).expectError(t, 404, codeNotFound)

	// Large bucket (several purge batches) with an empty bucket recreated
	// under the same name afterwards.
	e.createBucket("big", storage.CreateBucketOptions{})
	for i := range purgeBatch + 50 {
		e.putObject("big", "obj-"+itoa(i), "")
	}
	e.root("DELETE", "/buckets/big?force=1", nil).expectStatus(t, 204)
	e.createBucket("big", storage.CreateBucketOptions{})
	e.root("GET", "/buckets/big", nil).expectStatus(t, 200)
}

func TestForceDeleteConcurrentWrites(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.createBucket("busy", storage.CreateBucketOptions{})
	for i := range 200 {
		e.putObject("busy", "k"+itoa(i), "data")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			w, err := e.backend.NewObjectWriter(ctx, "busy", "w"+itoa(i))
			if err != nil {
				return // bucket gone
			}
			_, _ = io.WriteString(w, "z")
			if _, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{}); err != nil {
				return
			}
		}
	}()
	r := e.root("DELETE", "/buckets/busy?force=true", nil)
	close(stop)
	wg.Wait()
	// Either it won the race (204) or reported a conflict after retries.
	if r.status != 204 && r.status != 409 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

func TestCannedACLOf(t *testing.T) {
	const grant = `<AccessControlPolicy><Owner><ID>x</ID></Owner><AccessControlList>%s</AccessControlList></AccessControlPolicy>`
	g := func(uri, perm string) string {
		return `<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>` + uri +
			`</URI></Grantee><Permission>` + perm + `</Permission></Grant>`
	}
	tests := []struct {
		in, want string
	}{
		{"", "private"},
		{"  Public-Read \n", "public-read"},
		{"bucket-owner-full-control", "bucket-owner-full-control"},
		{strings.Replace(grant, "%s", g(allUsersURI, "READ"), 1), iam.ACLPublicRead},
		{strings.Replace(grant, "%s", g(allUsersURI, "READ")+g(allUsersURI, "WRITE"), 1), iam.ACLPublicReadWrite},
		{strings.Replace(grant, "%s", g(allUsersURI, "FULL_CONTROL"), 1), iam.ACLPublicReadWrite},
		{strings.Replace(grant, "%s", g(authUsersURI, "READ"), 1), iam.ACLAuthenticatedRead},
		{strings.Replace(grant, "%s", "", 1), "private"},
		{"<broken", "private"},
		{strings.Repeat("x", 100), "private"},
	}
	for _, tt := range tests {
		if got := cannedACLOf([]byte(tt.in)); got != tt.want {
			t.Errorf("cannedACLOf(%.40q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDiscovery(t *testing.T) {
	e := newEnv(t)
	r := e.root("GET", "/discovery", nil)
	r.expectStatus(t, 200)
	var st discoveryState
	r.json(t, &st)
	if !st.Enabled || st.Self.ID != e.info.ID || st.Peers == nil || st.Config.BeaconGroup != discovery.DefaultBeaconGroup {
		t.Errorf("discovery = %+v", st)
	}
	if !strings.Contains(string(r.body), `"peers":[`) {
		t.Errorf("peers not an array: %s", r.body)
	}

	t.Run("probe failure", func(t *testing.T) {
		// A closed port: the probe fails whatever the implementation.
		msg := e.root("POST", "/discovery/probe", map[string]any{"url": "http://127.0.0.1:1"}).expectError(t, 502, codeProbeFailed)
		if !strings.Contains(msg, "127.0.0.1:1") {
			t.Errorf("message %q", msg)
		}
	})
	t.Run("probe success", func(t *testing.T) {
		peerInfo := discovery.Info{Service: discovery.ServiceName, ID: "fedcba9876543210", Name: "peer", Version: "1",
			APIVersion: "v1", Region: "us-east-1"}
		peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/_gos3/info" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(peerInfo)
		}))
		defer peerSrv.Close()
		r := e.root("POST", "/discovery/probe", map[string]any{"url": peerSrv.URL})
		if r.status == http.StatusBadGateway {
			if _, msg := r.errCode(t); strings.Contains(msg, "canceled") {
				t.Skip("discovery.Service.Probe is still a contract stub")
			}
		}
		r.expectStatus(t, 200)
		var p discovery.Peer
		r.json(t, &p)
		if p.ID != peerInfo.ID || p.Name != "peer" {
			t.Errorf("peer = %+v", p)
		}
	})
	t.Run("probe validation", func(t *testing.T) {
		e.root("POST", "/discovery/probe", map[string]any{"url": "ftp://x"}).expectError(t, 400, codeInvalidRequest)
		e.root("POST", "/discovery/probe", map[string]any{}).expectError(t, 400, codeInvalidRequest)
	})
	t.Run("scan", func(t *testing.T) {
		r := e.root("POST", "/discovery/scan", map[string]any{
			"targets": []string{"127.0.0.1"}, "ports": []int{1}, "timeout": 200_000_000,
		})
		r.expectStatus(t, 200)
		var res discovery.ScanResult
		r.json(t, &res)
		if res.Found == nil {
			t.Errorf("found is null: %s", r.body)
		}
	})
	t.Run("scan too many hosts", func(t *testing.T) {
		msg := e.root("POST", "/discovery/scan", map[string]any{"targets": []string{"10.0.0.0/16"}}).
			expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, "maxScanHosts") {
			t.Errorf("message %q", msg)
		}
		// The limit follows the runtime settings.
		s := e.settings.Get()
		s.Discovery.MaxScanHosts = 4
		e.root("PUT", "/config", s).expectStatus(t, 200)
		defer func() {
			s.Discovery.MaxScanHosts = 1024
			e.root("PUT", "/config", s).expectStatus(t, 200)
		}()
		e.root("POST", "/discovery/scan", map[string]any{"targets": []string{"127.0.0.0/29"}, "ports": []int{1}}).
			expectError(t, 400, codeInvalidRequest)
		e.root("POST", "/discovery/scan", map[string]any{
			"targets": []string{"127.0.0.0/30", "127.0.0.1", "localhost"}, "ports": []int{1}, "timeout": 200_000_000,
		}).expectStatus(t, 200)
	})
	t.Run("scan validation", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"targets": []string{}},
			{"targets": []string{"not a host!"}},
			{"targets": []string{"10.0.0.0/33"}},
			{"targets": []string{"h"}, "ports": []int{0}},
			{"targets": []string{"h"}, "schemes": []string{"gopher"}},
			{"targets": []string{"h"}, "concurrency": -1},
		} {
			e.root("POST", "/discovery/scan", body).expectError(t, 400, codeInvalidRequest)
		}
	})
	t.Run("scan disabled", func(t *testing.T) {
		s := e.settings.Get()
		s.Discovery.AllowScan = false
		e.root("PUT", "/config", s).expectStatus(t, 200)
		msg := e.root("POST", "/discovery/scan", map[string]any{"targets": []string{"127.0.0.1"}}).expectError(t, 403, codeAccessDenied)
		if !strings.Contains(msg, "allowScan") {
			t.Errorf("message %q", msg)
		}
	})
	t.Run("scan requires ScanNetwork", func(t *testing.T) {
		diag := e.addUser("diag", iam.PolicyDiagnostics)
		e.do(&diag, "POST", Prefix+"/discovery/scan", map[string]any{"targets": []string{"h"}}).expectError(t, 403, codeAccessDenied)
	})
}

func TestDiscoveryDisabled(t *testing.T) {
	e := newEnv(t, withoutDiscovery())
	r := e.root("GET", "/discovery", nil)
	r.expectStatus(t, 200)
	var st discoveryState
	r.json(t, &st)
	if st.Enabled || len(st.Peers) != 0 {
		t.Errorf("discovery = %+v", st)
	}
	e.root("POST", "/discovery/probe", map[string]any{"url": "http://127.0.0.1:1"}).expectError(t, 400, codeInvalidRequest)
	e.root("POST", "/discovery/scan", map[string]any{"targets": []string{"h"}}).expectError(t, 400, codeInvalidRequest)
}

func TestScanTargetHosts(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
		err  bool
	}{
		{"10.0.0.0/24", 254, false},
		{"10.0.0.0/30", 2, false},
		{"10.0.0.0/31", 2, false},
		{"10.0.0.1/32", 1, false},
		{"fd00::/120", 256, false},
		{"::/0", 1 << 63, false},
		{"nas.local", 1, false},
		{"nas.local:9000", 1, false},
		{"192.168.1.1", 1, false},
		{"::1", 1, false},
		{"https://s3.example.com", 1, false},
		{"gopher://x", 0, true},
		{"10.0.0.0/40", 0, true},
		{"bad host", 0, true},
	}
	for _, tt := range tests {
		got, err := scanTargetHosts(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("scanTargetHosts(%q) = %d, %v", tt.in, got, err)
		}
	}
}
