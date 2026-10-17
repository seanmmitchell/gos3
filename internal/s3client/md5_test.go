package s3client_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

func md5raw(b []byte) []byte {
	s := md5.Sum(b)
	return s[:]
}

// md5Recorder is a remote handler wrapper that, like AWS, rejects the
// requests that need an integrity header when they come without
// Content-MD5, and records the operations that carried a correct one.
type md5Recorder struct {
	mu   sync.Mutex
	seen []string // "<method> <sub-resource>" of requests with a correct Content-MD5
	body []byte   // the last retention document
}

// subresource names the request's sub-resource ("" = none).
func subresource(r *http.Request) string {
	q := r.URL.Query()
	for _, s := range []string{"versioning", "object-lock", "retention", "legal-hold", "tagging", "delete", "partNumber"} {
		if q.Has(s) {
			return s
		}
	}
	return ""
}

func (m *md5Recorder) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := subresource(r)
		needed := sub != "" && sub != "partNumber" && (r.Method == http.MethodPut || r.Method == http.MethodPost)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		got := r.Header.Get("Content-MD5")
		if got == "" && needed {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `<Error><Code>InvalidRequest</Code><Message>Missing required header for this request: Content-MD5</Message></Error>`)
			return
		}
		if got != "" && got == base64.StdEncoding.EncodeToString(md5raw(data)) {
			m.mu.Lock()
			m.seen = append(m.seen, r.Method+" "+sub)
			if sub == "retention" {
				m.body = data
			}
			m.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	})
}

// lastRetention returns the last retention document received.
func (m *md5Recorder) lastRetention() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.body)
}

// take returns and clears the recorded operations.
func (m *md5Recorder) take() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.seen
	m.seen = nil
	return s
}

