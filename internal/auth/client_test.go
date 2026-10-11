package auth

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPresignURLMatchesAWSExample reproduces the AWS documented presigned URL.
func TestPresignURLMatchesAWSExample(t *testing.T) {
	u, err := PresignURL("GET", "https://examplebucket.s3.amazonaws.com/test.txt", Credentials{testAK, testSK}, "us-east-1", 86400*time.Second, docTime)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u, "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404") {
		t.Fatalf("unexpected URL %s", u)
	}
	if !strings.HasPrefix(u, "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&") {
		t.Fatalf("unexpected URL %s", u)
	}
}

// TestSignRequestMatchesAWSExamples reproduces AWS documented header signatures.
func TestSignRequestMatchesAWSExamples(t *testing.T) {
	for target, sig := range map[string]string{
		"/?lifecycle":           "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		"/?max-keys=2&prefix=J": "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
	} {
		req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com"+target, nil)
		if err := SignRequest(req, Credentials{testAK, testSK}, "us-east-1", "s3", "", docTime); err != nil {
			t.Fatal(err)
		}
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=" + sig
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", target, got, want)
		}
		if req.Header.Get("X-Amz-Content-Sha256") != PayloadEmptySHA256 || req.Header.Get("X-Amz-Date") != "20130524T000000Z" {
			t.Errorf("headers not set: %v", req.Header)
		}
	}
}

