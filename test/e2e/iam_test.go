package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// denied asserts an SDK AccessDenied (403).
func denied(t *testing.T, err error, what string) {
	t.Helper()
	if apiCode(err) != "AccessDenied" || httpStatus(err) != http.StatusForbidden {
		t.Errorf("%s: want 403 AccessDenied, got %v", what, err)
	}
}

func allowed(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: want success, got %v", what, err)
	}
}

func TestIAMBuiltinPolicies(t *testing.T) {
	t.Parallel()
	e := fsEnv
	b := e.newBucket(t)
	e.put(t, b, "existing", []byte("data"))

	ro := e.s3Client(e.createUser(t, userName("reader"), "readonly"))
	wo := e.s3Client(e.createUser(t, userName("writer"), "writeonly"))
	rw := e.s3Client(e.createUser(t, userName("rw"), "readwrite"))

	// readonly.
	_, err := ro.ListBuckets(ctx(t), &s3.ListBucketsInput{})
	allowed(t, err, "readonly ListBuckets")
	_, err = ro.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	allowed(t, err, "readonly ListObjectsV2")
	out, err := ro.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("existing")})
	if allowed(t, err, "readonly GetObject"); err == nil {
		readAll(t, out.Body)
	}
	_, err = ro.HeadBucket(ctx(t), &s3.HeadBucketInput{Bucket: aws.String(b)})
	allowed(t, err, "readonly HeadBucket")
	_, err = ro.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("existing")})
	allowed(t, err, "readonly GetObjectTagging")
	_, err = ro.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("ro"), Body: stringsReader("x")})
	denied(t, err, "readonly PutObject")
	_, err = ro.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("existing")})
	denied(t, err, "readonly DeleteObject")
	_, err = ro.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(bucketName(t))})
	denied(t, err, "readonly CreateBucket")
	_, err = ro.PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(`{}`)})
	denied(t, err, "readonly PutBucketPolicy")

	// writeonly.
	_, err = wo.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("wo"), Body: stringsReader("x")})
	allowed(t, err, "writeonly PutObject")
	_, err = wo.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("existing")})
	denied(t, err, "writeonly GetObject")
	_, err = wo.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	denied(t, err, "writeonly ListObjectsV2")
	_, err = wo.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("wo")})
	denied(t, err, "writeonly DeleteObject")
	// Multipart through the manager (create/upload/complete map to s3:PutObject).
	up := manager.NewUploader(wo, func(u *manager.Uploader) { u.PartSize = 5 << 20; u.Concurrency = 3 })
	_, err = up.Upload(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("wo-multipart"), Body: bytes.NewReader(randomData(11<<20, 20))})
	allowed(t, err, "writeonly multipart upload")
	// Copy needs read access to the source.
	_, err = wo.CopyObject(ctx(t), &s3.CopyObjectInput{Bucket: aws.String(b), Key: aws.String("wo-copy"), CopySource: aws.String(b + "/existing")})
	denied(t, err, "writeonly CopyObject (source read)")

	// readwrite: everything on objects; DeleteObjects mixed with policy deny
	// is covered by TestIAMCustomPolicy.
	_, err = rw.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("rw"), Body: stringsReader("x")})
	allowed(t, err, "readwrite PutObject")
	_, err = rw.CopyObject(ctx(t), &s3.CopyObjectInput{Bucket: aws.String(b), Key: aws.String("rw-copy"), CopySource: aws.String(b + "/existing")})
	allowed(t, err, "readwrite CopyObject")
	_, err = rw.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("rw")})
	allowed(t, err, "readwrite DeleteObject")
	// readwrite has no admin:* actions.
	if r := e.admin(t, e.lastUserCreds(t, rw), http.MethodGet, "/users", nil, nil); r.status != http.StatusForbidden {
		t.Errorf("readwrite admin GET /users: %d %s", r.status, r.body)
	}
}

// lastUserCreds extracts the static credentials of an SDK client.
func (e *env) lastUserCreds(t *testing.T, c *s3.Client) auth.Credentials {
	t.Helper()
	v, err := c.Options().Credentials.Retrieve(ctx(t))
	must(t, err, "credentials")
	return auth.Credentials{AccessKey: v.AccessKeyID, SecretKey: v.SecretAccessKey}
}