// TestContentMD5 checks that Content-MD5 is sent where services require
// it (configuration and sub-resource PUTs, DeleteObjects) and where the
// caller asks for it (PutOptions.ContentMD5, UploadOptions.PartMD5).
func TestContentMD5(t *testing.T) {
	var m md5Recorder
	r := s3clienttest.StartWith(t, s3clienttest.Options{Wrap: m.wrap})
	c := r.Client(t)
	ctx := t.Context()
	if err := c.CreateBucketWith(ctx, "bkt", s3client.CreateBucketOptions{ObjectLock: true}); err != nil {
		t.Fatal(err)
	}
	check := func(what string, want ...string) {
		t.Helper()
		if got := m.take(); !slices.Equal(got, want) {
			t.Errorf("%s: requests with Content-MD5 %q, want %q", what, got, want)
		}
	}
	check("CreateBucketWith")

	if err := c.PutBucketVersioning(ctx, "bkt", "Enabled"); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	check("PutBucketVersioning", "PUT versioning")
	if err := c.PutObjectLockConfiguration(ctx, "bkt", s3client.ObjectLockConfig{Enabled: true, Mode: "GOVERNANCE", Days: 1}); err != nil {
		t.Fatalf("PutObjectLockConfiguration: %v", err)
	}
	check("PutObjectLockConfiguration", "PUT object-lock")

	// A plain Put sends none; ContentMD5 and PartMD5 send it.
	data := []byte("content")
	if _, err := c.Put(ctx, "bkt", "plain", bytes.NewReader(data), int64(len(data)), s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	check("Put")
	res, err := c.Put(ctx, "bkt", "k", bytes.NewReader(data), int64(len(data)), s3client.PutOptions{ContentMD5: md5raw(data)})
	if err != nil {
		t.Fatalf("Put with ContentMD5: %v", err)
	}
	check("Put with ContentMD5", "PUT ")
	if _, err := c.Put(ctx, "bkt", "bad", bytes.NewReader(data), int64(len(data)), s3client.PutOptions{ContentMD5: md5raw([]byte("other"))}); !s3client.IsCode(err, "BadDigest") {
		t.Errorf("Put with a wrong ContentMD5: %v, want BadDigest", err)
	}
	m.take()
	if _, err := c.UploadObject(ctx, "bkt", "small", bytes.NewReader(data), int64(len(data)), s3client.UploadOptions{PartMD5: true}); err != nil {
		t.Fatalf("UploadObject (single PUT) with PartMD5: %v", err)
	}
	check("UploadObject (single PUT) with PartMD5", "PUT ")
	big := randBytes(2*s3client.MinPartSize + 10)
	up, err := c.UploadObject(ctx, "bkt", "big", bytes.NewReader(big), int64(len(big)), s3client.UploadOptions{
		PartSize: s3client.MinPartSize, PartMD5: true, Concurrency: 1,
	})
	if err != nil || !up.Multipart {
		t.Fatalf("UploadObject (multipart) with PartMD5: %+v, %v", up, err)
	}
	check("UploadObject (multipart) with PartMD5", "PUT partNumber", "PUT partNumber", "PUT partNumber")

	// Longer than the default retention the object got (1 day).
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second).Add(1500 * time.Microsecond)
	if err := c.PutObjectRetention(ctx, "bkt", "k", res.VersionID, s3client.Retention{Mode: "GOVERNANCE", RetainUntil: until}, false); err != nil {
		t.Fatalf("PutObjectRetention: %v", err)
	}
	check("PutObjectRetention", "PUT retention")
	if want := until.Format("2006-01-02T15:04:05") + ".002Z"; !strings.Contains(m.lastRetention(), "<RetainUntilDate>"+want+"</RetainUntilDate>") {
		t.Errorf("retention document %s, want RetainUntilDate %s", m.lastRetention(), want)
	}
	if err := c.PutObjectLegalHold(ctx, "bkt", "k", res.VersionID, true); err != nil {
		t.Fatalf("PutObjectLegalHold: %v", err)
	}
	check("PutObjectLegalHold", "PUT legal-hold")
	if err := c.PutObjectLegalHold(ctx, "bkt", "k", res.VersionID, false); err != nil {
		t.Fatal(err)
	}
	m.take()
	if err := c.PutObjectTagging(ctx, "bkt", "k", res.VersionID, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("PutObjectTagging: %v", err)
	}
	check("PutObjectTagging", "PUT tagging")
	if _, err := c.DeleteVersions(ctx, "bkt", []s3client.ObjectID{{Key: "plain"}}, false); err != nil {
		t.Fatalf("DeleteVersions: %v", err)
	}
	check("DeleteVersions", "POST delete")
	if err := c.DeleteObjects(ctx, "bkt", []string{"small"}); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	check("DeleteObjects", "POST delete")
}

// TestBypassGovernanceHeader checks that the bypass header is sent exactly
// when asked for.
func TestBypassGovernanceHeader(t *testing.T) {
	var mu sync.Mutex
	var bypassed []string
	r := s3clienttest.StartWith(t, s3clienttest.Options{Wrap: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("X-Amz-Bypass-Governance-Retention") == "true" {
				mu.Lock()
				bypassed = append(bypassed, req.Method+" "+subresource(req))
				mu.Unlock()
			}
			h.ServeHTTP(w, req)
		})
	}})
	c := r.Client(t)
	ctx := t.Context()
	if err := c.CreateBucketWith(ctx, "bkt", s3client.CreateBucketOptions{ObjectLock: true}); err != nil {
		t.Fatal(err)
	}
	v := putString(t, c, "bkt", "k", "x")
	none := s3client.Retention{}
	for _, bypass := range []bool{false, true} {
		_ = c.PutObjectRetention(ctx, "bkt", "k", v.VersionID, none, bypass)
		_, _ = c.DeleteObjectWith(ctx, "bkt", "k", s3client.DeleteOptions{VersionID: "missing", BypassGovernance: bypass})
		_, _ = c.DeleteVersions(ctx, "bkt", []s3client.ObjectID{{Key: "k", VersionID: "missing"}}, bypass)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"PUT retention", "DELETE ", "POST delete"}; !slices.Equal(bypassed, want) {
		t.Errorf("requests with the bypass header %q, want %q", bypassed, want)
	}
}
