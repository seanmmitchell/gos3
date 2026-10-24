package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// lockOpRequests are the four object lock operations addressed at version
// vid of lock/k: method, path and body.
func lockOpRequests(vid string) []struct {
	name, method, path string
	body               []byte
} {
	v := "&versionId=" + url.QueryEscape(vid)
	until := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	return []struct {
		name, method, path string
		body               []byte
	}{
		{"GetObjectLegalHold", http.MethodGet, "/lock/k?legal-hold" + v, nil},
		{"PutObjectLegalHold", http.MethodPut, "/lock/k?legal-hold" + v, []byte(`<LegalHold><Status>OFF</Status></LegalHold>`)},
		{"GetObjectRetention", http.MethodGet, "/lock/k?retention" + v, nil},
		{"PutObjectRetention", http.MethodPut, "/lock/k?retention" + v,
			[]byte(`<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>` + until + `</RetainUntilDate></Retention>`)},
	}
}

// TestLockOpsEvaluateAddressedVersion: the object lock operations act on
// the version named by ?versionId, so authorization evaluates that
// version (its s3:ExistingObjectTag keys, s3:VersionId), not the latest
// one (N-01).
func TestLockOpsEvaluateAddressedVersion(t *testing.T) {
	e := newTestEnv(t)
	objLockBucket(t, e, "lock")
	v1 := e.putObject("lock", "k", []byte("evidence"), storage.ObjectMeta{Tags: map[string]string{"hold": "litigation"}, LegalHold: true})
	e.putPolicy("lit", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::lock/*"},
		{"Effect":"Deny","Action":["s3:PutObjectLegalHold","s3:GetObjectLegalHold","s3:PutObjectRetention","s3:GetObjectRetention"],
		 "Resource":"arn:aws:s3:::lock/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/hold":"litigation"}}}]}`)
	u := e.createUser("mallory", "lit")
	// An untagged overwrite becomes the latest version.
	v2 := e.putObject("lock", "k", []byte("decoy"), storage.ObjectMeta{})
	if v1.VersionID == "" || v2.VersionID == "" || v1.VersionID == v2.VersionID {
		t.Fatalf("versions %q %q", v1.VersionID, v2.VersionID)
	}
	for _, tc := range lockOpRequests(v1.VersionID) {
		t.Run(tc.name, func(t *testing.T) {
			objExpect(t, e.signedAs(u, tc.method, tc.path, tc.body, nil), 403, "AccessDenied")
		})
	}
	if info := objGetInfo(t, e, "lock", "k", v1.VersionID); !info.LegalHold || !info.RetainUntil.IsZero() {
		t.Fatalf("protected version changed: hold %v, retain until %v", info.LegalHold, info.RetainUntil)
	}
	// The untagged version is not protected by the Deny.
	for _, tc := range lockOpRequests(v2.VersionID) {
		if r := e.signedAs(u, tc.method, tc.path, tc.body, nil); r.StatusCode == 403 {
			t.Errorf("%s of the untagged version: %d %s", tc.name, r.StatusCode, r.Data)
		}
	}

	// s3:VersionId is the addressed version.
	e.putPolicy("byversion", fmt.Sprintf(`{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::lock/*"},
		{"Effect":"Deny","Action":["s3:PutObjectLegalHold","s3:GetObjectLegalHold","s3:PutObjectRetention","s3:GetObjectRetention"],
		 "Resource":"arn:aws:s3:::lock/*","Condition":{"StringEquals":{"s3:VersionId":%q}}}]}`, v1.VersionID))
	w := e.createUser("walter", "byversion")
	for _, tc := range lockOpRequests(v1.VersionID) {
		t.Run(tc.name+"/VersionId", func(t *testing.T) {
			objExpect(t, e.signedAs(w, tc.method, tc.path, tc.body, nil), 403, "AccessDenied")
		})
	}
}

// TestReauthorizeIgnoresDummyVersion: re-authorizing a long write repeats
// the J-03 rule: a write with a dummy versionId creates the current object,
// so the decision evaluates the current object's tags, not the bogus
// version's (N-01).
func TestReauthorizeIgnoresDummyVersion(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "k", []byte("protected"), storage.ObjectMeta{Tags: map[string]string{"protected": "yes"}})
	e.putPolicy("write", `{"Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"},
		{"Effect":"Deny","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"s3:ExistingObjectTag/protected":"yes"}}}]}`)
	u := e.createUser("writer", "write")
	for _, vid := range []string{"", "dummy", "null"} {
		r, err := http.NewRequest(http.MethodPut, e.url("/b/k"), nil)
		if err != nil {
			t.Fatal(err)
		}
		q := url.Values{}
		if vid != "" {
			q.Set("versionId", vid)
		}
		rc := &reqContext{
			op: lookupOp("PutObject"), opName: "PutObject", bucket: "b", key: "k", query: q, start: time.Now(),
			identity: auth.Identity{User: "writer", AccessKey: u.AccessKey},
			auth:     &auth.Result{Method: auth.MethodHeaderV4},
		}
		if err := e.server.reauthorizeOp(context.Background(), r, rc, nil); !errors.Is(err, s3err.ErrAccessDenied) {
			t.Errorf("versionId %q: reauthorized with %v", vid, err)
		}
	}
}

// listOps are the four listings and their query strings.
var listOps = []struct{ name, query string }{
	{"ListObjects", ""},
	{"ListObjectsV2", "list-type=2&"},
	{"ListObjectVersions", "versions&"},
	{"ListMultipartUploads", "uploads&"},
}

// TestListQueryMatchesPolicyView: a listing parameter longer than the
// condition context holds, or not valid UTF-8, is refused before
// authorization, so a policy never sees a truncated value while the
// listing uses the full one (N-02, J-02).
func TestListQueryMatchesPolicyView(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("home")
	doc, err := os.ReadFile("../iam/testdata/prefix-per-user.json")
	if err != nil {
		t.Fatal(err)
	}
	e.putPolicy("home", string(doc))
	alice := e.createUser("alice", "home")
	e.putObject("home", "alice/notes", []byte("a"), storage.ObjectMeta{})
	e.putObject("home", "bob/secret", []byte("b"), storage.ObjectMeta{})

	// The PoC: the policy saw delimiter "/" (TruncateRequestValue backs up
	// to a character boundary), the listing used "/a\x80..." and returned
	// every user's keys.
	poc := "/a" + strings.Repeat("\x80", iam.MaxRequestValueLen-1)
	r := e.signedAs(alice, http.MethodGet, "/home?list-type=2&prefix=&delimiter="+url.QueryEscape(poc), nil, nil)
	if strings.Contains(string(r.Data), "bob/secret") {
		t.Fatalf("listing leaked another user's keys: %s", r.Data)
	}
	objExpect(t, r, 400, "InvalidArgument")
	// The template still works.
	r = e.signedAs(alice, http.MethodGet, "/home?list-type=2&prefix=&delimiter=/", nil, nil)
	if r.StatusCode != 200 || strings.Contains(string(r.Data), "bob/secret") {
		t.Fatalf("root listing: %d %s", r.StatusCode, r.Data)
	}

	params := []string{"prefix", "delimiter", "marker", "start-after", "key-marker", "upload-id-marker", "version-id-marker"}
	bad := map[string]string{
		"too long":     strings.Repeat("a", iam.MaxRequestValueLen+1),
		"invalid utf8": "a\xff",
		"partial rune": "a\xc3",
	}
	for _, op := range listOps {
		for _, p := range params {
			for name, v := range bad {
				t.Run(op.name+"/"+p+"/"+name, func(t *testing.T) {
					r := e.signed(http.MethodGet, "/home?"+op.query+p+"="+url.QueryEscape(v), nil, nil)
					objExpect(t, r, 400, "InvalidArgument")
					if !strings.Contains(string(r.Data), "<ArgumentName>"+p+"</ArgumentName>") {
						t.Fatalf("argument name missing: %s", r.Data)
					}
				})
			}
			// The longest value a policy sees in full is accepted.
			ok := strings.Repeat("a", iam.MaxRequestValueLen)
			if p == "version-id-marker" {
				ok = "" // needs a key-marker and a real version ID
			}
			if r := e.signed(http.MethodGet, "/home?"+op.query+p+"="+url.QueryEscape(ok), nil, nil); r.StatusCode != 200 {
				t.Errorf("%s %s of %d bytes: %d %s", op.name, p, len(ok), r.StatusCode, r.Data)
			}
		}
		// max-keys is in the condition context too.
		objExpect(t, e.signed(http.MethodGet, "/home?"+op.query+"max-keys="+strings.Repeat("0", iam.MaxRequestValueLen)+"5", nil, nil), 400, "InvalidArgument")
	}
}

// TestContinuationTokenMustBeUTF8: a continuation token decoding to
// invalid UTF-8 (never issued: listed keys are valid UTF-8) is refused,
// so it cannot loosen a native listing's lower bound (J-02).
func TestContinuationTokenMustBeUTF8(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putObject("b", "k", []byte("x"), storage.ObjectMeta{})
	for _, after := range []string{"\x80", "a\xc3"} {
		tok := encodeContinuationToken(after)
		objExpect(t, e.signed(http.MethodGet, "/b?list-type=2&continuation-token="+url.QueryEscape(tok), nil, nil), 400, "InvalidArgument")
	}
	tok := encodeContinuationToken("a")
	if r := e.signed(http.MethodGet, "/b?list-type=2&continuation-token="+url.QueryEscape(tok), nil, nil); r.StatusCode != 200 {
		t.Fatalf("valid token: %d %s", r.StatusCode, r.Data)
	}
}