func TestIAMCustomPolicy(t *testing.T) {
	t.Parallel()
	e := memEnv
	b := e.newBucket(t)
	policy := `{
	  "Version": "2012-10-17",
	  "Statement": [
	    {"Sid": "ListOwnPrefix", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::` + b + `",
	     "Condition": {"StringLike": {"s3:prefix": ["home/${aws:username}/*", "home/${aws:username}/"]}}},
	    {"Sid": "OwnObjects", "Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
	     "Resource": "arn:aws:s3:::` + b + `/home/${aws:username}/*"},
	    {"Sid": "NoDeleteKeep", "Effect": "Deny", "Action": "s3:DeleteObject", "Resource": "arn:aws:s3:::` + b + `/home/*/keep*"}
	  ]
	}`
	pname := userName("homedirs")
	e.putPolicy(t, pname, policy)
	alice := userName("alice")
	bob := userName("bob")
	ac := e.s3Client(e.createUser(t, alice, pname))
	bc := e.s3Client(e.createUser(t, bob, pname))
	e.put(t, b, "home/"+bob+"/private", []byte("bob's"))

	_, err := ac.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("home/" + alice + "/doc"), Body: stringsReader("a")})
	allowed(t, err, "alice PUT own")
	_, err = ac.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("home/" + alice + "/keep-me"), Body: stringsReader("a")})
	allowed(t, err, "alice PUT keep")
	_, err = ac.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("home/" + bob + "/x"), Body: stringsReader("a")})
	denied(t, err, "alice PUT bob's")
	_, err = ac.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("home/" + bob + "/private")})
	denied(t, err, "alice GET bob's")
	out, err := bc.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("home/" + bob + "/private")})
	if allowed(t, err, "bob GET own"); err == nil {
		readAll(t, out.Body)
	}
	_, err = ac.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), Prefix: aws.String("home/" + alice + "/")})
	allowed(t, err, "alice list own prefix")
	_, err = ac.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), Prefix: aws.String("home/")})
	denied(t, err, "alice list home/")
	_, err = ac.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	denied(t, err, "alice list bucket root")
	_, err = ac.ListBuckets(ctx(t), &s3.ListBucketsInput{})
	denied(t, err, "alice ListBuckets")

	// DeleteObjects with mixed results: own key deleted; protected and
	// foreign keys AccessDenied per key.
	res, err := ac.DeleteObjects(ctx(t), &s3.DeleteObjectsInput{Bucket: aws.String(b), Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{
		{Key: aws.String("home/" + alice + "/doc")},
		{Key: aws.String("home/" + alice + "/keep-me")},
		{Key: aws.String("home/" + bob + "/private")},
	}}})
	must(t, err, "alice DeleteObjects")
	if len(res.Deleted) != 1 || aws.ToString(res.Deleted[0].Key) != "home/"+alice+"/doc" {
		t.Errorf("deleted %+v", res.Deleted)
	}
	codes := map[string]string{}
	for _, er := range res.Errors {
		codes[aws.ToString(er.Key)] = aws.ToString(er.Code)
	}
	if codes["home/"+alice+"/keep-me"] != "AccessDenied" || codes["home/"+bob+"/private"] != "AccessDenied" || len(codes) != 2 {
		t.Errorf("errors %v", codes)
	}
	if got := string(e.get(t, b, "home/"+bob+"/private")); got != "bob's" {
		t.Errorf("bob's object = %q", got)
	}

	// Simulation through the admin API agrees.
	var dec struct {
		Allowed      bool   `json:"allowed"`
		ExplicitDeny bool   `json:"explicitDeny"`
		Reason       string `json:"reason"`
	}
	r := e.admin(t, rootCreds, http.MethodPost, "/authz/simulate", map[string]any{"user": alice, "action": "s3:DeleteObject", "bucket": b, "key": "home/" + alice + "/keep-me"}, &dec)
	if r.status != 200 || dec.Allowed || !dec.ExplicitDeny {
		t.Errorf("simulate: %d %+v %s", r.status, dec, r.body)
	}

	// Changing the user's policies takes effect immediately.
	r = e.admin(t, rootCreds, http.MethodPatch, "/users/"+alice, map[string]any{"policies": []string{"readonly"}}, nil)
	if r.status != 200 {
		t.Fatalf("PATCH user: %d %s", r.status, r.body)
	}
	_, err = ac.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("home/" + alice + "/doc2"), Body: stringsReader("a")})
	denied(t, err, "alice PUT after switching to readonly")
	_, err = ac.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	allowed(t, err, "alice list as readonly")

	// A policy in use cannot be deleted.
	if r := e.admin(t, rootCreds, http.MethodDelete, "/policies/"+pname, nil, nil); r.status != http.StatusConflict {
		t.Errorf("DELETE attached policy: %d %s", r.status, r.body)
	}
}

