package s3

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// allocDuring returns the bytes allocated while f runs (an upper bound of
// the peak heap growth it causes).
func allocDuring(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// repeatTo returns prefix + unit repeated until the document is at least
// size bytes + suffix.
func repeatTo(prefix, unit, suffix string, size int) []byte {
	n := (size - len(prefix) - len(suffix)) / len(unit)
	return []byte(prefix + strings.Repeat(unit, n+1) + suffix)
}

// worstDeleteBody is the largest legitimate DeleteObjects request (see
// maxDeleteObjectsBody): 1000 keys of 1024 bytes that all need escaping
// (as "&quot;"), each with a version ID and a multipart ETag, pretty
// printed.
func worstDeleteBody() ([]byte, string) {
	key := strings.Repeat(`"`, 1024)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + "\n  <Quiet>true</Quiet>\n")
	for i := 0; i < maxDeleteObjects; i++ {
		fmt.Fprintf(&b, "  <Object>\n    <Key>%s</Key>\n    <VersionId>%032d</VersionId>\n    <ETag>&quot;%032x-10000&quot;</ETag>\n  </Object>\n",
			strings.Repeat("&quot;", 1024), i, i)
	}
	b.WriteString("</Delete>\n")
	return []byte(b.String()), key
}

// worstCompleteBody is the largest legitimate CompleteMultipartUpload
// request (see maxCompleteBodySize): 10000 parts with a quoted ETag and a
// SHA-256 checksum, pretty printed.
func worstCompleteBody() []byte {
	sum := base64.StdEncoding.EncodeToString(make([]byte, 32))
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + "\n")
	for i := 1; i <= maxPartNumber; i++ {
		fmt.Fprintf(&b, "  <Part>\n    <PartNumber>%d</PartNumber>\n    <ETag>&quot;%032x&quot;</ETag>\n    <ChecksumSHA256>%s</ChecksumSHA256>\n  </Part>\n", i, i, sum)
	}
	b.WriteString("</CompleteMultipartUpload>\n")
	return []byte(b.String())
}

func TestParseDeleteRequestValid(t *testing.T) {
	body, key := worstDeleteBody()
	if len(body) > maxDeleteObjectsBody {
		t.Fatalf("worst legitimate body is %d bytes, over the %d limit", len(body), maxDeleteObjectsBody)
	}
	cr := &countingReader{r: bytes.NewReader(body)}
	req, err := parseDeleteRequest(newXMLStream(cr, maxDeleteObjectsBody, maxDeleteObjectsElements))
	if err != nil {
		t.Fatal(err)
	}
	if !req.Quiet || len(req.Objects) != maxDeleteObjects || cr.n != int64(len(body)) {
		t.Fatalf("quiet=%v objects=%d read=%d", req.Quiet, len(req.Objects), cr.n)
	}
	o := req.Objects[999]
	if o.Key != key || o.VersionID != fmt.Sprintf("%032d", 999) || o.ETag != fmt.Sprintf(`"%032x-10000"`, 999) {
		t.Fatalf("last entry %q %q %q", o.Key[:10], o.VersionID, o.ETag)
	}

	// encoding/xml semantics are kept: namespaces, CDATA, comments,
	// unknown elements, nested text skipped, last repeated field wins,
	// trailing data after the root ignored.
	doc := `<?xml version="1.0"?><!-- c --><s3:Delete xmlns:s3="urn:x"><Quiet> 1 </Quiet><Extra><Object><Key>no</Key></Object></Extra>` +
		`<Object><Key>a</Key><Key>b<![CDATA[<c>]]><i>x</i></Key><Unknown/></Object><Object><Key> sp </Key><VersionId>v</VersionId></Object></s3:Delete> trailing`
	req, err = parseDeleteRequest(newXMLStream(strings.NewReader(doc), maxDeleteObjectsBody, maxDeleteObjectsElements))
	if err != nil || !req.Quiet || len(req.Objects) != 2 || req.Objects[0].Key != "b<c>" || req.Objects[1].Key != " sp " || req.Objects[1].VersionID != "v" {
		t.Fatalf("%+v %v", req, err)
	}
	for _, tc := range []struct{ doc, code string }{
		{"", "MissingRequestBodyError"},
		{" \n\t ", "MissingRequestBodyError"},
		{"<?xml version=\"1.0\"?>", "MalformedXML"},
		{"<Remove/>", "MalformedXML"},
		{"<Delete><Quiet>maybe</Quiet></Delete>", "MalformedXML"},
		{"<Delete><Object><Key>a</Key>", "MalformedXML"},
		{"<Delete><Object><Key>a</Kay></Object></Delete>", "MalformedXML"},
	} {
		_, err := parseDeleteRequest(newXMLStream(strings.NewReader(tc.doc), maxDeleteObjectsBody, maxDeleteObjectsElements))
		var se *s3err.Error
		if !errors.As(err, &se) || se.Code != tc.code {
			t.Errorf("%q: %v, want %s", tc.doc, err, tc.code)
		}
	}
}

