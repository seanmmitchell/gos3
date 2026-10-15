package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
)

func TestRouting(t *testing.T) {
	cfg := testConfig()
	cfg.UI.Enabled = true // tests inject assets, including in noui builds
	cfg.Server.Domains = []string{"s3.test"}
	srv := start(t, cfg, WithUIFS(fakeUI()))
	u := srv.url

	// Path-style /_gos3/info is the public info document.
	r := do(t, "GET", u+"/_gos3/info", nil, nil, nil)
	if r.status != 200 || !strings.Contains(string(r.body), `"service":"gos3"`) {
		t.Fatalf("info: %d %s", r.status, r.body)
	}

	// Virtual-hosted-style requests always go to S3, even for /_gos3/ paths:
	// here an object key in a bucket that does not exist.
	for _, host := range []string{"photos.s3.test", "photos.S3.TEST:9000", "photos.s3.test."} {
		r = do(t, "GET", u+"/_gos3/info", nil, &rootCreds, map[string]string{"Host": host})
		if r.status != 404 || !strings.Contains(string(r.body), "<Code>NoSuchBucket</Code>") {
			t.Fatalf("vhost %s: %d %s", host, r.status, r.body)
		}
	}
	// The bare domain is path-style.
	r = do(t, "GET", u+"/_gos3/health", nil, nil, map[string]string{"Host": "s3.test"})
	if r.status != 200 || !strings.Contains(string(r.body), `"ok"`) {
		t.Fatalf("bare domain health: %d %s", r.status, r.body)
	}

	// Unknown gos3 endpoints are JSON 404s.
	for _, p := range []string{"/_gos3", "/_gos3/", "/_gos3/nope", "/_gos3/admin", "/_gos3/infox"} {
		r = do(t, "GET", u+p, nil, nil, nil)
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if r.status != 404 || r.header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %q %s", p, r.status, r.header.Get("Content-Type"), r.body)
		}
		r.json(t, &e)
		if e.Error.Code != "NotFound" {
			t.Fatalf("%s: %s", p, r.body)
		}
	}

	// Health and ready.
	for _, p := range []string{"/_gos3/health", "/_gos3/ready"} {
		r = do(t, "GET", u+p, nil, nil, nil)
		if r.status != 200 || strings.TrimSpace(string(r.body)) != `{"status":"ok"}` {
			t.Fatalf("%s: %d %s", p, r.status, r.body)
		}
		r = do(t, "POST", u+p, nil, nil, nil)
		if r.status != 405 {
			t.Fatalf("POST %s: %d", p, r.status)
		}
	}

	// Browser navigation to / goes to the UI; API calls to / are S3.
	r = do(t, "GET", u+"/", nil, nil, map[string]string{"Accept": "text/html,application/xhtml+xml"})
	if r.status != 302 || r.header.Get("Location") != "/_gos3/ui/" {
		t.Fatalf("browser root: %d %q", r.status, r.header.Get("Location"))
	}
	r = do(t, "GET", u+"/", nil, nil, nil)
	if r.status != 403 || !strings.Contains(string(r.body), "<Error>") {
		t.Fatalf("anonymous ListBuckets: %d %s", r.status, r.body)
	}
	r = do(t, "GET", u+"/", nil, &rootCreds, map[string]string{"Accept": "text/html"})
	if r.status != 200 || !strings.Contains(string(r.body), "ListAllMyBucketsResult") {
		t.Fatalf("signed ListBuckets with Accept html: %d %s", r.status, r.body)
	}
	r = do(t, "GET", u+"/?X-Amz-Credential=x", nil, nil, map[string]string{"Accept": "text/html"})
	if r.status == 302 {
		t.Fatal("presigned-looking request redirected")
	}
	// The browser lands on the vhost root as S3, never the UI.
	r = do(t, "GET", u+"/", nil, nil, map[string]string{"Accept": "text/html", "Host": "photos.s3.test"})
	if r.status == 302 {
		t.Fatal("virtual-host root redirected to the UI")
	}

	// UI: files, SPA fallback, 404 for missing assets.
	r = do(t, "GET", u+"/_gos3/ui/servers/x/buckets", nil, nil, nil)
	if r.status != 200 || !strings.Contains(string(r.body), "gos3 spa") || r.header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("ui fallback: %d %q %s", r.status, r.header.Get("Cache-Control"), r.body)
	}
	r = do(t, "GET", u+"/_gos3/ui/_nuxt/app.123abc.js", nil, nil, nil)
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "text/javascript") ||
		!strings.Contains(r.header.Get("Cache-Control"), "immutable") {
		t.Fatalf("ui asset: %d %v", r.status, r.header)
	}
	r = do(t, "GET", u+"/_gos3/ui/_nuxt/gone.js", nil, nil, nil)
	if r.status != 404 {
		t.Fatalf("missing asset: %d", r.status)
	}
	r = do(t, "GET", u+"/_gos3/ui", nil, nil, nil)
	if r.status != 301 || r.header.Get("Location") != "/_gos3/ui/" {
		t.Fatalf("ui without slash: %d %q", r.status, r.header.Get("Location"))
	}

	// Info advertises the UI.
	var info discovery.Info
	do(t, "GET", u+"/_gos3/info", nil, nil, nil).json(t, &info)
	if info.Endpoints.UI != u+"/_gos3/ui/" || !slices.Contains(info.Features, "ui") ||
		info.Endpoints.S3 != u || info.Endpoints.Admin != u+"/_gos3/admin/v1" || info.Name != "test-server" ||
		info.ID != srv.ServerID() || len(info.ID) != 16 {
		t.Fatalf("info = %+v", info)
	}

	// Turning the redirect off keeps / as S3.
	if _, err := srv.Settings().Update(func(s *config.Settings) error { s.UIRedirect = false; return nil }); err != nil {
		t.Fatal(err)
	}
	r = do(t, "GET", u+"/", nil, nil, map[string]string{"Accept": "text/html"})
	if r.status == 302 {
		t.Fatal("redirect still active with uiRedirect=false")
	}
}

func TestUIDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.UI.Enabled = false
	srv := start(t, cfg, WithUIFS(fakeUI()))
	r := do(t, "GET", srv.url+"/_gos3/ui/", nil, nil, nil)
	if r.status != 404 || !strings.Contains(string(r.body), "disabled") {
		t.Fatalf("disabled ui: %d %s", r.status, r.body)
	}
	r = do(t, "GET", srv.url+"/", nil, nil, map[string]string{"Accept": "text/html"})
	if r.status == 302 {
		t.Fatal("redirect to a disabled UI")
	}
	if inf := srv.Info(); inf.Endpoints.UI != "" || slices.Contains(inf.Features, "ui") {
		t.Fatalf("info advertises a disabled UI: %+v", inf)
	}
}

func TestPlaceholderUINotAdvertised(t *testing.T) {
	cfg := testConfig()
	cfg.UI.Enabled = true
	srv := start(t, cfg, WithUIFS(fstestPlaceholder()))
	if !srv.UIEnabled() || srv.UIBuilt() {
		t.Fatalf("enabled=%v built=%v", srv.UIEnabled(), srv.UIBuilt())
	}
	if srv.Info().Endpoints.UI != "" {
		t.Fatal("placeholder UI advertised")
	}
	r := do(t, "GET", srv.url+"/_gos3/ui/anything", nil, nil, nil)
	if r.status != 200 || !strings.Contains(string(r.body), "make ui") {
		t.Fatalf("placeholder: %d %s", r.status, r.body)
	}
}

func fstestPlaceholder() fsMap {
	data, err := os.ReadFile("../webui/placeholder.html")
	if err != nil {
		panic(err)
	}
	return fsMap{"index.html": {Data: data}}
}

