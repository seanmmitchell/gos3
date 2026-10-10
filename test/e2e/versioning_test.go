package e2e

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestVersioningLifecycle(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			gv, err := c.GetBucketVersioning(ctx(t), &s3.GetBucketVersioningInput{Bucket: aws.String(b)})
			must(t, err, "GetBucketVersioning")
			if gv.Status != "" {
				t.Fatalf("new bucket versioning %q", gv.Status)
			}

			// Unversioned: no version ids anywhere.
			pre := e.put(t, b, "k", []byte("pre-versioning"))
			if pre.VersionId != nil {
				t.Errorf("unversioned PUT returned version %q", aws.ToString(pre.VersionId))
			}
			raw := e.signed(t, rootCreds, http.MethodHead, "/"+b+"/k", nil, nil)
			if _, ok := raw.header["X-Amz-Version-Id"]; ok {
				t.Errorf("unversioned HEAD has x-amz-version-id")
			}

			e.setVersioning(t, b, "Enabled")
			gv, err = c.GetBucketVersioning(ctx(t), &s3.GetBucketVersioningInput{Bucket: aws.String(b)})
			must(t, err, "GetBucketVersioning")
			if gv.Status != s3types.BucketVersioningStatusEnabled {
				t.Fatalf("status %q", gv.Status)
			}
			// The pre-existing object is the "null" version.
			raw = e.signed(t, rootCreds, http.MethodHead, "/"+b+"/k", nil, nil)
			if raw.header.Get("X-Amz-Version-Id") != "null" {
				t.Errorf("pre-versioning object version %q, want null", raw.header.Get("X-Amz-Version-Id"))
			}

			v1 := aws.ToString(e.put(t, b, "k", []byte("one")).VersionId)
			v2 := aws.ToString(e.put(t, b, "k", []byte("two")).VersionId)
			if v1 == "" || v2 == "" || v1 == v2 || v1 == "null" {
				t.Fatalf("version ids %q %q", v1, v2)
			}
			for vid, want := range map[string]string{"null": "pre-versioning", v1: "one", v2: "two"} {
				out, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("k"), VersionId: aws.String(vid)})
				must(t, err, "GetObject version "+vid)
				if got := string(readAll(t, out.Body)); got != want || aws.ToString(out.VersionId) != vid {
					t.Errorf("version %s: %q (version header %q)", vid, got, aws.ToString(out.VersionId))
				}
			}
			if got := string(e.get(t, b, "k")); got != "two" {
				t.Errorf("latest = %q", got)
			}

			// Delete marker.
			del, err := c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("k")})
			must(t, err, "DeleteObject")
			marker := aws.ToString(del.VersionId)
			if !aws.ToBool(del.DeleteMarker) || marker == "" {
				t.Fatalf("delete: marker=%v version=%q", aws.ToBool(del.DeleteMarker), marker)
			}
			resp := e.signed(t, rootCreds, http.MethodGet, "/"+b+"/k", nil, nil)
			if resp.status != 404 || resp.code() != "NoSuchKey" || resp.header.Get("X-Amz-Delete-Marker") != "true" || resp.header.Get("X-Amz-Version-Id") != marker {
				t.Errorf("GET behind delete marker: %d %s %v", resp.status, resp.code(), resp.header)
			}
			resp = e.signed(t, rootCreds, http.MethodGet, "/"+b+"/k?versionId="+url.QueryEscape(marker), nil, nil)
			if resp.status != 405 || resp.code() != "MethodNotAllowed" || resp.header.Get("X-Amz-Delete-Marker") != "true" {
				t.Errorf("GET delete marker version: %d %s %v", resp.status, resp.code(), resp.header)
			}
			resp = e.signed(t, rootCreds, http.MethodHead, "/"+b+"/k?versionId="+url.QueryEscape(marker), nil, nil)
			if resp.status != 405 || resp.header.Get("X-Amz-Delete-Marker") != "true" {
				t.Errorf("HEAD delete marker version: %d %v", resp.status, resp.header)
			}
			// Old versions are still readable.
			out, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("k"), VersionId: aws.String(v1)})
			must(t, err, "GetObject old version behind marker")
			readAll(t, out.Body)

			// Removing the delete marker restores the object.
			dd, err := c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("k"), VersionId: aws.String(marker)})
			must(t, err, "DeleteObject marker")
			if !aws.ToBool(dd.DeleteMarker) || aws.ToString(dd.VersionId) != marker {
				t.Errorf("delete marker removal: %+v", dd)
			}
			if got := string(e.get(t, b, "k")); got != "two" {
				t.Errorf("after removing marker = %q", got)
			}
			// Permanently delete the latest version: v1 becomes current.
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("k"), VersionId: aws.String(v2)})
			must(t, err, "DeleteObject v2")
			if got := string(e.get(t, b, "k")); got != "one" {
				t.Errorf("after deleting v2 = %q", got)
			}
			_, err = c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("k"), VersionId: aws.String(v2)})
			wantErr(t, err, "NoSuchVersion", http.StatusNotFound)
			resp = e.signed(t, rootCreds, http.MethodGet, "/"+b+"/k?versionId=", nil, nil)
			if resp.status != 400 || resp.code() != "InvalidArgument" {
				t.Errorf("empty versionId: %d %s", resp.status, resp.code())
			}

			// Suspended: new writes are "null", replacing the old null version.
			e.setVersioning(t, b, "Suspended")
			sp := e.put(t, b, "k", []byte("suspended"))
			if aws.ToString(sp.VersionId) != "null" {
				t.Errorf("suspended PUT version %q, want null", aws.ToString(sp.VersionId))
			}
			lv, err := c.ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{Bucket: aws.String(b), Prefix: aws.String("k")})
			must(t, err, "ListObjectVersions")
			var ids []string
			for _, v := range lv.Versions {
				ids = append(ids, aws.ToString(v.VersionId))
				if aws.ToBool(v.IsLatest) != (aws.ToString(v.VersionId) == "null") {
					t.Errorf("IsLatest wrong for %s", aws.ToString(v.VersionId))
				}
			}
			if len(ids) != 2 || ids[0] != "null" || ids[1] != v1 {
				t.Errorf("versions after suspend = %v, want [null %s]", ids, v1)
			}
			out, err = c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("k"), VersionId: aws.String("null")})
			must(t, err, "GetObject null")
			if got := string(readAll(t, out.Body)); got != "suspended" {
				t.Errorf("null version = %q", got)
			}
			// A delete in a suspended bucket creates a null delete marker.
			sd, err := c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("k")})
			must(t, err, "DeleteObject suspended")
			if !aws.ToBool(sd.DeleteMarker) || aws.ToString(sd.VersionId) != "null" {
				t.Errorf("suspended delete: marker=%v version=%q", aws.ToBool(sd.DeleteMarker), aws.ToString(sd.VersionId))
			}
			lv, err = c.ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{Bucket: aws.String(b), Prefix: aws.String("k")})
			must(t, err, "ListObjectVersions")
			if len(lv.DeleteMarkers) != 1 || aws.ToString(lv.DeleteMarkers[0].VersionId) != "null" || len(lv.Versions) != 1 {
				t.Errorf("after suspended delete: %d markers, %d versions", len(lv.DeleteMarkers), len(lv.Versions))
			}
			// Cannot go back to unversioned.
			_, err = c.PutBucketVersioning(ctx(t), &s3.PutBucketVersioningInput{Bucket: aws.String(b),
				VersioningConfiguration: &s3types.VersioningConfiguration{Status: "Disabled"}})
			wantErr(t, err, "MalformedXML", 400)
		})
	}
}