func TestIAMKeysAndAdminAPI(t *testing.T) {
	t.Parallel()
	e := fsEnv
	b := e.newBucket(t)
	name := userName("keys")
	creds := e.createUser(t, name, "readwrite")
	c := e.s3Client(creds)
	_, err := c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("k"), Body: stringsReader("x")})
	must(t, err, "PutObject as user")

	// whoami.
	var who struct {
		User         string          `json:"user"`
		AccessKey    string          `json:"accessKey"`
		IsRoot       bool            `json:"isRoot"`
		Policies     []string        `json:"policies"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	r := e.admin(t, creds, http.MethodGet, "/whoami", nil, &who)
	if r.status != 200 || who.User != name || who.AccessKey != creds.AccessKey || who.IsRoot || who.Capabilities["admin"] {
		t.Errorf("whoami: %d %+v", r.status, who)
	}
	r = e.admin(t, rootCreds, http.MethodGet, "/whoami", nil, &who)
	if r.status != 200 || !who.IsRoot || !who.Capabilities["manageUsers"] {
		t.Errorf("root whoami: %d %+v", r.status, who)
	}
	// Anonymous and badly signed admin calls.
	if r := e.anon(t, http.MethodGet, adminPrefix+"/whoami", nil, nil); r.status != http.StatusUnauthorized {
		t.Errorf("anonymous whoami: %d", r.status)
	}
	bad := auth.Credentials{AccessKey: creds.AccessKey, SecretKey: "wrong-secret-key"}
	if r := e.admin(t, bad, http.MethodGet, "/whoami", nil, nil); r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), "SignatureDoesNotMatch") {
		t.Errorf("bad signature whoami: %d %s", r.status, r.body)
	}

	// Second key, deactivate the first.
	var k2 adminKey
	r = e.admin(t, rootCreds, http.MethodPost, "/users/"+name+"/keys", map[string]any{"description": "second"}, &k2)
	if r.status != http.StatusCreated || k2.SecretKey == "" {
		t.Fatalf("create key: %d %s", r.status, r.body)
	}
	if r := e.admin(t, rootCreds, http.MethodPatch, "/keys/"+creds.AccessKey, map[string]any{"status": "Inactive"}, nil); r.status != http.StatusNoContent {
		t.Fatalf("deactivate key: %d %s", r.status, r.body)
	}
	_, err = c.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	wantErr(t, err, "InvalidAccessKeyId", http.StatusForbidden)
	c2 := e.s3Client(auth.Credentials{AccessKey: k2.AccessKey, SecretKey: k2.SecretKey})
	_, err = c2.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	must(t, err, "ListObjectsV2 with second key")
	if r := e.admin(t, rootCreds, http.MethodPatch, "/keys/"+creds.AccessKey, map[string]any{"status": "Active"}, nil); r.status != http.StatusNoContent {
		t.Fatalf("reactivate key: %d %s", r.status, r.body)
	}
	_, err = c.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	must(t, err, "ListObjectsV2 after reactivation")

	// Disabled user.
	if r := e.admin(t, rootCreds, http.MethodPatch, "/users/"+name, map[string]any{"disabled": true}, nil); r.status != 200 {
		t.Fatalf("disable user: %d %s", r.status, r.body)
	}
	_, err = c2.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	if err == nil {
		t.Error("disabled user still allowed")
	}
	// Deleted user: keys are gone.
	if r := e.admin(t, rootCreds, http.MethodDelete, "/users/"+name, nil, nil); r.status != http.StatusNoContent {
		t.Fatalf("delete user: %d %s", r.status, r.body)
	}
	_, err = c2.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	wantErr(t, err, "InvalidAccessKeyId", http.StatusForbidden)
	if r := e.admin(t, rootCreds, http.MethodGet, "/users/"+name, nil, nil); r.status != http.StatusNotFound {
		t.Errorf("GET deleted user: %d", r.status)
	}

	// Admin API: duplicate user, unknown policy, invalid name.
	dup := userName("dup")
	e.createUser(t, dup)
	if r := e.admin(t, rootCreds, http.MethodPost, "/users", map[string]any{"name": dup, "policies": []string{}}, nil); r.status != http.StatusConflict {
		t.Errorf("duplicate user: %d %s", r.status, r.body)
	}
	if r := e.admin(t, rootCreds, http.MethodPost, "/users", map[string]any{"name": userName("x"), "policies": []string{"no-such-policy"}}, nil); r.status != http.StatusNotFound && r.status != http.StatusBadRequest {
		t.Errorf("unknown policy: %d %s", r.status, r.body)
	}
	if r := e.admin(t, rootCreds, http.MethodPost, "/users", map[string]any{"name": "root", "policies": []string{}}, nil); r.status != http.StatusBadRequest {
		t.Errorf("reserved name: %d %s", r.status, r.body)
	}
	// An invalid policy document.
	if r := e.admin(t, rootCreds, http.MethodPut, "/policies/bad-doc", map[string]any{"document": map[string]any{"Statement": []any{map[string]any{"Effect": "Maybe", "Action": "s3:GetObject", "Resource": "*"}}}}, nil); r.status != http.StatusBadRequest {
		t.Errorf("invalid policy: %d %s", r.status, r.body)
	}
}

// TestIAMGovernanceBypassPermission: bypassing governance retention needs
// s3:BypassGovernanceRetention.
func TestIAMGovernanceBypassPermission(t *testing.T) {
	t.Parallel()
	e := fsEnv
	c := e.root()
	b := bucketName(t)
	_, err := c.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(b), ObjectLockEnabledForBucket: aws.Bool(true)})
	must(t, err, "CreateBucket")
	pname := userName("nobypass")
	e.putPolicy(t, pname, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject*","s3:PutObject*","s3:DeleteObject*","s3:ListBucket*"],"Resource":["arn:aws:s3:::`+b+`","arn:aws:s3:::`+b+`/*"]}]}`)
	user := e.s3Client(e.createUser(t, userName("locker"), pname))
	powerUser := e.s3Client(e.createUser(t, userName("bypasser"), "readwrite"))
	until := time.Now().Add(time.Hour)
	for _, who := range []struct {
		c  *s3.Client
		ok bool
	}{{user, false}, {powerUser, true}} {
		v := e.put(t, b, "locked", []byte("x"), func(in *s3.PutObjectInput) {
			in.ObjectLockMode = s3types.ObjectLockModeGovernance
			in.ObjectLockRetainUntilDate = aws.Time(until)
		}).VersionId
		_, err := who.c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("locked"), VersionId: v, BypassGovernanceRetention: aws.Bool(true)})
		if who.ok {
			allowed(t, err, "bypass with s3:BypassGovernanceRetention")
		} else {
			denied(t, err, "bypass without s3:BypassGovernanceRetention")
		}
	}
}

