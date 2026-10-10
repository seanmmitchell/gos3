package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func TestObjectMetaFromHeaders(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(Options{Backend: memory.New(), Now: func() time.Time { return now }})
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Disposition", `attachment; filename="a.txt"`)
	h.Set("Content-Language", "en")
	h.Set("Cache-Control", "no-cache")
	h.Set("Expires", "Wed, 21 Oct 2015 07:28:00 GMT")
	h.Set("X-Amz-Meta-Color", "blue")
	h.Add("X-Amz-Meta-Multi", "a")
	h.Add("X-Amz-Meta-Multi", "b")
	h.Set("X-Amz-Storage-Class", "STANDARD_IA")
	h.Set("X-Amz-Tagging", "k1=v1&k%202=v%202")
	h.Set("X-Amz-Acl", "public-read")
	h.Set("X-Amz-Server-Side-Encryption", "aws:kms")
	h.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", "key-1")
	h.Set("X-Amz-Object-Lock-Mode", "GOVERNANCE")
	h.Set("X-Amz-Object-Lock-Retain-Until-Date", "2030-01-02T03:04:05.000Z")
	h.Set("X-Amz-Object-Lock-Legal-Hold", "ON")
	h.Set("X-Amz-Website-Redirect-Location", "/other")
	m, err := s.objectMetaFromHeaders(h)
	if err != nil {
		t.Fatal(err)
	}
	if m.ContentType != "text/plain" || m.ContentEncoding != "gzip" || m.ContentDisposition == "" || m.ContentLanguage != "en" ||
		m.CacheControl != "no-cache" || m.Expires == "" || m.UserMeta["color"] != "blue" || m.UserMeta["multi"] != "a,b" ||
		m.StorageClass != "STANDARD_IA" || m.Tags["k1"] != "v1" || m.Tags["k 2"] != "v 2" || m.ACL != "public-read" ||
		m.SSE != "aws:kms" || m.SSEKMSKeyID != "key-1" || m.RetentionMode != "GOVERNANCE" ||
		!m.RetainUntil.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) || !m.LegalHold || m.WebsiteRedirect != "/other" {
		t.Fatalf("%+v", m)
	}
	if err := requireObjectLock(m, storage.BucketInfo{}); codeOf(err) != "InvalidRequest" {
		t.Fatalf("object lock on plain bucket: %v", err)
	}
	if err := requireObjectLock(m, storage.BucketInfo{ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}

	// Defaults: STANDARD and private are stored as "".
	m, err = s.objectMetaFromHeaders(http.Header{"X-Amz-Storage-Class": {"STANDARD"}, "X-Amz-Acl": {"private"}})
	if err != nil || m.StorageClass != "" || m.ACL != "" || m.UserMeta != nil || m.Tags != nil {
		t.Fatalf("%+v %v", m, err)
	}

	for name, tc := range map[string]struct {
		hdr  map[string]string
		code string
	}{
		"meta at limit":       {map[string]string{"X-Amz-Meta-K": strings.Repeat("v", maxUserMetaSize-1)}, ""},
		"meta over limit":     {map[string]string{"X-Amz-Meta-K": strings.Repeat("v", maxUserMetaSize)}, "MetadataTooLarge"},
		"bad storage class":   {map[string]string{"X-Amz-Storage-Class": "FAST"}, "InvalidStorageClass"},
		"bad tagging":         {map[string]string{"X-Amz-Tagging": "a=1&a=2"}, "InvalidArgument"},
		"bad tagging escape":  {map[string]string{"X-Amz-Tagging": "a=%zz"}, "InvalidArgument"},
		"too many tags":       {map[string]string{"X-Amz-Tagging": "a=1&b=1&c=1&d=1&e=1&f=1&g=1&h=1&i=1&j=1&k=1"}, "BadRequest"},
		"aws tag":             {map[string]string{"X-Amz-Tagging": "aws:x=1"}, "InvalidTag"},
		"bad acl":             {map[string]string{"X-Amz-Acl": "log-delivery-write"}, "InvalidArgument"},
		"grant header":        {map[string]string{"X-Amz-Grant-Read": "uri=x"}, "NotImplemented"},
		"bad sse":             {map[string]string{"X-Amz-Server-Side-Encryption": "DES"}, "InvalidArgument"},
		"kms key without kms": {map[string]string{"X-Amz-Server-Side-Encryption": "AES256", "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id": "k"}, "InvalidArgument"},
		"sse-c":               {map[string]string{"X-Amz-Server-Side-Encryption-Customer-Algorithm": "AES256"}, "NotImplemented"},
		"lock mode only":      {map[string]string{"X-Amz-Object-Lock-Mode": "GOVERNANCE"}, "InvalidArgument"},
		"lock date only":      {map[string]string{"X-Amz-Object-Lock-Retain-Until-Date": "2030-01-01T00:00:00Z"}, "InvalidArgument"},
		"lock bad mode":       {map[string]string{"X-Amz-Object-Lock-Mode": "forever", "X-Amz-Object-Lock-Retain-Until-Date": "2030-01-01T00:00:00Z"}, "InvalidArgument"},
		"lock bad date":       {map[string]string{"X-Amz-Object-Lock-Mode": "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date": "soon"}, "InvalidArgument"},
		"lock past date":      {map[string]string{"X-Amz-Object-Lock-Mode": "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date": "2020-01-01T00:00:00Z"}, "InvalidArgument"},
		"bad legal hold":      {map[string]string{"X-Amz-Object-Lock-Legal-Hold": "yes"}, "InvalidArgument"},
		"bad redirect":        {map[string]string{"X-Amz-Website-Redirect-Location": "ftp://x"}, "InvalidArgument"},
		"long redirect":       {map[string]string{"X-Amz-Website-Redirect-Location": "/" + strings.Repeat("a", maxRedirectLength)}, "InvalidArgument"},
	} {
		h := http.Header{}
		for k, v := range tc.hdr {
			h.Set(k, v)
		}
		_, err := s.objectMetaFromHeaders(h)
		if codeOf(err) != tc.code {
			t.Errorf("%s: %v, want %q", name, err, tc.code)
		}
	}
	if err := checkSSECHeaders(http.Header{"X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key": {"x"}}); codeOf(err) != "NotImplemented" {
		t.Fatal("copy-source SSE-C accepted")
	}
}