func TestSignRequestRoundTrip(t *testing.T) {
	srv := newAuthServer(t, liveVerifier())
	c := Credentials{testAK, testSK}
	for _, key := range trickyKeys {
		t.Run(key, func(t *testing.T) {
			body := []byte("body of " + key)
			u := srv.URL + "/bucket/" + url.PathEscape(key) + "?prefix=" + url.QueryEscape("a b+c") + "&uploads"
			req, err := http.NewRequest("PUT", u, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			md := md5.Sum(body)
			req.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(md[:]))
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("X-Amz-Meta-Note", "  spaced   out ")
			req.Header.Set("X-Amz-Tagging", "a=b&c=d")
			sum := sha256.Sum256(body)
			if err := SignRequest(req, c, "us-west-2", "s3", hex.EncodeToString(sum[:]), time.Now()); err != nil {
				t.Fatal(err)
			}
			o := send(t, srv.Client(), req)
			if o.Code != "" || !bytes.Equal(o.Body, body) || o.Region != "us-west-2" {
				t.Fatalf("%s %s %+v", o.Code, o.Message, o)
			}
			// Presigned GET of the same key.
			pu, err := PresignURL("GET", srv.URL+"/bucket/"+url.PathEscape(key)+"?versionId=v%201", c, "", time.Minute, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			get, _ := http.NewRequest("GET", pu, nil)
			if o := send(t, srv.Client(), get); o.Code != "" || o.Method != MethodPresignedV4 || o.Region != DefaultRegion {
				t.Fatalf("presigned: %s %s (%s)", o.Code, o.Message, pu)
			}
		})
	}
}

func TestSignRequestEncodedSlashAndRawPath(t *testing.T) {
	v := liveVerifier()
	u, _ := url.Parse("http://h:9000/bucket/a%2Fb/c%20d")
	req := &http.Request{Method: "GET", URL: u, Header: http.Header{}, Host: u.Host}
	if err := SignRequest(req, Credentials{testAK, testSK}, "", "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := req.URL.EscapedPath(); got != "/bucket/a%2Fb/c%20d" {
		t.Fatalf("wire path %q", got)
	}
	if o := runAuth(v, toServer(req, nil)); o.Code != "" {
		t.Fatalf("%s %s", o.Code, o.Message)
	}
	// Go would send '=' and '!' raw; SignRequest pins the AWS encoding.
	u, _ = url.Parse("http://h/bucket/k=v!")
	req = &http.Request{Method: "GET", URL: u, Header: http.Header{}, Host: u.Host}
	if err := SignRequest(req, Credentials{testAK, testSK}, "", "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := req.URL.EscapedPath(); got != "/bucket/k%3Dv%21" {
		t.Fatalf("wire path %q", got)
	}
	if o := runAuth(v, toServer(req, nil)); o.Code != "" {
		t.Fatalf("%s %s", o.Code, o.Message)
	}
}

func TestPresignURLReplacesStaleParams(t *testing.T) {
	now := time.Now()
	c := Credentials{testAK, testSK}
	first, err := PresignURL("GET", "http://h/b/k?x=1", c, "", time.Minute, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := PresignURL("GET", first+"#frag", c, "", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(second)
	if n := len(pu.Query()["X-Amz-Signature"]); n != 1 || pu.Fragment != "" {
		t.Fatalf("stale params kept: %s", second)
	}
	r := httptest.NewRequest("GET", pu.RequestURI(), nil)
	r.Host = pu.Host
	if o := runAuth(liveVerifier(), r); o.Code != "" {
		t.Fatalf("%s %s", o.Code, o.Message)
	}
	// Method is part of the signature.
	r = httptest.NewRequest("PUT", pu.RequestURI(), nil)
	r.Host = pu.Host
	if o := runAuth(liveVerifier(), r); o.Code != "SignatureDoesNotMatch" {
		t.Fatalf("method change: %s", o.Code)
	}
	// Empty path becomes "/".
	root, _ := PresignURL("", "http://h", c, "", time.Minute, now)
	ru, _ := url.Parse(root)
	r = httptest.NewRequest("GET", ru.RequestURI(), nil)
	r.Host = ru.Host
	if ru.Path != "/" || runAuth(liveVerifier(), r).Code != "" {
		t.Fatalf("root presign: %s", root)
	}
}

func TestClientHelperErrors(t *testing.T) {
	c := Credentials{testAK, testSK}
	now := time.Now()
	if err := SignRequest(nil, c, "", "", "", now); err == nil {
		t.Error("nil request")
	}
	req, _ := http.NewRequest("GET", "http://h/", nil)
	if err := SignRequest(req, Credentials{}, "", "", "", now); err == nil {
		t.Error("empty credentials")
	}
	req = &http.Request{Method: "GET", URL: &url.URL{Path: "/x"}}
	if err := SignRequest(req, c, "", "", "", now); err == nil {
		t.Error("no host")
	}
	for _, d := range []time.Duration{0, 500 * time.Millisecond, -time.Second, MaxPresignExpires + time.Second} {
		if _, err := PresignURL("GET", "http://h/b", c, "", d, now); err == nil {
			t.Errorf("expires %v accepted", d)
		}
	}
	if _, err := PresignURL("GET", "http://h/b", c, "", MaxPresignExpires, now); err != nil {
		t.Errorf("7 days rejected: %v", err)
	}
	if _, err := PresignURL("GET", "/relative", c, "", time.Minute, now); err == nil {
		t.Error("relative URL accepted")
	}
	if _, err := PresignURL("GET", "http://h/%zz", c, "", time.Minute, now); err == nil {
		t.Error("invalid URL accepted")
	}
	if _, err := PresignURL("GET", "http://h/", Credentials{}, "", time.Minute, now); err == nil {
		t.Error("empty credentials accepted")
	}
	// Payload defaults.
	// A re-readable body is hashed (and left unread) ...
	req, _ = http.NewRequest("PUT", "http://h/b/k", strings.NewReader("x"))
	_ = SignRequest(req, c, "", "", "", now)
	if got, want := req.Header.Get("X-Amz-Content-Sha256"), "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"; got != want {
		t.Errorf("re-readable body default: %s, want %s", got, want)
	}
	if b, _ := io.ReadAll(req.Body); string(b) != "x" {
		t.Errorf("body consumed: %q", b)
	}
	// ... any other body is sent as UNSIGNED-PAYLOAD.
	req, _ = http.NewRequest("PUT", "http://h/b/k", io.NopCloser(strings.NewReader("x")))
	req.ContentLength = 1
	_ = SignRequest(req, c, "", "", "", now)
	if req.Header.Get("X-Amz-Content-Sha256") != PayloadUnsigned {
		t.Errorf("stream body default: %s", req.Header.Get("X-Amz-Content-Sha256"))
	}
	// Re-signing replaces the previous Authorization.
	_ = SignRequest(req, c, "", "", "", now)
	if len(req.Header.Values("Authorization")) != 1 {
		t.Error("Authorization duplicated")
	}
}

func TestSigningKey(t *testing.T) {
	// AWS documentation (sigv4 "Deriving the signing key") example.
	k := SigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam")
	if got := hex.EncodeToString(k); got != "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d" {
		t.Fatalf("signing key %s", got)
	}
}

func TestConcurrentAuthenticate(t *testing.T) {
	v := liveVerifier()
	c := Credentials{testAK, testSK}
	errs := make(chan string, 64)
	for i := 0; i < 64; i++ {
		go func(i int) {
			body := bytes.Repeat([]byte{byte(i)}, 1000+i)
			sum := sha256.Sum256(body)
			req, _ := http.NewRequest("PUT", "http://h/b/key-"+strings.Repeat("x", i), bytes.NewReader(body))
			if err := SignRequest(req, c, "", "", hex.EncodeToString(sum[:]), time.Now()); err != nil {
				errs <- err.Error()
				return
			}
			o := runAuth(v, toServer(req, body))
			if o.Code != "" || !bytes.Equal(o.Body, body) {
				errs <- o.Code + " " + o.Message
				return
			}
			errs <- ""
		}(i)
	}
	for i := 0; i < 64; i++ {
		if e := <-errs; e != "" {
			t.Error(e)
		}
	}
}