func TestDeleteObjectsMixed(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newVersionedBucket(t)
			v := e.put(t, b, "versioned", []byte("1"))
			e.put(t, b, "plain", []byte("1"))
			e.put(t, b, "with space+plus", []byte("1"))
			out, err := c.DeleteObjects(ctx(t), &s3.DeleteObjectsInput{Bucket: aws.String(b), Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{
				{Key: aws.String("plain")},
				{Key: aws.String("with space+plus")},
				{Key: aws.String("missing")},                                           // still "Deleted" (marker)
				{Key: aws.String("versioned"), VersionId: v.VersionId},                 // permanent
				{Key: aws.String("versioned"), VersionId: aws.String("bogus-version")}, // no such version
			}}})
			must(t, err, "DeleteObjects")
			deleted := map[string]s3types.DeletedObject{}
			for _, d := range out.Deleted {
				deleted[aws.ToString(d.Key)+"@"+aws.ToString(d.VersionId)] = d
			}
			for _, k := range []string{"plain", "with space+plus", "missing"} {
				found := false
				for _, d := range out.Deleted {
					if aws.ToString(d.Key) == k {
						found = true
						if !aws.ToBool(d.DeleteMarker) || aws.ToString(d.DeleteMarkerVersionId) == "" {
							t.Errorf("%s: deleted entry %+v", k, d)
						}
					}
				}
				if !found {
					t.Errorf("%s not in Deleted", k)
				}
			}
			if d, ok := deleted["versioned@"+aws.ToString(v.VersionId)]; !ok || aws.ToBool(d.DeleteMarker) {
				t.Errorf("permanent version delete missing/wrong: %+v", out.Deleted)
			}
			// Deleting a version that does not exist is idempotent, like
			// DeleteObject?versionId= (AWS rejects only malformed AWS-format
			// IDs, which gos3 cannot tell apart from its own).
			if _, ok := deleted["versioned@bogus-version"]; !ok && (len(out.Errors) != 1 || aws.ToString(out.Errors[0].Code) != "NoSuchVersion") {
				t.Errorf("bogus version: deleted %+v errors %+v", out.Deleted, out.Errors)
			}

			// Quiet mode reports only errors.
			e.put(t, b, "q1", []byte("1"))
			out, err = c.DeleteObjects(ctx(t), &s3.DeleteObjectsInput{Bucket: aws.String(b), Delete: &s3types.Delete{Quiet: aws.Bool(true),
				Objects: []s3types.ObjectIdentifier{{Key: aws.String("q1")}, {Key: aws.String("q2")}}}})
			must(t, err, "DeleteObjects quiet")
			if len(out.Deleted) != 0 || len(out.Errors) != 0 {
				t.Errorf("quiet: %d deleted, %d errors", len(out.Deleted), len(out.Errors))
			}

			// Unversioned bucket: plain deletes.
			ub := e.newBucket(t)
			var ids []s3types.ObjectIdentifier
			for i := 0; i < 50; i++ {
				k := "obj" + string(rune('a'+i%26)) + string(rune('0'+i/26))
				e.put(t, ub, k, []byte("x"))
				ids = append(ids, s3types.ObjectIdentifier{Key: aws.String(k)})
			}
			out, err = c.DeleteObjects(ctx(t), &s3.DeleteObjectsInput{Bucket: aws.String(ub), Delete: &s3types.Delete{Objects: ids}})
			must(t, err, "DeleteObjects unversioned")
			if len(out.Deleted) != 50 || len(out.Errors) != 0 {
				t.Fatalf("unversioned: %d deleted, %d errors", len(out.Deleted), len(out.Errors))
			}
			l, err := c.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(ub)})
			must(t, err, "list")
			if len(l.Contents) != 0 {
				t.Errorf("%d objects left", len(l.Contents))
			}
			// More than 1000 keys is rejected.
			var many []s3types.ObjectIdentifier
			for i := 0; i < 1001; i++ {
				many = append(many, s3types.ObjectIdentifier{Key: aws.String("k")})
			}
			_, err = c.DeleteObjects(ctx(t), &s3.DeleteObjectsInput{Bucket: aws.String(ub), Delete: &s3types.Delete{Objects: many}})
			wantErr(t, err, "MalformedXML", 400)
		})
	}
}