func TestInfoAuthModes(t *testing.T) {
	srv := start(t, testConfig())
	u := srv.url + "/_gos3/info"

	r := do(t, "GET", u, nil, nil, map[string]string{"Origin": "http://elsewhere.example"})
	if r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "*" || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("public info: %d %v", r.status, r.header)
	}
	r = do(t, "OPTIONS", u, nil, nil, map[string]string{
		"Origin": "http://elsewhere.example", "Access-Control-Request-Method": "GET",
		"Access-Control-Request-Headers": "authorization,x-amz-date",
	})
	if r.status != 204 || r.header.Get("Access-Control-Allow-Origin") != "*" ||
		r.header.Get("Access-Control-Allow-Headers") != "authorization,x-amz-date" {
		t.Fatalf("preflight: %d %v", r.status, r.header)
	}

	if _, err := srv.Settings().Update(func(s *config.Settings) error { s.PublicInfo = false; return nil }); err != nil {
		t.Fatal(err)
	}
	r = do(t, "GET", u, nil, nil, nil)
	if r.status != 401 || !strings.Contains(string(r.body), "Unauthenticated") || r.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("anonymous private info: %d %s", r.status, r.body)
	}
	bad := auth.Credentials{AccessKey: testAccessKey, SecretKey: "wrong-secret-key"}
	r = do(t, "GET", u, nil, &bad, nil)
	if r.status != 401 || !strings.Contains(string(r.body), "SignatureDoesNotMatch") {
		t.Fatalf("bad signature: %d %s", r.status, r.body)
	}
	r = do(t, "GET", u, nil, &rootCreds, nil)
	if r.status != 200 {
		t.Fatalf("signed private info: %d %s", r.status, r.body)
	}
}