// TestBucketPolicyPrincipals grants access to specific IAM users through a
// bucket policy (principal ARNs as the web UI templates write them) and
// checks authenticated-read ACLs.
func TestBucketPolicyPrincipals(t *testing.T) {
	t.Parallel()
	e := fsEnv
	b := e.newBucket(t)
	e.put(t, b, "shared/doc", []byte("shared"))
	e.put(t, b, "authread", []byte("signed users only"), func(in *s3.PutObjectInput) { in.ACL = s3types.ObjectCannedACLAuthenticatedRead })
	friendName, strangerName := userName("friend"), userName("stranger")
	friend := e.s3Client(e.createUser(t, friendName))
	stranger := e.s3Client(e.createUser(t, strangerName))
	policy := `{"Version":"2012-10-17","Statement":[
	  {"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::000000000000:user/` + friendName + `"]},"Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::` + b + `/shared/*"},
	  {"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:user/` + friendName + `"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::` + b + `"}]}`
	_, err := e.root().PutBucketPolicy(ctx(t), &s3.PutBucketPolicyInput{Bucket: aws.String(b), Policy: aws.String(policy)})
	must(t, err, "PutBucketPolicy")

	out, err := friend.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("shared/doc")})
	if allowed(t, err, "friend GetObject"); err == nil {
		readAll(t, out.Body)
	}
	_, err = friend.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("shared/new"), Body: stringsReader("x")})
	allowed(t, err, "friend PutObject")
	_, err = friend.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("private"), Body: stringsReader("x")})
	denied(t, err, "friend PutObject outside the policy")
	_, err = friend.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b)})
	allowed(t, err, "friend ListObjectsV2")
	_, err = stranger.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("shared/doc")})
	denied(t, err, "stranger GetObject")

	// authenticated-read: any signed identity reads, anonymous does not.
	out, err = stranger.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("authread")})
	if allowed(t, err, "stranger GetObject authenticated-read"); err == nil {
		readAll(t, out.Body)
	}
	if r := e.anon(t, http.MethodGet, "/"+b+"/authread", nil, nil); r.status != http.StatusForbidden {
		t.Errorf("anonymous GET authenticated-read: %d", r.status)
	}
}

