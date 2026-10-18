package s3

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

var requestIDRe = regexp.MustCompile(`^[0-9A-F]{16}$`)

func checkCommonHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if id := h.Get("X-Amz-Request-Id"); !requestIDRe.MatchString(id) {
		t.Errorf("request id %q", id)
	}
	if h.Get("X-Amz-Id-2") == "" || h.Get("Date") == "" {
		t.Errorf("missing id-2/date: %v", h)
	}
	if s := h.Get("Server"); !strings.HasPrefix(s, "gos3") {
		t.Errorf("server %q", s)
	}
}

func TestCommonHeadersAndErrorBody(t *testing.T) {
	e := newTestEnv(t)
	// Missing bucket => NoSuchBucket with a full XML error body.
	resp := e.signed("GET", "/nosuchbucket?list-type=2", nil, nil)
	checkCommonHeaders(t, resp.Header)
	if resp.StatusCode != 404 || resp.code() != "NoSuchBucket" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	if !strings.HasPrefix(string(resp.Data), `<?xml version="1.0" encoding="UTF-8"?>`) || resp.Header.Get("Content-Type") != "application/xml" {
		t.Fatalf("body %s / %s", resp.Data, resp.Header.Get("Content-Type"))
	}
	var doc struct {
		Code, Message, Resource, RequestId, HostId string
	}
	if err := xml.Unmarshal(resp.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.RequestId != resp.Header.Get("X-Amz-Request-Id") || doc.HostId != resp.Header.Get("X-Amz-Id-2") || doc.Resource != "/nosuchbucket" || doc.Message == "" {
		t.Fatalf("doc %+v", doc)
	}
	// Distinct request IDs.
	resp2 := e.signed("GET", "/", nil, nil)
	if resp2.Header.Get("X-Amz-Request-Id") == resp.Header.Get("X-Amz-Request-Id") {
		t.Fatal("request IDs repeat")
	}
	if resp2.Header.Get("Server") != "gos3/test" {
		t.Fatalf("server header %q", resp2.Header.Get("Server"))
	}
	// HEAD errors have no body.
	resp = e.signed("HEAD", "/bkt/key", nil, nil)
	if resp.StatusCode != 404 || len(resp.Data) != 0 {
		t.Fatalf("HEAD: %d %q", resp.StatusCode, resp.Data)
	}
	checkCommonHeaders(t, resp.Header)
	// Authentication failures render too.
	resp = e.signedAs(authCreds("GOS3TESTROOTKEY", "wrong"), "GET", "/", nil, nil)
	if resp.StatusCode != 403 || resp.code() != "SignatureDoesNotMatch" {
		t.Fatalf("bad sig: %d %s", resp.StatusCode, resp.Data)
	}
	resp = e.signed("PATCH", "/bkt", nil, nil)
	if resp.StatusCode != 405 || resp.code() != "MethodNotAllowed" || !strings.Contains(string(resp.Data), "<Method>PATCH</Method>") {
		t.Fatalf("405: %d %s", resp.StatusCode, resp.Data)
	}
}

func TestErrorExtrasAndHeaders(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.override("GetObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		w.Header().Set("Content-Length", "12345") // must not leak into the error
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("X-Amz-Delete-Marker", "true")
		switch rc.key {
		case "nokey":
			return storage.ErrObjectNotFound
		case "nover":
			return fmt.Errorf("wrapped: %w", storage.ErrVersionNotFound)
		case "304":
			return s3err.ErrNotModified
		case "range":
			_, err := parseRange("bytes=100-", 10)
			return err
		case "internal":
			return errors.New("disk on fire")
		}
		return s3err.ErrNoSuchBucket
	})
	resp := e.signed("GET", "/bkt/nokey", nil, nil)
	if resp.StatusCode != 404 || resp.code() != "NoSuchKey" || !strings.Contains(string(resp.Data), "<Key>nokey</Key>") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	if resp.Header.Get("X-Amz-Delete-Marker") != "true" || resp.Header.Get("ETag") != "" {
		t.Fatalf("headers %v", resp.Header)
	}
	if int(resp.ContentLength) != len(resp.Data) {
		t.Fatalf("content-length %d vs %d", resp.ContentLength, len(resp.Data))
	}
	resp = e.signed("GET", "/bkt/nover?versionId=v9", nil, nil)
	if resp.code() != "NoSuchVersion" || !strings.Contains(string(resp.Data), "<VersionId>v9</VersionId>") || !strings.Contains(string(resp.Data), "<Key>nover</Key>") {
		t.Fatalf("%s", resp.Data)
	}
	resp = e.signed("GET", "/bkt/304", nil, nil)
	if resp.StatusCode != 304 || len(resp.Data) != 0 || resp.Header.Get("ETag") != `"abc"` {
		t.Fatalf("304: %d %q %v", resp.StatusCode, resp.Data, resp.Header)
	}
	resp = e.signed("GET", "/bkt/range", nil, nil)
	if resp.StatusCode != 416 || resp.code() != "InvalidRange" || resp.Header.Get("Content-Range") != "bytes */10" ||
		!strings.Contains(string(resp.Data), "<ActualObjectSize>10</ActualObjectSize>") {
		t.Fatalf("416: %d %s %v", resp.StatusCode, resp.Data, resp.Header)
	}
	resp = e.signed("GET", "/bkt/internal", nil, nil)
	if resp.StatusCode != 500 || resp.code() != "InternalError" || strings.Contains(string(resp.Data), "fire") {
		t.Fatalf("500: %d %s", resp.StatusCode, resp.Data)
	}
	if !strings.Contains(e.logs.String(), "disk on fire") {
		t.Fatal("internal error not logged")
	}
	resp = e.signed("GET", "/bkt/other", nil, nil)
	if resp.code() != "NoSuchBucket" || !strings.Contains(string(resp.Data), "<BucketName>bkt</BucketName>") {
		t.Fatalf("%s", resp.Data)
	}
}

