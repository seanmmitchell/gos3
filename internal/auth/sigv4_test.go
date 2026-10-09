package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go/encoding/httpbinding"
)

func docAuth(signedHeaders, sig string) string {
	return "AWS4-HMAC-SHA256 Credential=" + testAK + "/20130524/us-east-1/s3/aws4_request,SignedHeaders=" +
		signedHeaders + ",Signature=" + sig
}

// TestAWSDocumentedExamples verifies the examples from the AWS "Signature
// Calculations for the Authorization Header" documentation.
func TestAWSDocumentedExamples(t *testing.T) {
	v := newTestVerifier(docTime.Add(3 * time.Minute))
	tests := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		body    string
		authz   string
	}{
		{
			name: "GetObject", method: "GET", target: "/test.txt",
			headers: map[string]string{
				"Range":                "bytes=0-9",
				"X-Amz-Content-Sha256": PayloadEmptySHA256,
				"X-Amz-Date":           "20130524T000000Z",
			},
			authz: docAuth("host;range;x-amz-content-sha256;x-amz-date", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"),
		},
		{
			name: "PutObject raw $", method: "PUT", target: "/test$file.text",
			headers: map[string]string{
				"Date":                 "Fri, 24 May 2013 00:00:00 GMT",
				"X-Amz-Date":           "20130524T000000Z",
				"X-Amz-Storage-Class":  "REDUCED_REDUNDANCY",
				"X-Amz-Content-Sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			},
			body:  "Welcome to Amazon S3.",
			authz: docAuth("date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class", "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"),
		},
		{
			name: "PutObject encoded $", method: "PUT", target: "/test%24file.text",
			headers: map[string]string{
				"Date":                 "Fri, 24 May 2013 00:00:00 GMT",
				"X-Amz-Date":           "20130524T000000Z",
				"X-Amz-Storage-Class":  "REDUCED_REDUNDANCY",
				"X-Amz-Content-Sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			},
			body:  "Welcome to Amazon S3.",
			authz: docAuth("date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class", "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"),
		},
		{
			name: "GetBucketLifecycle", method: "GET", target: "/?lifecycle",
			headers: map[string]string{
				"X-Amz-Date":           "20130524T000000Z",
				"X-Amz-Content-Sha256": PayloadEmptySHA256,
			},
			authz: docAuth("host;x-amz-content-sha256;x-amz-date", "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"),
		},
		{
			name: "ListObjects", method: "GET", target: "/?max-keys=2&prefix=J",
			headers: map[string]string{
				"X-Amz-Date":           "20130524T000000Z",
				"X-Amz-Content-Sha256": PayloadEmptySHA256,
			},
			authz: docAuth("host;x-amz-content-sha256;x-amz-date", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"),
		},
		{
			name: "ListObjects reordered query", method: "GET", target: "/?prefix=J&max-keys=2",
			headers: map[string]string{
				"X-Amz-Date":           "20130524T000000Z",
				"X-Amz-Content-Sha256": PayloadEmptySHA256,
			},
			authz: "AWS4-HMAC-SHA256  Credential=" + testAK + "/20130524/us-east-1/s3/aws4_request ,  SignedHeaders=host;x-amz-content-sha256;x-amz-date ,Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			r.Host = "examplebucket.s3.amazonaws.com"
			for k, val := range tc.headers {
				r.Header.Set(k, val)
			}
			r.Header.Set("Authorization", tc.authz)
			o := runAuth(v, r)
			if o.Code != "" {
				t.Fatalf("got %s (%s) at %s", o.Code, o.Message, o.Stage)
			}
			if o.User != "user-"+testAK || o.Method != MethodHeaderV4 || o.Region != "us-east-1" {
				t.Fatalf("unexpected outcome %+v", o)
			}
			if string(o.Body) != tc.body {
				t.Fatalf("body = %q", o.Body)
			}
		})
	}
}

func TestAWSDocumentedPresignedExample(t *testing.T) {
	const target = "/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	mk := func() *http.Request {
		r := httptest.NewRequest("GET", target, nil)
		r.Host = "examplebucket.s3.amazonaws.com"
		return r
	}
	if o := runAuth(newTestVerifier(docTime.Add(time.Hour)), mk()); o.Code != "" || o.Method != MethodPresignedV4 {
		t.Fatalf("valid presign rejected: %+v", o)
	}
	o := runAuth(newTestVerifier(docTime.Add(86401*time.Second)), mk())
	if o.Code != "AccessDenied" || o.Message != "Request has expired" {
		t.Fatalf("expired presign: %+v", o)
	}
	o = runAuth(newTestVerifier(docTime.Add(-time.Hour)), mk())
	if o.Code != "AccessDenied" || o.Message != "Request is not valid yet" {
		t.Fatalf("future presign: %+v", o)
	}
	// Tampering with the expiry invalidates the signature.
	r := httptest.NewRequest("GET", strings.Replace(target, "86400", "86401", 1), nil)
	r.Host = "examplebucket.s3.amazonaws.com"
	if o := runAuth(newTestVerifier(docTime.Add(time.Hour)), r); o.Code != "SignatureDoesNotMatch" {
		t.Fatalf("tampered presign: %+v", o)
	}
	// Different host.
	r = mk()
	r.Host = "otherbucket.s3.amazonaws.com"
	if o := runAuth(newTestVerifier(docTime.Add(time.Hour)), r); o.Code != "SignatureDoesNotMatch" {
		t.Fatalf("wrong host: %+v", o)
	}
}

// sdkSign signs req with the AWS SDK v2 signer configured like the S3
// client (no double path escaping).
func sdkSign(t *testing.T, req *http.Request, ak, sk, payloadHash, region string, at time.Time) {
	t.Helper()
	s := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	err := s.SignHTTP(context.Background(), aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk}, req, payloadHash, "s3", region, at)
	if err != nil {
		t.Fatal(err)
	}
}

