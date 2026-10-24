package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
)

func relayPath(t *testing.T, meta relayRequest) string {
	t.Helper()
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return Prefix + "/proxy?request=" + base64.RawURLEncoding.EncodeToString(data)
}

func TestRelayAuthenticationAndPermission(t *testing.T) {
	e := newEnv(t)
	path := relayPath(t, relayRequest{URL: "http://127.0.0.1:1/", Method: "GET"})
	e.do(nil, "POST", path, nil).expectError(t, 401, codeUnauthenticated)
	rw := e.addUser("rw", iam.PolicyReadWrite)
	e.do(&rw, "POST", path, nil).expectError(t, 403, codeAccessDenied)
	e.putPolicy("relay", map[string]any{"Effect": "Allow", "Action": "admin:ProxyRequest", "Resource": "*"})
	relay := e.addUser("relay-user", "relay")
	var who whoAmI
	e.do(&relay, "GET", Prefix+"/whoami", nil).json(t, &who)
	if !who.Capabilities.Proxy {
		t.Fatal("relay capability missing")
	}
	e.do(&relay, "POST", path, nil).expectError(t, 502, "ProxyFailed")
}

func TestRelaySignedDestinationAndBinaryBody(t *testing.T) {
	e := newEnv(t)
	destination := newEnv(t)
	payload := bytes.Repeat([]byte{0, 1, 255, 128}, 300000) // larger than the admin JSON limit
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		result, err := destination.api.d.Verifier.Authenticate(r)
		if err != nil || result.Identity.IsAnonymous {
			http.Error(w, "destination signature rejected", 401)
			return
		}
		body, err := io.ReadAll(result.Body)
		if err != nil || !bytes.Equal(body, payload) {
			http.Error(w, "wrong payload", 400)
			return
		}
		if r.RequestURI != "/bucket/a%2Fb%20c%2B%25?versionId=a%2Bb&x=1&x=2" {
			t.Errorf("URI changed: %s", r.RequestURI)
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie forwarded")
		}
		w.Header().Set("ETag", `"binary"`)
		w.Header().Set("Set-Cookie", "dangerous=true")
		w.Header().Set("Clear-Site-Data", "* ")
		w.Header().Set("X-Gos3-Backing", "private")
		w.Header().Set("X-Amz-Meta-Owner", "team")
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.WriteHeader(201)
		w.Write(body)
	}))
	defer upstream.Close()
	req, _ := http.NewRequest("PUT", upstream.URL+"/bucket/a%2Fb%20c+%25?versionId=a%2Bb&x=1&x=2", bytes.NewReader(payload))
	req.Header.Set("X-Gos3-Backing", "private")
	digest := sha256.Sum256(payload)
	if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", hex.EncodeToString(digest[:]), time.Now()); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{}
	for name := range req.Header {
		headers[name] = req.Header.Get(name)
	}
	r := e.root("POST", strings.TrimPrefix(relayPath(t, relayRequest{URL: req.URL.String(), Method: "PUT", Headers: headers}), Prefix), payload, withHeader("Cookie", "local=secret"))
	r.expectStatus(t, 201)
	if r.header.Get("X-Gos3-Backing") != "private" || !strings.Contains(r.header.Get("Access-Control-Expose-Headers"), "X-Amz-Meta-Owner") {
		t.Fatal("object metadata not exposed")
	}
	if !bytes.Equal(r.body, payload) || r.header.Get("ETag") != `"binary"` || calls.Load() != 1 {
		t.Fatal("response changed")
	}
	if r.header.Get("Set-Cookie") != "" || r.header.Get("Clear-Site-Data") != "" || !strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("unsafe response headers")
	}
}

func TestRelayRejectsTamperingAndCleansSpool(t *testing.T) {
	e := newEnv(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	path := relayPath(t, relayRequest{URL: upstream.URL, Method: "PUT"})
	// Modify the payload after its relay signature was generated.
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader("good"))
	sum := sha256.Sum256([]byte("good"))
	auth.SignRequest(req, rootCreds, "us-east-1", "s3", hex.EncodeToString(sum[:]), time.Now())
	req.Body = io.NopCloser(strings.NewReader("evil"))
	e.send(req).expectError(t, 400, codeInvalidRequest)
	if calls.Load() != 0 {
		t.Fatal("tampered payload reached destination")
	}
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 0 {
		t.Fatalf("spool not cleaned: %v", entries)
	}
	// The signed destination metadata is protected even on empty requests.
	req, _ = http.NewRequest("GET", e.srv.URL+path, nil)
	auth.SignRequest(req, rootCreds, "us-east-1", "s3", "", time.Now())
	req.URL.RawQuery += "&extra=changed"
	e.send(req).expectError(t, 401, codeUnauthenticated)
}