func TestPanicRecoveryAndMetrics(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.override("GetObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		panic("boom")
	})
	e.override("PutObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		data, err := io.ReadAll(rc.body)
		if err != nil {
			return err
		}
		writeBody(w, 200, "text/plain", []byte(strings.ToUpper(string(data))))
		return nil
	})
	resp := e.signed("GET", "/bkt/k", nil, nil)
	if resp.StatusCode != 500 || resp.code() != "InternalError" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	checkCommonHeaders(t, resp.Header)
	if !strings.Contains(e.logs.String(), "panic serving request") {
		t.Fatal("panic not logged")
	}
	resp = e.signed("PUT", "/bkt/k", []byte("hello"), nil)
	if resp.StatusCode != 200 || string(resp.Data) != "HELLO" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	snap := e.metrics.Snapshot()
	byOp := map[string]int{}
	for _, o := range snap.Ops {
		byOp[o.Op] = int(o.Requests)
		switch o.Op {
		case "GetObject":
			if o.Errors5xx != 1 {
				t.Errorf("GetObject %+v", o)
			}
		case "PutObject":
			if o.BytesIn != 5 || o.BytesOut != 5 {
				t.Errorf("PutObject %+v", o)
			}
		}
	}
	if byOp["GetObject"] != 1 || byOp["PutObject"] != 1 || snap.InFlight != 0 {
		t.Fatalf("metrics %+v", snap)
	}
	// Access log line at info level (accessLog=true) with the op and status.
	logs := e.logs.String()
	if !strings.Contains(logs, `"msg":"s3 request"`) || !strings.Contains(logs, `"op":"PutObject"`) || !strings.Contains(logs, `"status":500`) ||
		!strings.Contains(logs, `"level":"INFO"`) || !strings.Contains(logs, `"user":"root"`) {
		t.Fatalf("access log:\n%s", logs)
	}
}

func TestPanicAfterResponseStartedAborts(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.override("GetObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("partial"))
		panic("mid-stream")
	})
	req := e.newRequest("GET", "/bkt/k", nil, nil)
	if err := signRoot(req); err != nil {
		t.Fatal(err)
	}
	resp, err := e.httpc.Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("truncated response was not aborted")
	}
}

func TestHandlerErrorAfterWrite(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.override("GetObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		writeBody(w, 200, "text/plain", []byte("ok"))
		return s3err.ErrInternalError
	})
	resp := e.signed("GET", "/bkt/k", nil, nil)
	if resp.StatusCode != 200 || string(resp.Data) != "ok" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	if !strings.Contains(e.logs.String(), "error after response started") {
		t.Fatal("not logged")
	}
}