func TestMetricsAuthAndAdminOp(t *testing.T) {
	srv := start(t, testConfig())
	ctx := context.Background()
	u := srv.url + "/_gos3/metrics"

	// Admin API mounted; its requests are recorded as op "Admin".
	r := do(t, "GET", srv.url+"/_gos3/admin/v1/whoami", nil, &rootCreds, nil)
	var who struct {
		IsRoot bool           `json:"isRoot"`
		Server discovery.Info `json:"server"`
	}
	if r.status != 200 {
		t.Fatalf("whoami: %d %s", r.status, r.body)
	}
	r.json(t, &who)
	if !who.IsRoot || who.Server.ID != srv.ServerID() {
		t.Fatalf("whoami = %s", r.body)
	}
	r = do(t, "PUT", srv.url+"/newbucket", nil, &rootCreds, nil)
	if r.status != 200 {
		t.Fatalf("CreateBucket: %d %s", r.status, r.body)
	}

	r = do(t, "GET", u, nil, nil, nil)
	if r.status != 401 {
		t.Fatalf("anonymous metrics: %d %s", r.status, r.body)
	}
	r = do(t, "GET", u, nil, &rootCreds, nil)
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("root metrics: %d %v %s", r.status, r.header, r.body)
	}
	body := string(r.body)
	for _, want := range []string{"gos3_buckets 1", `op="Admin"`, `op="CreateBucket"`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	found := false
	for _, op := range srv.Metrics().Snapshot().Ops {
		found = found || (op.Op == adminOp && op.Requests >= 1)
	}
	if !found {
		t.Error("admin request not in the metrics snapshot")
	}

	// IAM users: readonly may not read metrics, diagnostics may.
	mk := func(name, policy string) auth.Credentials {
		if _, err := srv.IAM().CreateUser(ctx, iam.UserSpec{Name: name, Policies: []string{policy}}); err != nil {
			t.Fatal(err)
		}
		k, err := srv.IAM().CreateAccessKey(ctx, name, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		return auth.Credentials{AccessKey: k.AccessKey, SecretKey: k.SecretKey}
	}
	reader, diag := mk("reader", "readonly"), mk("diag", "diagnostics")
	if r = do(t, "GET", u, nil, &reader, nil); r.status != 403 {
		t.Fatalf("readonly metrics: %d %s", r.status, r.body)
	}
	if r = do(t, "GET", u, nil, &diag, nil); r.status != 200 {
		t.Fatalf("diagnostics metrics: %d %s", r.status, r.body)
	}

	if _, err := srv.Settings().Update(func(s *config.Settings) error { s.MetricsPublic = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if r = do(t, "GET", u, nil, nil, nil); r.status != 200 {
		t.Fatalf("public metrics: %d %s", r.status, r.body)
	}
}

func TestMetricsDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.Enabled = false
	srv := start(t, cfg)
	if r := do(t, "GET", srv.url+"/_gos3/metrics", nil, &rootCreds, nil); r.status != 404 {
		t.Fatalf("disabled metrics: %d", r.status)
	}
}

func TestReadyFailsWhenBackendDown(t *testing.T) {
	srv, err := New(testConfig(), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Backend().Close(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rec := newRecorder()
	srv.Handler().ServeHTTP(rec, httpRequest("GET", "/_gos3/ready"))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"unavailable"`) {
		t.Fatalf("ready with closed backend: %d %s", rec.Code, rec.Body)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = dir
	cfg.Auth.RootAccessKey, cfg.Auth.RootSecretKey = "", ""

	srv := start(t, cfg)
	if srv.CredentialsSource() != config.CredentialsGenerated {
		t.Fatalf("source = %s", srv.CredentialsSource())
	}
	ak, sk := srv.RootCredentials()
	creds := auth.Credentials{AccessKey: ak, SecretKey: sk}
	id := srv.ServerID()

	// Change settings through the admin API.
	var st config.Settings
	do(t, "GET", srv.url+"/_gos3/admin/v1/config", nil, &creds, nil).json(t, &st)
	st.Name = "renamed"
	st.Lifecycle.IntervalSeconds = 120
	data, _ := json.Marshal(st)
	if r := do(t, "PUT", srv.url+"/_gos3/admin/v1/config", data, &creds, map[string]string{"Content-Type": "application/json"}); r.status != 200 {
		t.Fatalf("PUT config: %d %s", r.status, r.body)
	}
	if got := srv.lifecycle.Status().Interval; got != 2*time.Minute {
		t.Fatalf("lifecycle interval not applied: %s", got)
	}
	if r := do(t, "PUT", srv.url+"/keep", nil, &creds, nil); r.status != 200 {
		t.Fatalf("CreateBucket: %d %s", r.status, r.body)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"server-id", "settings.json", "root-credentials.json", "iam.json"} {
		st, err := os.Stat(filepath.Join(dir, ".gos3", f))
		if f == "iam.json" && err != nil { // only written on the first IAM mutation
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// Windows has no POSIX permission bits (ACLs apply instead).
		if f != "server-id" && runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", f, st.Mode().Perm())
		}
	}

	// Restart: same ID, stored credentials, persisted settings, data kept.
	srv2 := start(t, cfg)
	if srv2.CredentialsSource() != config.CredentialsStored || srv2.ServerID() != id {
		t.Fatalf("restart: source=%s id=%s (was %s)", srv2.CredentialsSource(), srv2.ServerID(), id)
	}
	if ak2, sk2 := srv2.RootCredentials(); ak2 != ak || sk2 != sk {
		t.Fatal("root credentials changed across restart")
	}
	if srv2.Info().Name != "renamed" || srv2.Settings().Get().Lifecycle.IntervalSeconds != 120 {
		t.Fatalf("settings not persisted: %+v", srv2.Settings().Get())
	}
	if srv2.lifecycle.Status().Interval != 2*time.Minute {
		t.Fatalf("persisted lifecycle interval not applied: %s", srv2.lifecycle.Status().Interval)
	}
	r := do(t, "GET", srv2.url+"/", nil, &creds, nil)
	if r.status != 200 || !strings.Contains(string(r.body), "<Name>keep</Name>") {
		t.Fatalf("bucket lost: %d %s", r.status, r.body)
	}
	if srv2.DataPath() != dir && !strings.HasSuffix(srv2.DataPath(), filepath.Base(dir)) {
		t.Errorf("DataPath = %s", srv2.DataPath())
	}

	// A second process on the same data directory is refused.
	if _, err := New(cfg, testLogger(t)); err == nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("second New on a locked data dir: %v", err)
	}
}

func TestSettingsApplyDiscoveryAndLogging(t *testing.T) {
	a := start(t, testConfig())

	logger, ctl := NewLogger(io.Discard, "info", "text")
	b, err := New(testConfig(), logger, WithLogControl(ctl))
	if err != nil {
		t.Fatal(err)
	}
	bs := serve(t, b)
	if len(bs.Discovery().Peers()) != 0 {
		t.Fatal("peers with discovery disabled")
	}

	_, err = b.Settings().Update(func(s *config.Settings) error {
		s.Discovery.Enabled = true
		s.Discovery.MDNS = false
		s.Discovery.Beacon = false
		s.Discovery.Seeds = []string{a.url}
		s.Logging.Level = "debug"
		s.Logging.Format = "json"
		s.Lifecycle.Enabled = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Discovery().Config().Enabled || !slices.Contains(b.Discovery().Config().Seeds, a.url) {
		t.Fatalf("discovery not reconfigured: %+v", b.Discovery().Config())
	}
	waitFor(t, 15*time.Second, "seed peer", func() bool {
		for _, p := range b.Discovery().Peers() {
			if p.ID == a.ServerID() && p.Verified {
				return true
			}
		}
		return false
	})
	if ctl.Level() != slog.LevelDebug || !ctl.JSON() {
		t.Fatalf("logging not applied: level=%v json=%v", ctl.Level(), ctl.JSON())
	}
	if st := b.lifecycle.Status(); st.Enabled {
		t.Fatal("lifecycle not disabled")
	}
	if inf := b.Info(); !slices.Contains(inf.Features, "discovery") || slices.Contains(inf.Features, "lifecycle") {
		t.Fatalf("features = %v", inf.Features)
	}

	// The admin discovery view shows the peer.
	r := do(t, "GET", bs.url+"/_gos3/admin/v1/discovery", nil, &rootCreds, nil)
	if r.status != 200 || !strings.Contains(string(r.body), a.ServerID()) {
		t.Fatalf("admin discovery: %d %s", r.status, r.body)
	}
}

func TestGracefulShutdown(t *testing.T) {
	srv := start(t, testConfig())
	if r := do(t, "PUT", srv.url+"/slow", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("CreateBucket: %d %s", r.status, r.body)
	}
	// The CreateBucket request may still count as in flight for a moment
	// after its response reached the client; wait for it to drain so the
	// in-flight gauge below observes the upload alone.
	inFlight := func() int64 { return srv.Metrics().Snapshot().InFlight }
	waitFor(t, 5*time.Second, "CreateBucket to finish", func() bool { return inFlight() == 0 })

	// Start an upload whose body arrives slowly.
	pr, pw := io.Pipe()
	const size = 1 << 20
	req, _ := http.NewRequest("PUT", srv.url+"/slow/object", pr)
	req.ContentLength = size
	if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", auth.PayloadUnsigned, time.Now()); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	// A fresh connection: reusing the idle keep-alive connection of the
	// CreateBucket request would race with Shutdown, which closes
	// connections that are still idle because the server has not read the
	// new request's headers yet (a client then sees a closed connection).
	// New connections are left to finish.
	uploadClient := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	go func() {
		resp, err := uploadClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- result{status: resp.StatusCode}
	}()
	chunk := bytes.Repeat([]byte("x"), size/2)
	if _, err := pw.Write(chunk); err != nil {
		t.Fatal(err)
	}
	// The client having written the chunk only means it reached the
	// socket: the server may not have accepted the connection or read the
	// request headers yet, and Shutdown would then reset the not yet
	// accepted (or still idle) connection. Wait until the S3 handler is
	// running, i.e. the connection is active and Shutdown must wait for it.
	waitFor(t, 5*time.Second, "upload handler to start", func() bool { return inFlight() == 1 })

	stopped := make(chan error, 1)
	go func() { stopped <- srv.stop() }()
	// The listener closes promptly while the upload is still in flight.
	waitFor(t, 5*time.Second, "listener close", func() bool {
		c := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
		resp, err := c.Get(srv.url + "/_gos3/health")
		if err == nil {
			resp.Body.Close()
		}
		return err != nil
	})
	select {
	case err := <-stopped:
		t.Fatalf("server stopped before the in-flight request finished: %v", err)
	default:
	}
	if _, err := pw.Write(chunk); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	res := <-done
	if res.err != nil || res.status != 200 {
		t.Fatalf("in-flight upload: status %d err %v", res.status, res.err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// Resources are released.
	if _, err := srv.Backend().ListBuckets(context.Background()); err == nil {
		t.Fatal("backend still open after shutdown")
	}
}

func TestRunLifecycleOnce(t *testing.T) {
	srv, err := New(testConfig(), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := srv.RunLifecycleOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Backend = "tape"
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "storage.backend") {
		t.Fatalf("err = %v", err)
	}
	cfg = testConfig()
	cfg.Server.TLSCert, cfg.Server.TLSKey = "/nonexistent/cert.pem", "/nonexistent/key.pem"
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("err = %v", err)
	}
}

func TestIsVirtualHost(t *testing.T) {
	domains := []string{"s3.local", ".example.com", "http://s3.lan:9000/"}
	tests := map[string]bool{
		"bucket.s3.local":         true,
		"bucket.s3.local:9000":    true,
		"a.b.s3.local":            true,
		"BUCKET.S3.LOCAL.":        true,
		"x.example.com":           true,
		"b.s3.lan:9000":           true,
		"s3.local":                false,
		"s3.local:9000":           false,
		"bucket.s3.localhost":     false,
		"bucket-s3.local":         false,
		"[::1]:9000":              false,
		"127.0.0.1:9000":          false,
		"":                        false,
		"bucket.other.s3.local.x": false,
	}
	for host, want := range tests {
		if got := isVirtualHost(host, domains); got != want {
			t.Errorf("isVirtualHost(%q) = %v, want %v", host, got, want)
		}
	}
	if isVirtualHost("bucket.s3.local", nil) {
		t.Error("match without domains")
	}
	// A configured domain is path-style even when it is also a subdomain
	// of another configured domain (the admin API and UI stay reachable).
	nested := []string{"example.com", "s3.example.com"}
	for host, want := range map[string]bool{
		"s3.example.com":       false,
		"S3.Example.COM.:9000": false,
		"example.com":          false,
		"b.s3.example.com":     true,
		"b.example.com":        true,
		"admin.s3.example.com": true,
	} {
		if got := isVirtualHost(host, nested); got != want {
			t.Errorf("nested domains: isVirtualHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLoggerSwitchesFormat(t *testing.T) {
	var buf bytes.Buffer
	logger, ctl := NewLogger(&buf, "info", "text")
	l := logger.With("component", "x")
	l.Debug("hidden")
	l.Info("first")
	ctl.Set("debug", "json")
	l.Debug("second")
	ctl.Set("bogus", "bogus") // ignored
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "msg=first") || !strings.Contains(lines[0], "component=x") {
		t.Fatalf("output:\n%s", out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil || rec["msg"] != "second" || rec["component"] != "x" {
		t.Fatalf("json line %q: %v", lines[1], err)
	}
	if ctl.Level() != slog.LevelDebug || !ctl.JSON() {
		t.Fatal("invalid values changed the settings")
	}
}