// TestParseDeleteRequestLimits: documents over the limits are rejected
// while they are parsed, after reading little more than the offending
// part, with bounded allocations.
func TestParseDeleteRequestLimits(t *testing.T) {
	size := maxDeleteObjectsBody
	for _, tc := range []struct {
		name string
		body []byte
		code string
		read int64 // most bytes read
	}{
		{"1001 objects", repeatTo("<Delete>", "<Object><Key>k</Key></Object>", "</Delete>", 2*size), "MalformedXML", 64 << 10},
		{"empty objects", repeatTo("<Delete>", "<Object/>", "</Delete>", size), "MalformedXML", 32 << 10},
		{"unknown elements", repeatTo("<Delete>", "<x/>", "</Delete>", size), "MalformedXML", 128 << 10},
		{"repeated Quiet", repeatTo("<Delete>", "<Quiet/>", "</Delete>", size), "MalformedXML", 256 << 10},
		{"comments", repeatTo("<Delete>", "<!---->", "</Delete>", size), "MalformedXML", 256 << 10},
		{"deep nesting", repeatTo("<Delete><Object>", "<a>", "", size), "MalformedXML", 8 << 10},
		{"attributes", repeatTo("<Delete><Object", ` a=""`, ">", size), "MalformedXML", 24 << 10},
		{"long text", repeatTo("<Delete><Object><Key>", "a", "</Key></Object></Delete>", size), "MalformedXML", 24 << 10},
		{"too large", repeatTo("<Delete><Object><Key>k</Key></Object>", "<!--"+strings.Repeat("x", 8000)+"-->", "</Delete>", 2*size), "MaxMessageLengthExceeded", int64(size) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := &countingReader{r: bytes.NewReader(tc.body)}
			var err error
			alloc := allocDuring(func() {
				_, err = parseDeleteRequest(newXMLStream(cr, maxDeleteObjectsBody, maxDeleteObjectsElements))
			})
			var se *s3err.Error
			if !errors.As(err, &se) || se.Code != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			t.Logf("read %d of %d bytes, allocated %d KiB", cr.n, len(tc.body), alloc>>10)
			if cr.n > tc.read {
				t.Errorf("read %d of %d bytes, want at most %d", cr.n, len(tc.body), tc.read)
			}
			// encoding/xml allocated hundreds of MiB for such bodies.
			if alloc > 16<<20 {
				t.Errorf("allocated %d MiB", alloc>>20)
			}
		})
	}
}

func TestParseCompleteRequest(t *testing.T) {
	body := worstCompleteBody()
	if len(body) > maxCompleteBodySize {
		t.Fatalf("worst legitimate body is %d bytes, over the %d limit", len(body), maxCompleteBodySize)
	}
	var req completeMultipartUpload
	var err error
	alloc := allocDuring(func() {
		req, err = parseCompleteRequest(newXMLStream(bytes.NewReader(body), maxCompleteBodySize, maxCompleteElements), "u")
	})
	if err != nil {
		t.Fatal(err)
	}
	if nums, err := parseCompleteParts(&req, "u"); err != nil || len(nums) != maxPartNumber || req.Parts[9999].ChecksumSHA256 == "" {
		t.Fatalf("%d parts: %v", len(nums), err)
	}
	t.Logf("largest valid body (%d bytes): allocated %d KiB", len(body), alloc>>10)
	if alloc > 32<<20 {
		t.Errorf("valid body: allocated %d MiB", alloc>>20)
	}

	var many strings.Builder
	many.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= maxPartNumber+1; i++ {
		fmt.Fprintf(&many, "<Part><PartNumber>%d</PartNumber><ETag>e</ETag></Part>", i)
	}
	prefix := many.String()
	for _, tc := range []struct {
		name string
		body []byte
		code string
		read int64
	}{
		{"10001 parts", repeatTo(prefix, "<Part><PartNumber>1</PartNumber><ETag>e</ETag></Part>", "</CompleteMultipartUpload>", 2*maxCompleteBodySize), "InvalidArgument", int64(len(prefix)) + 8<<10},
		{"10001 unordered parts", repeatTo("<CompleteMultipartUpload>", "<Part><PartNumber>1</PartNumber><ETag>e</ETag></Part>", "</CompleteMultipartUpload>", maxCompleteBodySize), "InvalidPartOrder", 600 << 10},
		{"empty parts", repeatTo("<CompleteMultipartUpload>", "<Part/>", "</CompleteMultipartUpload>", maxCompleteBodySize), "MalformedXML", 128 << 10},
		{"unknown elements", repeatTo("<CompleteMultipartUpload>", "<x/>", "</CompleteMultipartUpload>", maxCompleteBodySize), "MalformedXML", 2 << 20},
		{"deep nesting", repeatTo("<CompleteMultipartUpload><Part>", "<a>", "", maxCompleteBodySize), "MalformedXML", 8 << 10},
		{"too large", repeatTo("<CompleteMultipartUpload>", "<!--"+strings.Repeat("x", 8000)+"-->", "</CompleteMultipartUpload>", 2*maxCompleteBodySize), "MaxMessageLengthExceeded", maxCompleteBodySize + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := &countingReader{r: bytes.NewReader(tc.body)}
			var err error
			alloc := allocDuring(func() {
				_, err = parseCompleteRequest(newXMLStream(cr, maxCompleteBodySize, maxCompleteElements), "u")
			})
			var se *s3err.Error
			if !errors.As(err, &se) || se.Code != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			t.Logf("read %d of %d bytes, allocated %d KiB", cr.n, len(tc.body), alloc>>10)
			if cr.n > tc.read {
				t.Errorf("read %d of %d bytes, want at most %d", cr.n, len(tc.body), tc.read)
			}
			// encoding/xml allocated about 400 MiB for such bodies; the
			// largest legitimate request takes about 16 MiB.
			if alloc > 32<<20 {
				t.Errorf("allocated %d MiB", alloc>>20)
			}
		})
	}
}

