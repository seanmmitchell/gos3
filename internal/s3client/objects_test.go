package s3client_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

func TestServerDate(t *testing.T) {
	_, c := setup(t)
	ctx := t.Context()
	if _, err := c.PutObject(ctx, "bkt", "k", bytes.NewReader([]byte("x")), 1, s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	near := func(what string, d time.Time) {
		t.Helper()
		if d.IsZero() || d.Location() != time.UTC || time.Since(d).Abs() > time.Minute {
			t.Errorf("%s: Date = %v, want the server's current time in UTC", what, d)
		}
	}
	a, err := c.HeadObject(ctx, "bkt", "k")
	if err != nil {
		t.Fatal(err)
	}
	near("HeadObject", a.Date)
	rc, a, err := c.GetObject(ctx, "bkt", "k", s3client.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	near("GetObject", a.Date)
	page, err := c.ListObjectsV2(ctx, "bkt", s3client.ListInput{})
	if err != nil || len(page.Objects) != 1 || !page.Objects[0].Date.IsZero() {
		t.Errorf("listing: %+v, %v; want one object without Date", page.Objects, err)
	}
}

func TestDeleteObjects(t *testing.T) {
	r, c := setup(t)
	ctx := t.Context()
	var keys []string
	for i := range 2100 {
		keys = append(keys, fmt.Sprintf("d/%04d", i))
	}
	keys = append(keys, "d/space & <xml>", "d/ünï\tcödé")
	for _, k := range keys {
		if _, err := c.PutObject(ctx, "bkt", k, bytes.NewReader(nil), 0, s3client.PutOptions{}); err != nil {
			t.Fatalf("PutObject %q: %v", k, err)
		}
	}
	if _, err := c.PutObject(ctx, "bkt", "keep", bytes.NewReader(nil), 0, s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	var batches, singles atomic.Int64
	count := func(req *http.Request) int {
		switch {
		case req.Method == http.MethodPost && req.URL.Query().Has("delete"):
			batches.Add(1)
		case req.Method == http.MethodDelete:
			singles.Add(1)
		}
		return 0
	}
	r.Fail.Store(&count)
	// Missing keys are fine.
	if err := c.DeleteObjects(ctx, "bkt", append(keys, "d/missing")); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if batches.Load() != 3 || singles.Load() != 0 {
		t.Errorf("sent %d batches and %d single deletes, want 3 and 0", batches.Load(), singles.Load())
	}
	page, err := c.ListObjectsV2(ctx, "bkt", s3client.ListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != "keep" {
		t.Fatalf("left %d objects (first %+v), want only keep", len(page.Objects), page.Objects[:min(1, len(page.Objects))])
	}

	if err := c.DeleteObjects(ctx, "bkt", nil); err != nil || batches.Load() != 3 {
		t.Fatalf("DeleteObjects(nil): %v, %d batches", err, batches.Load())
	}
	if err := c.DeleteObjects(ctx, "bkt", []string{""}); err == nil {
		t.Fatal("DeleteObjects with an empty key succeeded")
	}

	// A failed request is returned as is.
	deny := func(req *http.Request) int {
		if req.Method == http.MethodPost {
			return http.StatusForbidden
		}
		return 0
	}
	r.Fail.Store(&deny)
	var e *s3client.Error
	if err := c.DeleteObjects(ctx, "bkt", []string{"keep"}); !errors.As(err, &e) || e.StatusCode != http.StatusForbidden {
		t.Fatalf("DeleteObjects against a failing service: %v, want 403", err)
	}
}

// TestDeleteObjectsRequest checks the wire request and the per-key error
// report against a scripted service.
func TestDeleteObjectsRequest(t *testing.T) {
	var got struct {
		XMLName xml.Name
		Quiet   bool `xml:"Quiet"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	var single atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			p := r.URL.Path
			single.Store(&p)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		data, _ := io.ReadAll(r.Body)
		sum := md5.Sum(data)
		if r.Method != http.MethodPost || !r.URL.Query().Has("delete") ||
			r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := xml.Unmarshal(data, &got); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<Error><Key>a</Key><Code>NoSuchKey</Code><Message>gone</Message></Error>
<Error><Key>b &amp; c</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error>
</DeleteResult>`)
	}))
	defer srv.Close()
	c, err := s3client.New(s3client.Config{Endpoint: srv.URL, AccessKey: "a", SecretKey: "b", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	// A key XML cannot carry is deleted on its own.
	err = c.DeleteObjects(t.Context(), "bkt", []string{"a", "b & c", "ctl\x01key", "d"})
	if p := single.Load(); p == nil || *p != "/bkt/ctl\x01key" {
		t.Errorf("single delete of %v, want /bkt/ctl\\x01key", p)
	}
	if got.XMLName.Local != "Delete" || !got.Quiet || len(got.Objects) != 3 || got.Objects[1].Key != "b & c" {
		t.Errorf("request body: %+v", got)
	}
	var de *s3client.DeleteObjectsError
	if !errors.As(err, &de) || len(de.Failed) != 1 ||
		de.Failed[0] != (s3client.DeleteFailure{Key: "b & c", Code: "AccessDenied", Message: "Access Denied"}) {
		t.Fatalf("DeleteObjects: %#v, want one AccessDenied failure", err)
	}
	if !s3client.IsCode(err, "AccessDenied") || !strings.Contains(err.Error(), `"b & c": AccessDenied`) {
		t.Errorf("error %q does not report the failed key", err)
	}
}

// TestConditionalPutRetryAmbiguity shows why conditional writes that fail
// with a 5xx must be resolved by reading back: a retry after a 500 for an
// applied write fails with 412 against itself. NoRetry reports the 500.
func TestConditionalPutRetryAmbiguity(t *testing.T) {
	r, c := setup(t)
	ctx := t.Context()
	var puts atomic.Int64
	after := func(req *http.Request) s3clienttest.Fault {
		if req.Method == http.MethodPut && puts.Add(1) == 1 {
			return s3clienttest.Status500
		}
		return s3clienttest.Pass
	}
	r.After.Store(&after)
	_, err := c.PutObject(ctx, "bkt", "k1", bytes.NewReader([]byte("v")), 1, s3client.PutOptions{IfNoneMatch: true})
	if !s3client.IsPreconditionFailed(err) || puts.Load() != 2 {
		t.Fatalf("retried conditional PUT: %v after %d attempts, want 412 after 2", err, puts.Load())
	}

	puts.Store(0)
	_, err = c.PutObject(ctx, "bkt", "k2", bytes.NewReader([]byte("v")), 1, s3client.PutOptions{IfNoneMatch: true, NoRetry: true})
	if !s3client.IsUnavailable(err) || puts.Load() != 1 {
		t.Fatalf("NoRetry conditional PUT: %v after %d attempts, want the 500 after 1", err, puts.Load())
	}
	// Both writes took effect.
	for _, k := range []string{"k1", "k2"} {
		if _, err := c.HeadObject(ctx, "bkt", k); err != nil {
			t.Errorf("HeadObject %s: %v", k, err)
		}
	}
}