func TestWriteObjectHeaders(t *testing.T) {
	lm := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	info := storage.ObjectInfo{
		ObjectMeta: storage.ObjectMeta{
			ETag: "abc-2", UserMeta: map[string]string{"a": "1"}, Tags: map[string]string{"t": "v"},
			StorageClass: "GLACIER", SSE: "aws:kms", SSEKMSKeyID: "kid", RetentionMode: "COMPLIANCE",
			RetainUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), LegalHold: true, PartSizes: []int64{5, 5},
			Checksums: map[string]string{ChecksumCRC32: "AAAAAA=="}, ChecksumType: ChecksumTypeComposite,
			WebsiteRedirect: "/x", CacheControl: "max-age=1",
		},
		VersionID: "v1", Size: 10, LastModified: lm,
	}
	rec := httptest.NewRecorder()
	writeObjectHeaders(rec, info, true, true)
	h := rec.Header()
	for k, want := range map[string]string{
		"Content-Length": "10", "ETag": `"abc-2"`, "Last-Modified": "Sat, 03 Feb 2024 04:05:06 GMT",
		"Content-Type": "binary/octet-stream", "X-Amz-Version-Id": "v1",
		"X-Amz-Tagging-Count": "1", "X-Amz-Storage-Class": "GLACIER", "X-Amz-Server-Side-Encryption": "aws:kms",
		"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id": "kid", "X-Amz-Object-Lock-Mode": "COMPLIANCE",
		"X-Amz-Object-Lock-Retain-Until-Date": "2030-01-01T00:00:00.000Z", "X-Amz-Object-Lock-Legal-Hold": "ON",
		"X-Amz-Mp-Parts-Count": "2", "X-Amz-Website-Redirect-Location": "/x", "X-Amz-Checksum-Crc32": "AAAAAA==",
		"X-Amz-Checksum-Type": "COMPOSITE", "Accept-Ranges": "bytes", "Cache-Control": "max-age=1",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// User metadata keeps AWS's lowercase header spelling on the wire:
	// botocore strips "x-amz-meta-" case-insensitively but keeps the rest
	// verbatim, so a canonicalized "X-Amz-Meta-A" would read back as "A".
	if got := h["x-amz-meta-a"]; len(got) != 1 || got[0] != "1" {
		t.Errorf("x-amz-meta-a = %q (header map %v), want lowercase name with value 1", got, h)
	}
	if _, ok := h["X-Amz-Meta-A"]; ok {
		t.Error("user metadata header was canonicalized to X-Amz-Meta-A")
	}
	rec = httptest.NewRecorder()
	info.StorageClass = "STANDARD"
	writeObjectHeaders(rec, info, false, false)
	for _, k := range []string{"X-Amz-Version-Id", "X-Amz-Checksum-Crc32", "X-Amz-Checksum-Type", "X-Amz-Storage-Class"} {
		if v := rec.Header().Get(k); v != "" {
			t.Errorf("%s should be absent, got %q", k, v)
		}
	}
	if !checksumModeEnabled(http.Header{"X-Amz-Checksum-Mode": {"enabled"}}) || checksumModeEnabled(http.Header{}) {
		t.Fatal("checksum mode")
	}
}