// TestDeleteObjectsBodyLimits (HTTP): an anonymous DeleteObjects with a
// body full of entries no longer makes the server allocate per entry
// before rejecting it, and the largest legitimate request still works.
func TestDeleteObjectsBodyLimits(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	flood := repeatTo("<Delete>", "<Object/>", "</Delete>", maxDeleteObjectsBody-64)
	var resp *testResponse
	alloc := allocDuring(func() { resp = e.anon("POST", "/bkt?delete", flood, nil) })
	objExpect(t, resp, 400, "MalformedXML")
	if alloc > 32<<20 {
		t.Errorf("anonymous DeleteObjects allocated %d MiB", alloc>>20)
	}
	objExpect(t, e.anon("POST", "/bkt?delete", repeatTo("<Delete>", " ", "", maxDeleteObjectsBody+1), nil), 400, "MaxMessageLengthExceeded")

	body, key := worstDeleteBody()
	resp = e.signed("POST", "/bkt?delete", body, objHdr("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum16(body))))
	objExpect(t, resp, 200, "")
	var res deleteResult
	if err := xml.Unmarshal(resp.Data, &res); err != nil {
		t.Fatal(err)
	}
	// Quiet: only failures are listed; the ETag conditions on missing
	// versions all fail.
	if len(res.Errors) != maxDeleteObjects || res.Errors[999].Key != key {
		t.Fatalf("%d error entries: %.200s", len(res.Errors), resp.Data)
	}
	// A Content-MD5 mismatch is still reported for a valid document.
	objExpect(t, e.signed("POST", "/bkt?delete", body, objHdr("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum16(nil)))), 400, "BadDigest")
}

// TestCompleteMultipartUploadBodyLimits (HTTP): too many parts and too
// large bodies are rejected without decoding them in full.
func TestCompleteMultipartUploadBodyLimits(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("mp")
	id := mpRawCreate(t, e, "mp", "k", nil)
	path := mpPath("mp", "k", mpQuery("uploadId", id))
	flood := repeatTo("<CompleteMultipartUpload>", "<Part/>", "</CompleteMultipartUpload>", maxCompleteBodySize-64)
	var resp *testResponse
	alloc := allocDuring(func() { resp = e.signed("POST", path, flood, nil) })
	objExpect(t, resp, 400, "MalformedXML")
	if alloc > 32<<20 {
		t.Errorf("CompleteMultipartUpload allocated %d MiB", alloc>>20)
	}
	objExpect(t, e.signed("POST", path, repeatTo("<CompleteMultipartUpload>", " ", "", maxCompleteBodySize+1), nil), 400, "MaxMessageLengthExceeded")

	var many strings.Builder
	many.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= maxPartNumber+1; i++ {
		fmt.Fprintf(&many, "<Part><PartNumber>%d</PartNumber><ETag>e</ETag></Part>", i)
	}
	many.WriteString("</CompleteMultipartUpload>")
	objExpect(t, e.signed("POST", path, []byte(many.String()), nil), 400, "InvalidArgument")

	// The upload is intact and completes normally.
	part := e.signed("PUT", mpPath("mp", "k", mpQuery("uploadId", id, "partNumber", "1")), []byte("data"), nil)
	objExpect(t, part, 200, "")
	resp = e.signed("POST", path, mpCompleteBody("1", part.Header.Get("ETag")), nil)
	objExpect(t, resp, 200, "")
	if _, err := e.backend.GetObjectInfo(t.Context(), "mp", "k", ""); err != nil {
		t.Fatal(err)
	}
}