func sdkPresign(t *testing.T, req *http.Request, expires time.Duration, at time.Time) (string, http.Header) {
	t.Helper()
	q := req.URL.Query()
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	req.URL.RawQuery = q.Encode()
	s := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	u, hdr, err := s.PresignHTTP(context.Background(), aws.Credentials{AccessKeyID: testAK, SecretAccessKey: testSK}, req, PayloadUnsigned, "s3", "us-east-1", at)
	if err != nil {
		t.Fatal(err)
	}
	return u, hdr
}

// s3URL builds a URL the way the S3 SDK does: the key escaped with
// httpbinding.EscapePath (AWS rules) and the query encoded with %20 spaces.
func s3URL(base, bucket, key string, query url.Values) string {
	p := "/" + bucket
	if key != "" {
		p += "/" + httpbinding.EscapePath(key, false)
	}
	u := base + p
	if len(query) > 0 {
		u += "?" + strings.ReplaceAll(query.Encode(), "+", "%20")
	}
	return u
}

var trickyKeys = []string{
	"simple.txt",
	"dir/sub/file.txt",
	"with space.txt",
	"plus+sign",
	"equals=sign&amp",
	"percent%41literal",
	"percent%",
	"unicode/日本語/ファイル.txt",
	"emoji-😀",
	"special !\"#$&'()*,:;<>?@[\\]^`{|}",
	"tilde~under_score-dash.dot",
	"double//slash",
	"trailing/",
	"./dot/../segments",
	"a%2Fb",
	"ctrl\x01char",
	"%E2%82%AC",
}