// TestAdminForceDeleteBucket deletes a bucket with versions, delete
// markers and pending uploads through the admin API.
func TestAdminForceDeleteBucket(t *testing.T) {
	t.Parallel()
	e := memEnv
	b := e.newVersionedBucket(t)
	e.put(t, b, "k", []byte("1"))
	e.put(t, b, "k", []byte("2"))
	_, err := e.root().DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("k")})
	must(t, err, "DeleteObject")
	_, err = e.root().CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("up")})
	must(t, err, "CreateMultipartUpload")

	var sum struct {
		Name       string `json:"name"`
		Versioning string `json:"versioning"`
		Usage      *struct {
			Versions       int64 `json:"versions"`
			DeleteMarkers  int64 `json:"deleteMarkers"`
			PendingUploads int64 `json:"pendingUploads"`
		} `json:"usage"`
	}
	if r := e.admin(t, rootCreds, http.MethodGet, "/buckets/"+b, nil, &sum); r.status != 200 || sum.Name != b || sum.Versioning != "Enabled" ||
		sum.Usage == nil || sum.Usage.Versions != 2 || sum.Usage.DeleteMarkers != 1 || sum.Usage.PendingUploads != 1 {
		t.Errorf("GET /buckets/%s: %d %s", b, r.status, r.body)
	}
	// Without force: like S3 DeleteBucket.
	if r := e.admin(t, rootCreds, http.MethodDelete, "/buckets/"+b, nil, nil); r.status != http.StatusConflict {
		t.Errorf("DELETE without force: %d %s", r.status, r.body)
	}
	// A readwrite user lacks admin:ForceDeleteBucket.
	rw := e.createUser(t, userName("rwforce"), "readwrite")
	if r := e.admin(t, rw, http.MethodDelete, "/buckets/"+b+"?force=true", nil, nil); r.status != http.StatusForbidden {
		t.Errorf("DELETE force as readwrite: %d %s", r.status, r.body)
	}
	if r := e.admin(t, rootCreds, http.MethodDelete, "/buckets/"+b+"?force=true", nil, nil); r.status != http.StatusNoContent {
		t.Fatalf("DELETE force: %d %s", r.status, r.body)
	}
	_, err = e.root().HeadBucket(ctx(t), &s3.HeadBucketInput{Bucket: aws.String(b)})
	if httpStatus(err) != http.StatusNotFound {
		t.Errorf("bucket still exists: %v", err)
	}
}