func TestStripAWSChunked(t *testing.T) {
	for in, want := range map[string]string{
		"aws-chunked":       "",
		"aws-chunked,gzip":  "gzip",
		"gzip, AWS-Chunked": "gzip",
		"gzip":              "gzip",
		"gzip,br":           "gzip,br",
	} {
		h := http.Header{"Content-Encoding": {in}}
		stripAWSChunked(h)
		if got := h.Get("Content-Encoding"); got != want {
			t.Errorf("%q => %q, want %q", in, got, want)
		}
	}
	h := http.Header{}
	stripAWSChunked(h)
	if len(h) != 0 {
		t.Fatal(h)
	}
}

func TestNewDefaults(t *testing.T) {
	s := New(Options{Backend: memory.New()})
	if s.MaxObjectSize() != DefaultMaxObjectSize || s.region() != "us-east-1" || s.serverHeader != "gos3" {
		t.Fatalf("%d %s %s", s.MaxObjectSize(), s.region(), s.serverHeader)
	}
	// Without IAM every signed request is rejected and anonymous ones denied.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 403 {
		t.Fatalf("anonymous without IAM: %d %s", rec.Code, rec.Body)
	}
	if h := NewHandler(Options{Backend: memory.New()}); h == nil {
		t.Fatal("nil handler")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("missing backend accepted")
		}
	}()
	New(Options{})
}

func TestMapStorageError(t *testing.T) {
	for _, tc := range []struct {
		in   error
		want string
	}{
		{storage.ErrBucketNotFound, "NoSuchBucket"},
		{storage.ErrBucketExists, "BucketAlreadyOwnedByYou"},
		{storage.ErrBucketNotEmpty, "BucketNotEmpty"},
		{fmt.Errorf("x: %w", storage.ErrObjectNotFound), "NoSuchKey"},
		{storage.ErrVersionNotFound, "NoSuchVersion"},
		{storage.ErrUploadNotFound, "NoSuchUpload"},
		{storage.ErrPartNotFound, "InvalidPart"},
		{storage.ErrPreconditionFailed, "PreconditionFailed"},
		{fmt.Errorf("x: %w", storage.ErrConflict), "SlowDown"},
		{storage.ErrInvalidName, "InvalidArgument"},
		{context.Canceled, "RequestTimeout"},
		{&http.MaxBytesError{Limit: 1}, "MaxMessageLengthExceeded"},
		{io.ErrUnexpectedEOF, "IncompleteBody"},
		{s3err.ErrSlowDown, "SlowDown"},
	} {
		if got := s3err.As(mapStorageError(tc.in)).Code; got != tc.want {
			t.Errorf("%v => %s, want %s", tc.in, got, tc.want)
		}
	}
	other := errors.New("other")
	if mapStorageError(other) != other || mapStorageError(nil) != nil {
		t.Fatal("passthrough")
	}
	if mapStorageError(storage.ErrConfigNotFound) != storage.ErrConfigNotFound {
		t.Fatal("ErrConfigNotFound must not be mapped generically")
	}
}