func TestSDKSignedRequestsVerify(t *testing.T) {
	srv := newAuthServer(t, liveVerifier())
	queries := []url.Values{
		nil,
		{"acl": {""}},
		{"uploads": {""}, "prefix": {"a b/c"}},
		{"list-type": {"2"}, "prefix": {"日本/"}, "delimiter": {"/"}, "start-after": {"x=y&z"}},
		{"versionId": {"3/L4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY+MTRCxf3vjVBH40Nr8X8gdRQBpUMLUo"}},
		{"tagging": {""}, "k": {"v1", "v2", "a"}},
		{"prefix": {"~!*'()"}},
	}
	for i, key := range trickyKeys {
		q := queries[i%len(queries)]
		for _, method := range []string{"GET", "PUT", "DELETE", "POST"} {
			body := []byte("payload for " + key)
			if method == "GET" || method == "DELETE" {
				body = nil
			}
			t.Run(method+" "+key, func(t *testing.T) {
				req, err := http.NewRequest(method, s3URL(srv.URL, "bucket", key, q), bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Amz-Meta-Spaced", "  a   b  c ")
				req.Header.Add("X-Amz-Meta-Multi", "one")
				req.Header.Add("X-Amz-Meta-Multi", " two ")
				req.Header.Set("Content-Type", "text/plain; charset=utf-8")
				req.Header.Set("X-Amz-Acl", "private")
				hash := sha256.Sum256(body)
				hexHash := hex.EncodeToString(hash[:])
				req.Header.Set("X-Amz-Content-Sha256", hexHash)
				sdkSign(t, req, testAK, testSK, hexHash, "eu-west-1", time.Now())
				o := send(t, srv.Client(), req)
				if o.Code != "" {
					t.Fatalf("%s: %s", o.Code, o.Message)
				}
				if o.Region != "eu-west-1" || o.User != "user-"+testAK || !bytes.Equal(o.Body, body) {
					t.Fatalf("unexpected outcome %+v", o)
				}
			})
		}
	}
}

// TestHeadRequestsVerify checks HEAD via direct server-side requests.
func TestHeadRequestsVerify(t *testing.T) {
	v := liveVerifier()
	for _, key := range trickyKeys {
		req, err := http.NewRequest("HEAD", s3URL("http://example.com:9000", "bucket", key, nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Content-Sha256", PayloadEmptySHA256)
		sdkSign(t, req, testAK, testSK, PayloadEmptySHA256, "us-east-1", time.Now())
		sr := httptest.NewRequest("HEAD", req.URL.RequestURI(), nil)
		sr.Host = req.URL.Host
		sr.Header = req.Header.Clone()
		if o := runAuth(v, sr); o.Code != "" {
			t.Errorf("%q: %s %s", key, o.Code, o.Message)
		}
	}
}

func TestSDKPresignedRequestsVerify(t *testing.T) {
	srv := newAuthServer(t, liveVerifier())
	for _, key := range trickyKeys {
		t.Run(key, func(t *testing.T) {
			req, _ := http.NewRequest("GET", s3URL(srv.URL, "bucket", key, url.Values{"response-content-type": {"text/plain"}}), nil)
			u, hdr := sdkPresign(t, req, time.Hour, time.Now())
			get, _ := http.NewRequest("GET", u, nil)
			for k, vals := range hdr {
				if strings.EqualFold(k, "host") {
					continue
				}
				get.Header[k] = vals
			}
			o := send(t, srv.Client(), get)
			if o.Code != "" || o.Method != MethodPresignedV4 {
				t.Fatalf("%s: %s (%s)", o.Code, o.Message, u)
			}
		})
	}
	// Presigned PUT with a signed x-amz-meta header (kept as a header by the SDK).
	body := []byte("presigned upload")
	req, _ := http.NewRequest("PUT", s3URL(srv.URL, "bucket", "up load", nil), bytes.NewReader(body))
	req.Header.Set("X-Amz-Meta-Owner", "me")
	u, hdr := sdkPresign(t, req, 10*time.Minute, time.Now())
	put, _ := http.NewRequest("PUT", u, bytes.NewReader(body))
	for k, vals := range hdr {
		if !strings.EqualFold(k, "host") {
			put.Header[k] = vals
		}
	}
	if o := send(t, srv.Client(), put); o.Code != "" || !bytes.Equal(o.Body, body) {
		t.Fatalf("presigned PUT: %+v", o)
	}
	// Adding an unsigned x-amz-* header to a presigned request is refused.
	put, _ = http.NewRequest("PUT", u, bytes.NewReader(body))
	for k, vals := range hdr {
		if !strings.EqualFold(k, "host") {
			put.Header[k] = vals
		}
	}
	put.Header.Set("X-Amz-Acl", "public-read")
	if o := send(t, srv.Client(), put); o.Code != "AccessDenied" {
		t.Fatalf("unsigned header on presigned PUT: %+v", o)
	}
}

// signedReq returns a server-side request signed by the SDK at time at.
func signedReq(t *testing.T, method, target string, body []byte, at time.Time, mod func(*http.Request)) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "http://bucket.example.com"+target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if mod != nil {
		mod(req)
	}
	hash := sha256.Sum256(body)
	h := hex.EncodeToString(hash[:])
	if req.Header.Get("X-Amz-Content-Sha256") == "" {
		req.Header.Set("X-Amz-Content-Sha256", h)
	}
	sdkSign(t, req, testAK, testSK, req.Header.Get("X-Amz-Content-Sha256"), "us-east-1", at)
	return toServer(req, body)
}

// toServer converts a client request into what an http.Server handler sees.
func toServer(req *http.Request, body []byte) *http.Request {
	sr := httptest.NewRequest(req.Method, req.URL.RequestURI(), bytes.NewReader(body))
	sr.Host = req.URL.Host
	if req.Host != "" {
		sr.Host = req.Host
	}
	sr.Header = req.Header.Clone()
	return sr
}

func TestHeaderAuthErrors(t *testing.T) {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	body := []byte("hello")
	tests := []struct {
		name  string
		req   func() *http.Request
		code  string
		stage string
	}{
		{"ok", func() *http.Request { return signedReq(t, "PUT", "/k", body, now, nil) }, "", ""},
		{"wrong secret", func() *http.Request {
			req, _ := http.NewRequest("PUT", "http://bucket.example.com/k", bytes.NewReader(body))
			req.Header.Set("X-Amz-Content-Sha256", PayloadUnsigned)
			sdkSign(t, req, testAK, "bad", PayloadUnsigned, "us-east-1", now)
			return toServer(req, body)
		}, "SignatureDoesNotMatch", "auth"},
		{"unknown key", func() *http.Request {
			req, _ := http.NewRequest("PUT", "http://bucket.example.com/k", bytes.NewReader(body))
			req.Header.Set("X-Amz-Content-Sha256", PayloadUnsigned)
			sdkSign(t, req, "NOPE", testSK, PayloadUnsigned, "us-east-1", now)
			return toServer(req, body)
		}, "InvalidAccessKeyId", "auth"},
		{"skew past", func() *http.Request { return signedReq(t, "PUT", "/k", body, now.Add(-16*time.Minute), nil) }, "RequestTimeTooSkewed", "auth"},
		{"skew future", func() *http.Request { return signedReq(t, "PUT", "/k", body, now.Add(16*time.Minute), nil) }, "RequestTimeTooSkewed", "auth"},
		{"within skew", func() *http.Request { return signedReq(t, "PUT", "/k", body, now.Add(14*time.Minute), nil) }, "", ""},
		{"tampered path", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r2 := httptest.NewRequest("PUT", "/k2", bytes.NewReader(body))
			r2.Host, r2.Header = r.Host, r.Header
			return r2
		}, "SignatureDoesNotMatch", "auth"},
		{"tampered query", func() *http.Request {
			r := signedReq(t, "GET", "/?prefix=a", nil, now, nil)
			r2 := httptest.NewRequest("GET", "/?prefix=b", nil)
			r2.Host, r2.Header = r.Host, r.Header
			return r2
		}, "SignatureDoesNotMatch", "auth"},
		{"added query", func() *http.Request {
			r := signedReq(t, "GET", "/?prefix=a", nil, now, nil)
			r2 := httptest.NewRequest("GET", "/?prefix=a&versionId=1", nil)
			r2.Host, r2.Header = r.Host, r.Header
			return r2
		}, "SignatureDoesNotMatch", "auth"},
		{"tampered signed header", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, func(r *http.Request) { r.Header.Set("X-Amz-Meta-A", "1") })
			r.Header.Set("X-Amz-Meta-A", "2")
			return r
		}, "SignatureDoesNotMatch", "auth"},
		{"tampered method", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Method = "POST"
			return r
		}, "SignatureDoesNotMatch", "auth"},
		{"unsigned amz header", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("X-Amz-Acl", "public-read")
			return r
		}, "AccessDenied", "auth"},
		{"body hash mismatch", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Body = io.NopCloser(strings.NewReader("HELLO"))
			return r
		}, "XAmzContentSHA256Mismatch", "body"},
		{"body hash mismatch (declared)", func() *http.Request {
			return signedReq(t, "PUT", "/k", body, now, func(r *http.Request) {
				r.Header.Set("X-Amz-Content-Sha256", strings.Repeat("a", 64))
			})
		}, "XAmzContentSHA256Mismatch", "body"},
		{"unsigned payload", func() *http.Request {
			return signedReq(t, "PUT", "/k", body, now, func(r *http.Request) {
				r.Header.Set("X-Amz-Content-Sha256", PayloadUnsigned)
			})
		}, "", ""},
		{"invalid payload mode", func() *http.Request {
			return signedReq(t, "PUT", "/k", body, now, func(r *http.Request) {
				r.Header.Set("X-Amz-Content-Sha256", "bogus")
			})
		}, "InvalidArgument", "auth"},
		{"sigv4a payload", func() *http.Request {
			return signedReq(t, "PUT", "/k", body, now, func(r *http.Request) {
				r.Header.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD")
			})
		}, "NotImplemented", "auth"},
		{"missing content sha", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Del("X-Amz-Content-Sha256")
			return r
		}, "InvalidRequest", "auth"},
		{"missing date", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Del("X-Amz-Date")
			return r
		}, "AccessDenied", "auth"},
		{"bad date", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("X-Amz-Date", "2024-03-01T12:00:00Z")
			return r
		}, "AccessDenied", "auth"},
		{"credential date mismatch", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "/20240301/", "/20240302/", 1))
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"wrong service", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "/s3/", "/iam/", 1))
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"wrong terminator", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "aws4_request", "aws5_request", 1))
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"short credential", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AK/20240301/s3/aws4_request, SignedHeaders=host, Signature="+strings.Repeat("0", 64))
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"missing signature component", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+testAK+"/20240301/us-east-1/s3/aws4_request, SignedHeaders=host")
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"duplicate component", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", r.Header.Get("Authorization")+", Signature=abc")
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"unknown component", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", r.Header.Get("Authorization")+", Foo=bar")
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"component without =", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", r.Header.Get("Authorization")+", junk")
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"host not signed", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "host;", "", 1))
			return r
		}, "AccessDenied", "auth"},
		{"empty signed header name", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "host;", "host;;", 1))
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"sigv2 header", func() *http.Request {
			r := httptest.NewRequest("GET", "/k", nil)
			r.Header.Set("Authorization", "AWS "+testAK+":frJIUN8DYpKDtOLCwo//yllqDzg=")
			return r
		}, "AccessDenied", "auth"},
		{"sigv2 query", func() *http.Request {
			return httptest.NewRequest("GET", "/k?AWSAccessKeyId="+testAK+"&Expires=1141889120&Signature=vjbyPxybdZaNmGa%2ByT272YEAiv4%3D", nil)
		}, "AccessDenied", "auth"},
		{"unsupported algorithm", func() *http.Request {
			r := httptest.NewRequest("GET", "/k", nil)
			r.Header.Set("Authorization", "Bearer abc")
			return r
		}, "InvalidArgument", "auth"},
		{"sigv4a header", func() *http.Request {
			r := httptest.NewRequest("GET", "/k", nil)
			r.Header.Set("Authorization", "AWS4-ECDSA-P256-SHA256 Credential=x")
			return r
		}, "NotImplemented", "auth"},
		{"header and query auth", func() *http.Request {
			r := signedReq(t, "GET", "/k?X-Amz-Algorithm=AWS4-HMAC-SHA256", nil, now, nil)
			return r
		}, "InvalidArgument", "auth"},
		{"two authorization headers", func() *http.Request {
			r := signedReq(t, "PUT", "/k", body, now, nil)
			r.Header.Add("Authorization", r.Header.Get("Authorization"))
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"security token", func() *http.Request {
			return signedReq(t, "PUT", "/k", body, now, func(r *http.Request) { r.Header.Set("X-Amz-Security-Token", "tok") })
		}, "InvalidToken", "auth"},
		{"huge authorization", func() *http.Request {
			r := httptest.NewRequest("GET", "/k", nil)
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 "+strings.Repeat("x", 20000))
			return r
		}, "AuthorizationHeaderMalformed", "auth"},
		{"default port stripped by client", func() *http.Request {
			r := signedReq(t, "GET", "/k", nil, now, nil)
			r.Host += ":80"
			return r
		}, "", ""},
		{"Date header instead of x-amz-date", func() *http.Request {
			req, _ := http.NewRequest("GET", "http://bucket.example.com/k", nil)
			req.Header.Set("X-Amz-Content-Sha256", PayloadEmptySHA256)
			sdkSign(t, req, testAK, testSK, PayloadEmptySHA256, "us-east-1", now)
			// Re-sign manually using the Date header only.
			req.Header.Del("X-Amz-Date")
			req.Header.Set("Date", now.Format(http.TimeFormat))
			creq := buildCanonicalRequest("GET", "/k", "", "date:"+now.Format(http.TimeFormat)+"\nhost:bucket.example.com\nx-amz-content-sha256:"+PayloadEmptySHA256+"\n",
				"date;host;x-amz-content-sha256", PayloadEmptySHA256)
			sts := buildStringToSign(now.Format(iso8601Format), "20240301/us-east-1/s3/aws4_request", creq)
			sig := hex.EncodeToString(hmacSHA256(SigningKey(testSK, "20240301", "us-east-1", "s3"), sts))
			req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+testAK+"/20240301/us-east-1/s3/aws4_request, SignedHeaders=date;host;x-amz-content-sha256, Signature="+sig)
			return toServer(req, nil)
		}, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := runAuth(v, tc.req())
			if o.Code != tc.code || (tc.stage != "" && o.Stage != tc.stage) {
				t.Fatalf("got code=%q stage=%q msg=%q, want %q at %q", o.Code, o.Stage, o.Message, tc.code, tc.stage)
			}
		})
	}
}