func TestRelayPresignedRangesAndErrors(t *testing.T) {
	e := newEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/error" {
			w.WriteHeader(403)
			w.Write([]byte("destination denied"))
			return
		}
		if r.Header.Get("Range") != "bytes=1-2" {
			t.Error("Range missing")
		}
		w.Header().Set("Content-Range", "bytes 1-2/4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(206)
		w.Write([]byte("bc"))
	}))
	defer upstream.Close()
	raw := e.srv.URL + relayPath(t, relayRequest{URL: upstream.URL + "/file", Method: "GET"})
	signed, err := auth.PresignURL("GET", raw, rootCreds, "us-east-1", time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", signed, nil)
	req.Header.Set("Range", "bytes=1-2")
	r := e.send(req)
	r.expectStatus(t, 206)
	if string(r.body) != "bc" || r.header.Get("Content-Range") != "bytes 1-2/4" {
		t.Fatal("range response changed")
	}
	expired, _ := auth.PresignURL("GET", raw, rootCreds, "us-east-1", time.Second, time.Now().Add(-time.Hour))
	req, _ = http.NewRequest("GET", expired, nil)
	e.send(req).expectError(t, 401, codeUnauthenticated)
	r = e.do(&rootCreds, "POST", relayPath(t, relayRequest{URL: upstream.URL + "/error", Method: "GET"}), nil)
	r.expectStatus(t, 403)
	if string(r.body) != "destination denied" || r.header.Get(relayResultHeader) != "upstream" {
		t.Fatal("destination error confused with relay error")
	}
}

// relayUpstream serves body with exactly the given Content-Type values; nil
// sends no Content-Type at all (and suppresses the upstream's own sniffing).
func relayUpstream(t *testing.T, contentType []string, disposition, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = contentType
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Anything a browser could render as a PDF must download under the console
// origin, however the upstream spells or omits its Content-Type.
func TestRelayPDFIsDownloaded(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		name        string
		contentType []string
		disposition string
		want        string // exact Content-Disposition, or "attachment" for any attachment
		wantType    string
	}{
		{"control", []string{"application/pdf; charset=binary"}, `inline; filename="report.pdf"`, `attachment; filename=report.pdf`, "application/pdf"},
		// The relay must not let net/http sniff %PDF- into an inline application/pdf.
		{"missing type", nil, `inline; filename="r.pdf"`, `attachment; filename=r.pdf`, "application/octet-stream"},
		{"malformed parameters", []string{"application/pdf; charset"}, "inline", "attachment", "application/pdf"},
		{"comma-listed types", []string{"text/plain, application/pdf"}, `inline; filename="a.pdf"`, `attachment; filename=a.pdf`, "application/pdf"},
		{"duplicate headers", []string{"text/plain", "application/pdf"}, "", "attachment", "application/pdf"},
		{"uppercase", []string{"Application/PDF"}, "inline", "attachment", "application/pdf"},
		{"legacy alias text/pdf", []string{"text/pdf"}, "inline", "attachment", "application/pdf"},
		{"legacy alias x-pdf", []string{"application/x-pdf"}, "", "attachment", "application/pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := relayUpstream(t, tc.contentType, tc.disposition, "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
			r := e.do(&rootCreds, "GET", relayPath(t, relayRequest{URL: upstream.URL, Method: "GET"}), nil)
			r.expectStatus(t, 200)
			if types := r.header.Values("Content-Type"); len(types) != 1 || types[0] != tc.wantType {
				t.Fatalf("PDF Content-Type not collapsed to %s: %q", tc.wantType, types)
			}
			disposition := r.header.Get("Content-Disposition")
			if !strings.HasPrefix(disposition, "attachment") || (tc.want != "attachment" && disposition != tc.want) || len(r.header.Values("Content-Disposition")) != 1 {
				t.Fatalf("PDF must download under the console origin: %q", r.header.Values("Content-Disposition"))
			}
			if !strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") {
				t.Fatal("relay sandbox missing")
			}
		})
	}
}

