package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

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
	// Stub handler => NotImplemented with a full XML error body.
	resp := e.signed("GET", "/nosuchbucket?list-type=2", nil, nil)
	checkCommonHeaders(t, resp.Header)
	if resp.StatusCode != 501 || resp.code() != "NotImplemented" {
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
	if resp.StatusCode != 501 || len(resp.Data) != 0 {
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

func TestClientInfo(t *testing.T) {
	s := New(Options{Backend: memory.New()})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	if ip, sec := s.clientInfo(r); ip != "10.1.2.3" || sec {
		t.Fatalf("untrusted: %s %v", ip, sec)
	}
	s.trustProxy = true
	if ip, sec := s.clientInfo(r); ip != "203.0.113.9" || !sec {
		t.Fatalf("trusted: %s %v", ip, sec)
	}
	r.Header.Set("X-Forwarded-For", "garbage")
	if ip, _ := s.clientInfo(r); ip != "10.1.2.3" {
		t.Fatalf("garbage xff: %s", ip)
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
