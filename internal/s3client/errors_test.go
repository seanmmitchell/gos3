package s3client_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// TestErrorAnswers checks how error answers are parsed and classified,
// against a scripted service answering per key.
func TestErrorAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		body := func(status int, doc string) {
			h.Set("Content-Type", "application/xml")
			w.WriteHeader(status)
			if r.Method != http.MethodHead {
				io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+doc)
			}
		}
		switch r.URL.Path {
		case "/b/marker":
			h.Set("X-Amz-Delete-Marker", "true")
			h.Set("X-Amz-Version-Id", "v1")
			body(http.StatusMethodNotAllowed, `<Error><Code>MethodNotAllowed</Code></Error>`)
		case "/b/redirect":
			h.Set("X-Amz-Bucket-Region", "eu-west-1")
			body(http.StatusMovedPermanently, `<Error><Code>PermanentRedirect</Code><Endpoint>b.s3.eu-west-1.amazonaws.com</Endpoint></Error>`)
		case "/b/region":
			body(http.StatusBadRequest, `<Error><Code>AuthorizationHeaderMalformed</Code><Message>the region 'us-east-1' is wrong; expecting 'eu-central-1'</Message><Region>eu-central-1</Region></Error>`)
		case "/b/malformed":
			body(http.StatusBadRequest, `<Error><Code>AuthorizationHeaderMalformed</Code><Message>incorrect service</Message></Error>`)
		case "/b/illegal":
			h.Set("X-Amz-Bucket-Region", "ap-south-1")
			body(http.StatusBadRequest, `<Error><Code>IllegalLocationConstraintException</Code><Region>ignored</Region></Error>`)
		case "/b/unsupported":
			body(http.StatusNotImplemented, `<Error><Code>NotImplemented</Code></Error>`)
		case "/b/denied":
			body(http.StatusForbidden, `<Error><Code>AccessDenied</Code></Error>`)
		default:
			body(http.StatusNotFound, `<Error><Code>NoSuchKey</Code></Error>`)
		}
	}))
	defer srv.Close()
	c, err := s3client.New(s3client.Config{Endpoint: srv.URL, AccessKey: "a", SecretKey: "b", PathStyle: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	head := func(key string) error {
		_, err := c.HeadObject(ctx, "b", key)
		return err
	}
	get := func(key string) error {
		_, _, err := c.GetObject(ctx, "b", key, s3client.GetOptions{})
		return err
	}

	for _, tc := range []struct {
		name         string
		err          error
		code, region string
		marker       string // delete marker version ("" = not a marker)
		wrongRegion  bool
		notImpl      bool
		denied       bool
	}{
		{name: "HEAD marker", err: head("marker"), code: "MethodNotAllowed", marker: "v1"},
		{name: "GET marker", err: get("marker"), code: "MethodNotAllowed", marker: "v1"},
		{name: "HEAD redirect", err: head("redirect"), code: "PermanentRedirect", region: "eu-west-1", wrongRegion: true},
		{name: "GET redirect", err: get("redirect"), code: "PermanentRedirect", region: "eu-west-1", wrongRegion: true},
		{name: "GET wrong region", err: get("region"), code: "AuthorizationHeaderMalformed", region: "eu-central-1", wrongRegion: true},
		{name: "GET malformed", err: get("malformed"), code: "AuthorizationHeaderMalformed"},
		{name: "GET illegal constraint", err: get("illegal"), code: "IllegalLocationConstraintException", region: "ap-south-1", wrongRegion: true},
		{name: "HEAD unsupported", err: head("unsupported"), code: "NotImplemented", notImpl: true},
		{name: "GET unsupported", err: get("unsupported"), code: "NotImplemented", notImpl: true},
		{name: "HEAD denied", err: head("denied"), denied: true},
		{name: "GET denied", err: get("denied"), code: "AccessDenied", denied: true},
		{name: "HEAD missing", err: head("missing"), code: "NotFound"},
		{name: "GET missing", err: get("missing"), code: "NoSuchKey"},
	} {
		var e *s3client.Error
		if !errors.As(tc.err, &e) {
			t.Errorf("%s: %v is no *Error", tc.name, tc.err)
			continue
		}
		if e.Code != tc.code || e.Region != tc.region || e.Header == nil {
			t.Errorf("%s: code %q region %q header %v; want %q, %q", tc.name, e.Code, e.Region, e.Header, tc.code, tc.region)
		}
		vid, isMarker := s3client.IsDeleteMarker(tc.err)
		if isMarker != (tc.marker != "") || vid != tc.marker {
			t.Errorf("%s: IsDeleteMarker = %q, %v", tc.name, vid, isMarker)
		}
		region, wrong := s3client.IsWrongRegion(tc.err)
		if wrong != tc.wrongRegion || (wrong && region != tc.region) {
			t.Errorf("%s: IsWrongRegion = %q, %v", tc.name, region, wrong)
		}
		if got := s3client.IsNotImplemented(tc.err); got != tc.notImpl {
			t.Errorf("%s: IsNotImplemented = %v", tc.name, got)
		}
		if got := s3client.IsAccessDenied(tc.err); got != tc.denied {
			t.Errorf("%s: IsAccessDenied = %v", tc.name, got)
		}
	}

	// Transport errors are none of these.
	srv.Close()
	err = head("marker")
	if _, ok := s3client.IsDeleteMarker(err); ok || s3client.IsNotImplemented(err) || s3client.IsAccessDenied(err) {
		t.Errorf("transport error %v classified", err)
	}
	if _, ok := s3client.IsWrongRegion(err); ok {
		t.Errorf("transport error %v is a wrong region", err)
	}
	// A DeleteObjects failure carries the code only.
	de := &s3client.DeleteObjectsError{Failed: []s3client.DeleteFailure{{Key: "k", VersionID: "v", Code: "AccessDenied"}}}
	if !s3client.IsAccessDenied(de) {
		t.Errorf("IsAccessDenied(%v) = false", de)
	}
}