func TestRelayNonPDFStaysInline(t *testing.T) {
	e := newEnv(t)
	upstream := relayUpstream(t, []string{"text/plain; charset=utf-8"}, `inline; filename="notes.txt"`, "hello")
	r := e.do(&rootCreds, "GET", relayPath(t, relayRequest{URL: upstream.URL, Method: "GET"}), nil)
	r.expectStatus(t, 200)
	if r.header.Get("Content-Type") != "text/plain; charset=utf-8" || r.header.Get("Content-Disposition") != `inline; filename="notes.txt"` {
		t.Fatalf("non-PDF response changed: %v", r.header)
	}
	if !strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("relay sandbox or nosniff missing")
	}
	// Without an upstream type the relay must not sniff one (e.g. text/html).
	for _, contentType := range [][]string{nil, {""}, {" "}} {
		upstream = relayUpstream(t, contentType, "", "<html><script>alert(1)</script></html>")
		r = e.do(&rootCreds, "GET", relayPath(t, relayRequest{URL: upstream.URL, Method: "GET"}), nil)
		r.expectStatus(t, 200)
		if types := r.header.Values("Content-Type"); len(types) != 1 || types[0] != "application/octet-stream" || r.header.Get("Content-Disposition") != "attachment" {
			t.Fatalf("untyped response (%q) sniffed or inline: %v", contentType, r.header)
		}
	}
	// HEAD carries no body to sniff; object metadata keeps its absent type.
	upstream = relayUpstream(t, nil, "", "")
	r = e.do(&rootCreds, "HEAD", relayPath(t, relayRequest{URL: upstream.URL, Method: "HEAD"}), nil)
	r.expectStatus(t, 200)
	if r.header.Get("Content-Type") != "" || r.header.Get("Content-Disposition") != "" {
		t.Fatalf("HEAD metadata invented: %v", r.header)
	}
}

func TestRelayGuardsAndCancellation(t *testing.T) {
	e := newEnv(t)
	for _, target := range []string{"file:///etc/passwd", "http://user:pass@localhost/", "http://localhost" + Prefix + "/proxy", "http://localhost" + Prefix + "/a/../proxy"} {
		e.do(&rootCreds, "POST", relayPath(t, relayRequest{URL: target, Method: "GET"}), nil).expectError(t, 400, codeInvalidRequest)
	}
	for _, host := range []string{"169.254.169.254", "[fd00:ec2::254]", "0.0.0.0", "224.0.0.1", "100.100.100.200"} {
		e.do(&rootCreds, "POST", relayPath(t, relayRequest{URL: "http://" + host + "/", Method: "GET"}), nil).expectError(t, 502, "ProxyFailed")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://169.254.169.254/", 302) }))
	defer redirect.Close()
	e.do(&rootCreds, "POST", relayPath(t, relayRequest{URL: redirect.URL, Method: "GET"}), nil).expectError(t, 502, "ProxyFailed")
	for _, name := range []string{"Cookie", "Connection", "X-Forwarded-For", "Proxy-Authorization"} {
		e.do(&rootCreds, "POST", relayPath(t, relayRequest{URL: redirect.URL, Method: "GET", Headers: map[string]string{name: "bad"}}), nil).expectError(t, 400, codeInvalidRequest)
	}
	e.do(&rootCreds, "GET", relayPath(t, relayRequest{URL: redirect.URL, Method: "DELETE"}), nil).expectError(t, 400, codeInvalidRequest)
	reached, stopped := make(chan struct{}), make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(reached); <-r.Context().Done(); close(stopped) }))
	defer blocking.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+relayPath(t, relayRequest{URL: blocking.URL, Method: "GET"}), nil)
	auth.SignRequest(req, rootCreds, "us-east-1", "s3", "", time.Now())
	done := make(chan struct{})
	go func() {
		res, _ := http.DefaultClient.Do(req)
		if res != nil {
			res.Body.Close()
		}
		close(done)
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("destination not reached")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("destination not canceled")
	}
	<-done
}

func TestRelayHeadMetadata(t *testing.T) {
	e := newEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Error("expected HEAD")
		}
		w.Header().Set("Content-Length", "1234")
		w.Header().Set("Cache-Control", "max-age=123")
		w.Header().Set("X-Gos3-Backing", "private")
	}))
	defer upstream.Close()
	result := e.do(&rootCreds, "HEAD", relayPath(t, relayRequest{URL: upstream.URL, Method: "HEAD"}), nil)
	result.expectStatus(t, 200)
	if result.header.Get("Content-Length") != "1234" || result.header.Get("X-Gos3-Proxy-Cache-Control") != "max-age=123" || result.header.Get("X-Gos3-Backing") != "private" {
		t.Fatal("HEAD metadata changed", result.header)
	}
}