func TestStrictRegion(t *testing.T) {
	now := time.Now()
	strict := newTestVerifier(now, func(o *Options) { o.StrictRegion = true; o.Region = "us-east-1" })
	lax := newTestVerifier(now)
	req, _ := http.NewRequest("GET", "http://h/b", nil)
	req.Header.Set("X-Amz-Content-Sha256", PayloadEmptySHA256)
	sdkSign(t, req, testAK, testSK, PayloadEmptySHA256, "eu-central-1", now)
	if o := runAuth(strict, toServer(req, nil)); o.Code != "AuthorizationHeaderMalformed" {
		t.Fatalf("strict: %+v", o)
	}
	if o := runAuth(lax, toServer(req, nil)); o.Code != "" || o.Region != "eu-central-1" {
		t.Fatalf("lax: %+v", o)
	}
	req, _ = http.NewRequest("GET", "http://h/b", nil)
	req.Header.Set("X-Amz-Content-Sha256", PayloadEmptySHA256)
	sdkSign(t, req, testAK, testSK, PayloadEmptySHA256, "us-east-1", now)
	if o := runAuth(strict, toServer(req, nil)); o.Code != "" {
		t.Fatalf("strict same region: %+v", o)
	}
}

func TestPresignedErrors(t *testing.T) {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	good, err := PresignURL("GET", "http://bucket.example.com/key?versionId=3", Credentials{testAK, testSK}, "us-east-1", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	gu, _ := url.Parse(good)
	mk := func(edit func(q url.Values)) *http.Request {
		q := gu.Query()
		if edit != nil {
			edit(q)
		}
		r := httptest.NewRequest("GET", gu.Path+"?"+q.Encode(), nil)
		r.Host = gu.Host
		return r
	}
	tests := []struct {
		name string
		r    *http.Request
		code string
	}{
		{"ok", mk(nil), ""},
		{"missing expires", mk(func(q url.Values) { q.Del("X-Amz-Expires") }), "AuthorizationQueryParametersError"},
		{"missing signature", mk(func(q url.Values) { q.Del("X-Amz-Signature") }), "AuthorizationQueryParametersError"},
		{"bad algorithm", mk(func(q url.Values) { q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA1") }), "AuthorizationQueryParametersError"},
		{"bad date", mk(func(q url.Values) { q.Set("X-Amz-Date", "yesterday") }), "AuthorizationQueryParametersError"},
		{"expires NaN", mk(func(q url.Values) { q.Set("X-Amz-Expires", "soon") }), "AuthorizationQueryParametersError"},
		{"expires negative", mk(func(q url.Values) { q.Set("X-Amz-Expires", "-5") }), "AuthorizationQueryParametersError"},
		{"expires zero", mk(func(q url.Values) { q.Set("X-Amz-Expires", "0") }), "AuthorizationQueryParametersError"},
		{"expires too long", mk(func(q url.Values) { q.Set("X-Amz-Expires", "604801") }), "AuthorizationQueryParametersError"},
		{"expires changed", mk(func(q url.Values) { q.Set("X-Amz-Expires", "7200") }), "SignatureDoesNotMatch"},
		{"bad credential", mk(func(q url.Values) { q.Set("X-Amz-Credential", "x/y") }), "AuthorizationQueryParametersError"},
		{"credential date mismatch", mk(func(q url.Values) {
			q.Set("X-Amz-Credential", testAK+"/20240302/us-east-1/s3/aws4_request")
		}), "AuthorizationQueryParametersError"},
		{"unknown key", mk(func(q url.Values) {
			q.Set("X-Amz-Credential", "NOPE/20240301/us-east-1/s3/aws4_request")
		}), "InvalidAccessKeyId"},
		{"duplicate signature", mk(func(q url.Values) { q.Add("X-Amz-Signature", "x") }), "AuthorizationQueryParametersError"},
		{"security token", mk(func(q url.Values) { q.Set("X-Amz-Security-Token", "t") }), "InvalidToken"},
		{"host not signed", mk(func(q url.Values) { q.Set("X-Amz-SignedHeaders", "x-amz-date") }), "AccessDenied"},
		{"tampered param", mk(func(q url.Values) { q.Set("versionId", "4") }), "SignatureDoesNotMatch"},
		{"bad signature", mk(func(q url.Values) { q.Set("X-Amz-Signature", strings.Repeat("0", 64)) }), "SignatureDoesNotMatch"},
		{"sigv2 mixed", mk(func(q url.Values) { q.Set("AWSAccessKeyId", testAK) }), "InvalidArgument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if o := runAuth(v, tc.r); o.Code != tc.code {
				t.Fatalf("got %q (%s), want %q", o.Code, o.Message, tc.code)
			}
		})
	}
	// Expiry boundary: valid at exactly t+expires, expired one second later.
	if o := runAuth(newTestVerifier(now.Add(time.Hour)), mk(nil)); o.Code != "" {
		t.Fatalf("at expiry: %+v", o)
	}
	if o := runAuth(newTestVerifier(now.Add(time.Hour+time.Second)), mk(nil)); o.Code != "AccessDenied" {
		t.Fatalf("after expiry: %+v", o)
	}
}

func TestPresignedWithContentSHA256(t *testing.T) {
	now := time.Now().UTC()
	body := []byte("hashed presigned body")
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	req, _ := http.NewRequest("PUT", "http://bucket.example.com/obj?X-Amz-Expires=60", bytes.NewReader(body))
	s := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	// Put the hash in the query so it is part of the canonical request.
	q := req.URL.Query()
	q.Set("X-Amz-Content-Sha256", h)
	req.URL.RawQuery = q.Encode()
	u, _, err := s.PresignHTTP(context.Background(), aws.Credentials{AccessKeyID: testAK, SecretAccessKey: testSK}, req, h, "s3", "us-east-1", now)
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	v := liveVerifier()
	r := httptest.NewRequest("PUT", pu.RequestURI(), bytes.NewReader(body))
	r.Host = pu.Host
	if o := runAuth(v, r); o.Code != "" || o.PayloadMode != h {
		t.Fatalf("presigned with hash: %+v", o)
	}
	r = httptest.NewRequest("PUT", pu.RequestURI(), strings.NewReader("HASHED presigned body"))
	r.Host = pu.Host
	if o := runAuth(v, r); o.Code != "XAmzContentSHA256Mismatch" {
		t.Fatalf("presigned with wrong body: %+v", o)
	}
}

func TestAnonymous(t *testing.T) {
	v := liveVerifier()
	r := httptest.NewRequest("PUT", "/b/k", strings.NewReader("data"))
	o := runAuth(v, r)
	if o.Code != "" || !o.Anonymous || o.Method != MethodAnonymous || string(o.Body) != "data" || o.DecodedLen != 4 {
		t.Fatalf("anonymous: %+v", o)
	}
	sum := sha256.Sum256([]byte("data"))
	r = httptest.NewRequest("PUT", "/b/k", strings.NewReader("data"))
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	if o := runAuth(v, r); o.Code != "" {
		t.Fatalf("anonymous with hash: %+v", o)
	}
	r = httptest.NewRequest("PUT", "/b/k", strings.NewReader("DATA"))
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	if o := runAuth(v, r); o.Code != "XAmzContentSHA256Mismatch" || o.Stage != "body" {
		t.Fatalf("anonymous with wrong hash: %+v", o)
	}
	r = httptest.NewRequest("PUT", "/b/k", strings.NewReader("DATA"))
	r.Header.Set("X-Amz-Content-Sha256", PayloadStreamingSigned)
	if o := runAuth(v, r); o.Code != "AccessDenied" {
		t.Fatalf("anonymous signed streaming: %+v", o)
	}
	r = httptest.NewRequest("GET", "/b/k", nil)
	r.Body = nil
	res, err := v.Authenticate(r)
	if err != nil || res.Body == nil || res.Trailer == nil || res.Trailer() == nil {
		t.Fatalf("nil body request: %v %+v", err, res)
	}
	if _, err := v.Authenticate(nil); err == nil {
		t.Fatal("nil request accepted")
	}
}

// TestNilCredentialProvider makes sure a Verifier without credentials
// rejects signed requests cleanly.
func TestNilCredentialProvider(t *testing.T) {
	v := NewVerifier(nil, Options{})
	req, _ := http.NewRequest("GET", "http://h/b", nil)
	if err := SignRequest(req, Credentials{testAK, testSK}, "", "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if o := runAuth(v, toServer(req, nil)); o.Code != "InvalidAccessKeyId" {
		t.Fatalf("got %+v", o)
	}
}

// TestRewrittenURLStillVerifies checks that handlers rewriting r.URL (e.g.
// http.StripPrefix) do not break verification, since the path as sent is
// taken from RequestURI.
func TestRewrittenURLStillVerifies(t *testing.T) {
	v := liveVerifier()
	var got outcome
	h := http.StripPrefix("/_gos3/admin/v1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = runAuth(v, r)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/_gos3/admin/v1/users?limit=10", nil)
	if err := SignRequest(req, Credentials{testAK, testSK}, "us-east-1", "s3", PayloadEmptySHA256, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Code != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestCanonicalURIPath(t *testing.T) {
	tests := map[string]string{
		"":                "/",
		"/":               "/",
		"/b/k":            "/b/k",
		"/b/a%20b":        "/b/a%20b",
		"/b/a%2fb":        "/b/a%2Fb",
		"/b/a=b":          "/b/a%3Db",
		"/b/%41":          "/b/A",
		"/b/a+b":          "/b/a%2Bb",
		"/b/100%":         "/b/100%25",
		"/b/%zz":          "/b/%25zz",
		"/b//c/./../":     "/b//c/./../",
		"/b/%E6%97%A5":    "/b/%E6%97%A5",
		"/b/~_-.":         "/b/~_-.",
		"b":               "/b",
		"/b/$":            "/b/%24",
		"/b/a%2Fb%2fc/d%": "/b/a%2Fb%2Fc/d%25",
	}
	for in, want := range tests {
		if got := canonicalURIPath(in); got != want {
			t.Errorf("canonicalURIPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	tests := map[string]string{
		"":                          "",
		"acl":                       "acl=",
		"b=2&a=1":                   "a=1&b=2",
		"a=2&a=1&a=10":              "a=1&a=10&a=2",
		"prefix=a+b":                "prefix=a%20b",
		"prefix=a%2Bb":              "prefix=a%2Bb",
		"prefix=a%2bb":              "prefix=a%2Bb",
		"x=%7E&y=~":                 "x=~&y=~",
		"k=v/w":                     "k=v%2Fw",
		"&&a=1&&":                   "a=1",
		"X-Amz-Signature=abc&z=1":   "X-Amz-Signature=abc&z=1",
		"uploads&prefix=%E6%97%A5":  "prefix=%E6%97%A5&uploads=",
		"a=b=c":                     "a=b%3Dc",
		"bad=%zz":                   "bad=%25zz",
		"semi=a;b":                  "semi=a%3Bb",
		"Upper=1&lower=2&_u=3&~t=4": "Upper=1&_u=3&lower=2&~t=4",
	}
	for in, want := range tests {
		p, ok := parseRawQuery(in)
		if !ok {
			t.Fatalf("parse %q failed", in)
		}
		if got := canonicalQueryString(p, ""); got != want {
			t.Errorf("canonical(%q) = %q, want %q", in, got, want)
		}
	}
	p, _ := parseRawQuery("X-Amz-Signature=abc&z=1")
	if got := canonicalQueryString(p, "X-Amz-Signature"); got != "z=1" {
		t.Errorf("exclude: %q", got)
	}
	if _, ok := parseRawQuery(strings.Repeat("a=1&", maxQueryParams+1)); ok {
		t.Error("too many params accepted")
	}
}

func TestCanonicalHeaderValue(t *testing.T) {
	tests := map[string]string{
		"  a   b  ":   "a b",
		"a b":         "a b",
		"\ta  b\t":    "a b",
		"":            "",
		"x    y    z": "x y z",
	}
	for in, want := range tests {
		if got := canonicalHeaderValue(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func BenchmarkAuthenticateHeader(b *testing.B) {
	now := time.Now()
	v := newTestVerifier(now)
	req, _ := http.NewRequest("GET", "http://bucket.example.com/some/key.txt?versionId=1", nil)
	_ = SignRequest(req, Credentials{testAK, testSK}, "us-east-1", "s3", PayloadEmptySHA256, now)
	sr := toServer(req, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := v.Authenticate(sr); err != nil {
			b.Fatal(err)
		}
	}
}
