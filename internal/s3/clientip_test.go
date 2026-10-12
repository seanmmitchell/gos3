package s3

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"testing"

	"github.com/seanmmitchell/gos3/internal/clientip"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func TestClientInfo(t *testing.T) {
	s := New(Options{Backend: memory.New()})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	if ip, sec := s.clientInfo(r); ip != "10.1.2.3" || sec {
		t.Fatalf("untrusted: %s %v", ip, sec)
	}
	// Previously the first (client-controlled) entry won.
	s.clientIP = clientip.New(true, nil)
	if ip, sec := s.clientInfo(r); ip != "10.0.0.1" || !sec {
		t.Fatalf("trusted, no proxy list (rightmost entry): %s %v", ip, sec)
	}
	s.clientIP = clientip.New(true, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	if ip, sec := s.clientInfo(r); ip != "203.0.113.9" || !sec {
		t.Fatalf("trusted proxies: %s %v", ip, sec)
	}
	r.Header.Set("X-Forwarded-For", "garbage")
	if ip, _ := s.clientInfo(r); ip != "10.1.2.3" {
		t.Fatalf("garbage xff: %s", ip)
	}
}

// appendingProxy starts a reverse proxy in front of target that appends the
// peer address to X-Forwarded-For and "http" to X-Forwarded-Proto, like
// nginx with $proxy_add_x_forwarded_for (and many load balancers). When
// clientAddr is set, the proxy sees every request as coming from that
// address (a client on another host); otherwise from the real peer. The
// proxy always reaches target from 127.0.0.1: only that loopback address
// exists on every platform (macOS has no 127.0.0.2).
func appendingProxy(t *testing.T, target string, clientAddr string) *httptest.Server {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u) // Director: appends to X-Forwarded-For
	director := rp.Director
	rp.Director = func(r *http.Request) {
		director(r)
		r.Header.Add("X-Forwarded-Proto", "http")
	}
	var h http.Handler = rp
	if clientAddr != "" {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = net.JoinHostPort(clientAddr, "40000")
			rp.ServeHTTP(w, r)
		})
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestSourceIpThroughAppendingProxy is the audit scenario: behind a proxy
// that appends to X-Forwarded-For, a client could put an allowed address
// first ("X-Forwarded-For: 10.1.2.3") and satisfy a bucket policy's
// aws:SourceIp condition, because gos3 used the first entry. It now walks
// the header from the right, so the proxy-appended peer address counts.
// Likewise a client-sent "X-Forwarded-Proto: https" no longer wins over the
// value the proxy appends.
func TestSourceIpThroughAppendingProxy(t *testing.T) {
	const policy = `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::intra/office/*",
		 "Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}},
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::intra/tls/*",
		 "Condition":{"Bool":{"aws:SecureTransport":"true"}}},
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::intra/local/*",
		 "Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.1/32"}}},
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::intra/remote/*",
		 "Condition":{"IpAddress":{"aws:SourceIp":"192.0.2.50/32"}}}]}`
	get := func(t *testing.T, base, path string, hdr http.Header) int {
		t.Helper()
		req, err := http.NewRequest("GET", base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, vs := range hdr {
			req.Header[k] = vs
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	setup := func(t *testing.T, proxies ...string) *testEnv {
		e := newTestEnv(t, withOptions(func(o *Options) {
			p, err := clientip.ParsePrefixes(proxies)
			if err != nil {
				t.Fatal(err)
			}
			o.ClientIP = clientip.New(true, p)
		}))
		requireAuthz(t, e)
		e.createBucket("intra")
		for _, k := range []string{"office/k", "tls/k", "local/k", "remote/k"} {
			e.putObject("intra", k, []byte("secret"), storage.ObjectMeta{})
		}
		e.putBucketConfig("intra", storage.ConfigPolicy, policy)
		return e
	}
	spoof := http.Header{"X-Forwarded-For": {"10.1.2.3"}, "X-Forwarded-Proto": {"https"}}

	t.Run("empty trusted_proxies", func(t *testing.T) {
		e := setup(t)
		proxy := appendingProxy(t, e.srv.URL, "")
		// The proxy forwards "X-Forwarded-For: 10.1.2.3, 127.0.0.1".
		if code := get(t, proxy.URL, "/intra/office/k", spoof); code != 403 {
			t.Errorf("spoofed X-Forwarded-For through the proxy: %d, want 403", code)
		}
		if code := get(t, proxy.URL, "/intra/tls/k", spoof); code != 403 {
			t.Errorf("spoofed X-Forwarded-Proto through the proxy: %d, want 403", code)
		}
		// The address the proxy appended is the client.
		if code := get(t, proxy.URL, "/intra/local/k", spoof); code != 200 {
			t.Errorf("real client address: %d, want 200", code)
		}
	})
	t.Run("proxy listed in trusted_proxies", func(t *testing.T) {
		// The proxy reaches gos3 from 127.0.0.1, a trusted proxy; its
		// client is at 192.0.2.50. The proxy forwards
		// "X-Forwarded-For: 10.1.2.3, 192.0.2.50".
		e := setup(t, "127.0.0.1")
		proxy := appendingProxy(t, e.srv.URL, "192.0.2.50")
		if code := get(t, proxy.URL, "/intra/office/k", spoof); code != 403 {
			t.Errorf("spoofed X-Forwarded-For through the proxy: %d, want 403", code)
		}
		if code := get(t, proxy.URL, "/intra/remote/k", spoof); code != 200 {
			t.Errorf("real client address: %d, want 200", code)
		}
		// The proxy's own (trusted) address is not the client.
		if code := get(t, proxy.URL, "/intra/local/k", spoof); code != 403 {
			t.Errorf("trusted proxy address taken as the client: %d, want 403", code)
		}
	})
	t.Run("untrusted peer", func(t *testing.T) {
		// Only 192.0.2.1 is a trusted proxy: requests from 127.0.0.1 are
		// taken at face value, whatever headers they carry.
		e := setup(t, "192.0.2.1")
		if code := get(t, e.srv.URL, "/intra/office/k", spoof); code != 403 {
			t.Errorf("spoofed headers from an untrusted peer: %d, want 403", code)
		}
		if code := get(t, e.srv.URL, "/intra/tls/k", spoof); code != 403 {
			t.Errorf("spoofed X-Forwarded-Proto from an untrusted peer: %d, want 403", code)
		}
		if code := get(t, e.srv.URL, "/intra/local/k", spoof); code != 200 {
			t.Errorf("peer address: %d, want 200", code)
		}
	})
}