func TestWriteXML(t *testing.T) {
	rec := httptest.NewRecorder()
	type doc struct {
		XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Thing"`
		Name    string   `xml:"Name"`
	}
	if err := writeXML(rec, 200, doc{Name: "a<b"}); err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<Thing xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>a&lt;b</Name></Thing>`
	if rec.Body.String() != want || rec.Header().Get("Content-Length") != fmt.Sprint(len(want)) {
		t.Fatalf("%q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	if err := writeXML(rec, 200, map[string]string{}); err == nil || rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatal("unencodable value must fail before writing")
	}
	rec = httptest.NewRecorder()
	writeNoContent(rec)
	if rec.Code != 204 {
		t.Fatal(rec.Code)
	}
}

func TestErrorDocExtras(t *testing.T) {
	data, err := xml.Marshal(errorDoc{Code: "C", Message: "m", Extra: map[string]string{"Zed": "1", "Alpha": "<&>", "bad name": "x"}, Resource: "/r", RequestID: "ID", HostID: "H"})
	if err != nil {
		t.Fatal(err)
	}
	want := `<Error><Code>C</Code><Message>m</Message><Alpha>&lt;&amp;&gt;</Alpha><Zed>1</Zed><Resource>/r</Resource><RequestId>ID</RequestId><HostId>H</HostId></Error>`
	if string(data) != want {
		t.Fatalf("%s", data)
	}
}

// TestExpectContinueEarlyError checks that a request rejected before its
// body is read gets its response immediately when the client sent
// "Expect: 100-continue" and is waiting for permission to send the body
// (botocore and the AWS CLI do this for uploads). Previously the S3 handler
// replaced the server's own Request.Body, so net/http tried to drain the
// unread body before flushing the response and the client only saw the
// error after its 100-continue timeout (1 s).
func TestExpectContinueEarlyError(t *testing.T) {
	e := newTestEnv(t)
	conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "PUT /no-such-bucket/key HTTP/1.1\r\n" +
		"Host: " + e.srv.Listener.Addr().String() + "\r\n" +
		"Content-Length: 1048576\r\n" +
		"Expect: 100-continue\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	// Never send the body: the response must arrive on its own.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response without the body: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusContinue {
		t.Fatal("server sent 100 Continue for a request it rejects")
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want an error", resp.StatusCode)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("error response took %v; the server waited for the body", d)
	}
}

// TestMissingObjectWithoutListBucket checks AWS's rule for reads of keys
// that do not exist: callers without s3:ListBucket get 403 AccessDenied (so
// they cannot probe for keys), callers with it (and root) get 404.
func TestMissingObjectWithoutListBucket(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("pub")
	e.putObject("pub", "a/b.txt", []byte("x"), storage.ObjectMeta{})
	e.putBucketConfig("pub", storage.ConfigPolicy, publicReadPolicy)
	reader := e.createUser("reader")
	e.putPolicy("getonly", `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject*","Resource":"arn:aws:s3:::pub/*"}]}`)
	getOnly := e.createUser("getonly", "getonly")
	lister := e.createUser("lister", iam.PolicyReadOnly)

	check := func(who string, do func(method, path string) *testResponse, method, path string, status int, code string) {
		t.Helper()
		resp := do(method, path)
		if resp.StatusCode != status || (method != "HEAD" && resp.code() != code) {
			t.Errorf("%s %s %s: %d %s, want %d %s", who, method, path, resp.StatusCode, resp.code(), status, code)
		}
		if resp.Header.Get("X-Amz-Delete-Marker") != "" && status == 403 {
			t.Errorf("%s %s %s: delete marker revealed", who, method, path)
		}
	}
	anon := func(m, p string) *testResponse { return e.anon(m, p, nil, nil) }
	as := func(c auth.Credentials) func(m, p string) *testResponse {
		return func(m, p string) *testResponse { return e.signedAs(c, m, p, nil, nil) }
	}
	for _, m := range []string{"GET", "HEAD"} {
		check("anonymous", anon, m, "/pub/a/b.txt", 200, "")
		check("anonymous", anon, m, "/pub/missing", 403, "AccessDenied")
		check("get-only user", as(getOnly), m, "/pub/missing", 403, "AccessDenied")
		check("user without policies", as(reader), m, "/pub/missing", 403, "AccessDenied")
		check("readonly user", as(lister), m, "/pub/missing", 404, "NoSuchKey")
		check("root", func(m, p string) *testResponse { return e.signed(m, p, nil, nil) }, m, "/pub/missing", 404, "NoSuchKey")
	}
	attrs := func(c auth.Credentials) func(m, p string) *testResponse {
		return func(m, p string) *testResponse {
			return e.signedAs(c, m, p, nil, http.Header{"X-Amz-Object-Attributes": {"ETag"}})
		}
	}
	check("get-only user", attrs(getOnly), "GET", "/pub/missing?attributes", 403, "AccessDenied")
	check("readonly user", attrs(lister), "GET", "/pub/missing?attributes", 404, "NoSuchKey")

	// With s3:ListBucket granted by the bucket policy, anonymous callers get
	// the real 404.
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"},
		{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::pub"}]}`)
	check("anonymous with list", anon, "GET", "/pub/missing", 404, "NoSuchKey")
}