func TestObjectLock(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := bucketName(t)
			_, err := c.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(b), ObjectLockEnabledForBucket: aws.Bool(true)})
			must(t, err, "CreateBucket with object lock")
			gv, err := c.GetBucketVersioning(ctx(t), &s3.GetBucketVersioningInput{Bucket: aws.String(b)})
			must(t, err, "GetBucketVersioning")
			if gv.Status != s3types.BucketVersioningStatusEnabled {
				t.Errorf("object lock bucket versioning %q", gv.Status)
			}
			_, err = c.PutBucketVersioning(ctx(t), &s3.PutBucketVersioningInput{Bucket: aws.String(b),
				VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusSuspended}})
			wantErr(t, err, "InvalidBucketState", http.StatusConflict)

			olc, err := c.GetObjectLockConfiguration(ctx(t), &s3.GetObjectLockConfigurationInput{Bucket: aws.String(b)})
			must(t, err, "GetObjectLockConfiguration")
			if olc.ObjectLockConfiguration.ObjectLockEnabled != s3types.ObjectLockEnabledEnabled || olc.ObjectLockConfiguration.Rule != nil {
				t.Errorf("lock config %+v", olc.ObjectLockConfiguration)
			}
			_, err = c.PutObjectLockConfiguration(ctx(t), &s3.PutObjectLockConfigurationInput{Bucket: aws.String(b),
				ObjectLockConfiguration: &s3types.ObjectLockConfiguration{ObjectLockEnabled: s3types.ObjectLockEnabledEnabled,
					Rule: &s3types.ObjectLockRule{DefaultRetention: &s3types.DefaultRetention{Mode: s3types.ObjectLockRetentionModeGovernance, Days: aws.Int32(1)}}}})
			must(t, err, "PutObjectLockConfiguration")
			olc, err = c.GetObjectLockConfiguration(ctx(t), &s3.GetObjectLockConfigurationInput{Bucket: aws.String(b)})
			must(t, err, "GetObjectLockConfiguration")
			if r := olc.ObjectLockConfiguration.Rule; r == nil || r.DefaultRetention.Mode != s3types.ObjectLockRetentionModeGovernance || aws.ToInt32(r.DefaultRetention.Days) != 1 {
				t.Errorf("lock rule %+v", olc.ObjectLockConfiguration.Rule)
			}

			// Default retention applies to new versions.
			gov := aws.ToString(e.put(t, b, "gov", []byte("governed")).VersionId)
			h, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("gov")})
			must(t, err, "HeadObject")
			if h.ObjectLockMode != s3types.ObjectLockModeGovernance || h.ObjectLockRetainUntilDate == nil ||
				h.ObjectLockRetainUntilDate.Before(time.Now().Add(23*time.Hour)) || h.ObjectLockRetainUntilDate.After(time.Now().Add(25*time.Hour)) {
				t.Errorf("default retention: mode %q until %v", h.ObjectLockMode, h.ObjectLockRetainUntilDate)
			}
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("gov"), VersionId: aws.String(gov)})
			wantErr(t, err, "AccessDenied", http.StatusForbidden)
			// A delete without version id only adds a delete marker: allowed.
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("gov")})
			must(t, err, "DeleteObject (marker) on locked object")
			// Governance can be bypassed by a caller with the permission.
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("gov"), VersionId: aws.String(gov), BypassGovernanceRetention: aws.Bool(true)})
			must(t, err, "DeleteObject bypass governance")

			// Explicit COMPLIANCE retention on PutObject.
			until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
			comp := aws.ToString(e.put(t, b, "comp", []byte("x"), func(in *s3.PutObjectInput) {
				in.ObjectLockMode = s3types.ObjectLockModeCompliance
				in.ObjectLockRetainUntilDate = aws.Time(until)
			}).VersionId)
			ret, err := c.GetObjectRetention(ctx(t), &s3.GetObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("comp"), VersionId: aws.String(comp)})
			must(t, err, "GetObjectRetention")
			if ret.Retention.Mode != s3types.ObjectLockRetentionModeCompliance || !aws.ToTime(ret.Retention.RetainUntilDate).Equal(until) {
				t.Errorf("retention %+v (want until %v)", ret.Retention, until)
			}
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("comp"), VersionId: aws.String(comp), BypassGovernanceRetention: aws.Bool(true)})
			wantErr(t, err, "AccessDenied", http.StatusForbidden)
			// Compliance retention cannot be shortened or downgraded, but can be extended.
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("comp"), VersionId: aws.String(comp),
				Retention: &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(until.Add(-time.Hour))}})
			wantErr(t, err, "AccessDenied", http.StatusForbidden)
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("comp"), VersionId: aws.String(comp), BypassGovernanceRetention: aws.Bool(true),
				Retention: &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(until)}})
			wantErr(t, err, "AccessDenied", http.StatusForbidden)
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("comp"), VersionId: aws.String(comp),
				Retention: &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(until.Add(time.Hour))}})
			must(t, err, "extend compliance retention")
			// Retention in the past is invalid.
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("comp"), VersionId: aws.String(comp),
				Retention: &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(time.Now().Add(-time.Hour))}})
			wantErr(t, err, "InvalidArgument", 400)

			// Governance retention shortened only with bypass.
			g2 := aws.ToString(e.put(t, b, "gov2", []byte("x")).VersionId)
			short := &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(time.Now().Add(time.Hour))}
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("gov2"), VersionId: aws.String(g2), Retention: short})
			wantErr(t, err, "AccessDenied", http.StatusForbidden)
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("gov2"), VersionId: aws.String(g2), Retention: short, BypassGovernanceRetention: aws.Bool(true)})
			must(t, err, "shorten governance with bypass")

			// Legal hold blocks deletion even with bypass.
			lh := aws.ToString(e.put(t, b, "hold", []byte("x"), func(in *s3.PutObjectInput) {
				in.ObjectLockLegalHoldStatus = s3types.ObjectLockLegalHoldStatusOn
			}).VersionId)
			// The default retention also applies; remove it with bypass first.
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: aws.String("hold"), VersionId: aws.String(lh),
				BypassGovernanceRetention: aws.Bool(true), Retention: &s3types.ObjectLockRetention{}})
			must(t, err, "clear governance retention")
			hold, err := c.GetObjectLegalHold(ctx(t), &s3.GetObjectLegalHoldInput{Bucket: aws.String(b), Key: aws.String("hold"), VersionId: aws.String(lh)})
			must(t, err, "GetObjectLegalHold")
			if hold.LegalHold.Status != s3types.ObjectLockLegalHoldStatusOn {
				t.Errorf("legal hold %q", hold.LegalHold.Status)
			}
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("hold"), VersionId: aws.String(lh), BypassGovernanceRetention: aws.Bool(true)})
			wantErr(t, err, "AccessDenied", http.StatusForbidden)
			_, err = c.PutObjectLegalHold(ctx(t), &s3.PutObjectLegalHoldInput{Bucket: aws.String(b), Key: aws.String("hold"), VersionId: aws.String(lh),
				LegalHold: &s3types.ObjectLockLegalHold{Status: s3types.ObjectLockLegalHoldStatusOff}})
			must(t, err, "PutObjectLegalHold OFF")
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("hold"), VersionId: aws.String(lh)})
			must(t, err, "delete after legal hold released")

			// DeleteObjects respects locks per key.
			dr, err := c.DeleteObjects(ctx(t), &s3.DeleteObjectsInput{Bucket: aws.String(b), Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{
				{Key: aws.String("comp"), VersionId: aws.String(comp)}, {Key: aws.String("gov2"), VersionId: aws.String(g2)},
			}}})
			must(t, err, "DeleteObjects locked")
			if len(dr.Errors) != 2 || aws.ToString(dr.Errors[0].Code) != "AccessDenied" {
				t.Errorf("DeleteObjects on locked versions: %+v deleted %+v", dr.Errors, dr.Deleted)
			}

			// Buckets without object lock reject lock operations.
			plain := e.newVersionedBucket(t)
			e.put(t, plain, "k", []byte("x"))
			_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{Bucket: aws.String(plain), Key: aws.String("k"),
				Retention: &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(time.Now().Add(time.Hour))}})
			wantErr(t, err, "InvalidRequest", 400)
			_, err = c.GetObjectLockConfiguration(ctx(t), &s3.GetObjectLockConfigurationInput{Bucket: aws.String(plain)})
			wantErr(t, err, "ObjectLockConfigurationNotFoundError", http.StatusNotFound)
			_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(plain), Key: aws.String("k2"), Body: stringsReader("x"),
				ObjectLockLegalHoldStatus: s3types.ObjectLockLegalHoldStatusOn})
			wantErr(t, err, "InvalidRequest", 400)
		})
	}
}
