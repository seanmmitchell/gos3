package s3

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestExpirationHeader: PutObject, GetObject and HeadObject report
// x-amz-expiration for objects matched by an enabled Expiration rule
// (regression: the header was never sent).
func TestExpirationHeader(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.putBucketConfig("bkt", storage.ConfigLifecycle, `<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
		`<Rule><ID>tmp rule</ID><Filter><Prefix>tmp/</Prefix></Filter><Status>Enabled</Status><Expiration><Date>2030-01-02T00:00:00.000Z</Date></Expiration></Rule>`+
		`<Rule><ID>off</ID><Filter><Prefix>off/</Prefix></Filter><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule>`+
		`</LifecycleConfiguration>`)
	const want = `expiry-date="Wed, 02 Jan 2030 00:00:00 GMT", rule-id="tmp%20rule"`

	put := e.signed("PUT", "/bkt/tmp/a", []byte("x"), nil)
	if put.StatusCode != 200 || put.Header.Get("X-Amz-Expiration") != want {
		t.Fatalf("PUT: %d x-amz-expiration %q", put.StatusCode, put.Header.Get("X-Amz-Expiration"))
	}
	for _, m := range []string{"GET", "HEAD"} {
		r := e.signed(m, "/bkt/tmp/a", nil, nil)
		if r.StatusCode != 200 || r.Header.Get("X-Amz-Expiration") != want {
			t.Errorf("%s: %d x-amz-expiration %q", m, r.StatusCode, r.Header.Get("X-Amz-Expiration"))
		}
	}
	// No matching or only disabled rules: no header.
	for _, key := range []string{"other", "off/a"} {
		e.signed("PUT", "/bkt/"+key, []byte("x"), nil)
		if r := e.signed("HEAD", "/bkt/"+key, nil, nil); r.Header.Get("X-Amz-Expiration") != "" {
			t.Errorf("%s: unexpected x-amz-expiration %q", key, r.Header.Get("X-Amz-Expiration"))
		}
	}
	// A corrupt stored document never fails the request.
	e.putBucketConfig("bkt", storage.ConfigLifecycle, "<garbage")
	if r := e.signed("GET", "/bkt/tmp/a", nil, nil); r.StatusCode != 200 || r.Header.Get("X-Amz-Expiration") != "" || !strings.EqualFold(string(r.Data), "x") {
		t.Errorf("corrupt lifecycle: %d %q", r.StatusCode, r.Header.Get("X-Amz-Expiration"))
	}
}

// TestAbortDateHeaders: CreateMultipartUpload and ListParts report
// x-amz-abort-date / x-amz-abort-rule-id for uploads matched by an enabled
// AbortIncompleteMultipartUpload rule (regression: never sent).
func TestAbortDateHeaders(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.putBucketConfig("bkt", storage.ConfigLifecycle, `<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
		`<Rule><ID>abort-tmp</ID><Filter><Prefix>tmp/</Prefix></Filter><Status>Enabled</Status>`+
		`<AbortIncompleteMultipartUpload><DaysAfterInitiation>2</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>`+
		`</LifecycleConfiguration>`)
	create := e.signed("POST", "/bkt/tmp/upload?uploads", nil, nil)
	if create.StatusCode != 200 {
		t.Fatalf("CreateMultipartUpload: %d %s", create.StatusCode, create.Data)
	}
	date := create.Header.Get("X-Amz-Abort-Date")
	at, err := http.ParseTime(date)
	if err != nil || create.Header.Get("X-Amz-Abort-Rule-Id") != "abort-tmp" {
		t.Fatalf("create: abort date %q rule %q", date, create.Header.Get("X-Amz-Abort-Rule-Id"))
	}
	if d := time.Until(at); d < 48*time.Hour || d > 72*time.Hour || at.Hour() != 0 || at.Minute() != 0 {
		t.Errorf("abort date %v not the midnight after initiation+2 days", at)
	}
	var res struct{ UploadId string }
	if err := xml.Unmarshal(create.Data, &res); err != nil {
		t.Fatal(err)
	}
	lp := e.signed("GET", "/bkt/tmp/upload?uploadId="+res.UploadId, nil, nil)
	if lp.StatusCode != 200 || lp.Header.Get("X-Amz-Abort-Date") != date || lp.Header.Get("X-Amz-Abort-Rule-Id") != "abort-tmp" {
		t.Errorf("ListParts: %d %q %q", lp.StatusCode, lp.Header.Get("X-Amz-Abort-Date"), lp.Header.Get("X-Amz-Abort-Rule-Id"))
	}
	other := e.signed("POST", "/bkt/other?uploads", nil, nil)
	if other.StatusCode != 200 || other.Header.Get("X-Amz-Abort-Date") != "" {
		t.Errorf("unmatched upload: %d abort date %q", other.StatusCode, other.Header.Get("X-Amz-Abort-Date"))
	}
}
