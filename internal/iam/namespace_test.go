package iam

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

// TestKeyIDNamespace checks that access key IDs and user names stay apart
// (iam-1, iam-v1, admin-1): a chosen key ID equal to another user's name,
// "root" or a 12-digit account ID used to inherit the bucket-policy grants
// written for that user, root or the account.
func TestKeyIDNamespace(t *testing.T) {
	ctx := context.Background()
	m := openMem(t)
	for _, n := range []string{"alice", "team-bob"} {
		if _, err := m.CreateUser(ctx, UserSpec{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ak := range []string{"alice", "root", "ROOT", "anonymous", "Anonymous", "123456789012"} {
		if _, err := m.CreateAccessKey(ctx, "team-bob", "", ak, ""); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("CreateAccessKey(team-bob, %q) = %v, want ErrInvalidKey", ak, err)
		}
	}
	// Longer or shorter digit strings are not account IDs.
	if _, err := m.CreateAccessKey(ctx, "team-bob", "", "1234567890123", ""); err != nil {
		t.Errorf("13-digit key ID: %v", err)
	}
	if _, err := m.CreateAccessKey(ctx, "team-bob", "", "BOBKEY", ""); err != nil {
		t.Fatal(err)
	}

	// The other direction: user names must not equal key IDs (the root key
	// included) or account IDs.
	for _, tc := range []struct {
		name string
		want error
	}{
		{"BOBKEY", ErrUserExists},
		{"ROOTKEY", ErrUserExists},
		{"123456789012", ErrInvalidName},
	} {
		if _, err := m.CreateUser(ctx, UserSpec{Name: tc.name}); !errors.Is(err, tc.want) {
			t.Errorf("CreateUser(%q) = %v, want %v", tc.name, err, tc.want)
		}
	}
	// A deleted key frees its ID for a user name and vice versa.
	if err := m.DeleteAccessKey(ctx, "BOBKEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "BOBKEY"}); err != nil {
		t.Errorf("CreateUser after key deletion: %v", err)
	}
}

// TestPrincipalNamespaces checks bucket-policy principal matching: a bare
// 12-digit entry is only the account principal, and a bare entry matches
// the caller's access key ID only when that ID is unambiguous and the
// caller's own. Identities with colliding key IDs are built by hand: they
// can only come from an iam.json written by an older version.
func TestPrincipalNamespaces(t *testing.T) {
	ctx := context.Background()
	m := newEvalManager(t)
	if _, err := m.CreateUser(ctx, UserSpec{Name: "team-bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateAccessKey(ctx, "team-bob", "", "TEAMBOBKEY", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateAccessKey(ctx, "nobody", "", "NOBODYKEY", ""); err != nil {
		t.Fatal(err)
	}
	// "nobody" (no policies) is the user the policies are written for.
	bp := mustBucketPolicy(t, "secret", `{"Statement":[
		{"Sid":"NobodyOnly","Effect":"Allow","Principal":{"AWS":"nobody"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::secret/*"},
		{"Sid":"Account","Effect":"Allow","Principal":{"AWS":"123456789012"},"Action":"s3:DeleteObject","Resource":"arn:aws:s3:::secret/*"},
		{"Sid":"Root","Effect":"Allow","Principal":{"AWS":"root"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::secret/*"},
		{"Sid":"Key","Effect":"Allow","Principal":{"AWS":"TEAMBOBKEY"},"Action":"s3:GetObjectTagging","Resource":"arn:aws:s3:::secret/*"},
		{"Sid":"NobodyKey","Effect":"Allow","Principal":{"AWS":"NOBODYKEY"},"Action":"s3:GetObjectAcl","Resource":"arn:aws:s3:::secret/*"}]}`)
	vault := mustBucketPolicy(t, "vault", `{"Statement":[
		{"Sid":"Read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::vault/*"},
		{"Sid":"OnlyNobodyAndRoot","Effect":"Deny","NotPrincipal":{"AWS":["nobody","root"]},"Action":"s3:*","Resource":"arn:aws:s3:::vault/*"}]}`)
	bob := func(ak string) auth.Identity { return auth.Identity{User: "team-bob", AccessKey: ak} }
	runAuthzCases(t, m, []authzCase{
		{name: "name principal", id: user("nobody"), action: "s3:GetObject", bucket: "secret", key: "k", bp: bp, want: true, wantReason: "Sid=NobodyOnly"},
		{name: "key ID equal to a user name", id: bob("nobody"), action: "s3:GetObject", bucket: "secret", key: "k", bp: bp, want: false},
		{name: "key ID equal to root", id: bob("root"), action: "s3:PutObject", bucket: "secret", key: "k", bp: bp, want: false},
		{name: "key ID equal to the account", id: bob("123456789012"), action: "s3:DeleteObject", bucket: "secret", key: "k", bp: bp, want: false},
		{name: "account allow grants IAM users nothing", id: user("nobody"), action: "s3:DeleteObject", bucket: "secret", key: "k", bp: bp, want: false},
		{name: "own key principal", id: bob("TEAMBOBKEY"), action: "s3:GetObjectTagging", bucket: "secret", key: "k", bp: bp, want: true, wantReason: "Sid=Key"},
		{name: "another user's key ID", id: bob("NOBODYKEY"), action: "s3:GetObjectAcl", bucket: "secret", key: "k", bp: bp, want: false},
		{name: "not principal key ID equal to a user name", id: bob("nobody"), action: "s3:GetObject", bucket: "vault", key: "k", bp: vault, want: false, wantDeny: true},
		{name: "not principal key ID equal to root", id: bob("root"), action: "s3:GetObject", bucket: "vault", key: "k", bp: vault, want: false, wantDeny: true},
		{name: "not principal named user", id: user("nobody"), action: "s3:GetObject", bucket: "vault", key: "k", bp: vault, want: true},
	})

	// A 12-digit user name (possible in an older iam.json) is not matched
	// by a bare account entry either.
	digits := auth.Identity{User: "123456789012", AccessKey: "DIGITSKEY"}
	if d := m.Authorize(ctx, AuthzRequest{Identity: digits, Action: "s3:DeleteObject", Bucket: "secret", Key: "k", BucketPolicy: bp}); d.Allowed {
		t.Errorf("12-digit user name matched the account principal: %+v", d)
	}
}

// TestLegacyAmbiguousKeys checks that an iam.json written before the
// namespaces were separated still loads, and that its ambiguous keys are
// refused at authentication while the others keep working.
func TestLegacyAmbiguousKeys(t *testing.T) {
	dir := t.TempDir()
	const legacy = `{"version":1,"policies":[],"users":[
		{"name":"alice","policies":[],"keys":[{"accessKey":"ALICEKEY","secretKey":"12345678","status":"Active"}]},
		{"name":"123456789012","policies":[],"keys":[]},
		{"name":"team-bob","policies":[],"keys":[
			{"accessKey":"alice","secretKey":"12345678","status":"Active"},
			{"accessKey":"root","secretKey":"12345678","status":"Active"},
			{"accessKey":"888888888888","secretKey":"12345678","status":"Active"},
			{"accessKey":"BOBKEY","secretKey":"12345678","status":"Active"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, err := Open(Options{Dir: dir, RootAccessKey: "ROOTKEY", RootSecretKey: "rootsecret"})
	if err != nil {
		t.Fatalf("Open legacy iam.json: %v", err)
	}
	for _, want := range []string{"accessKey=alice", "accessKey=root", "accessKey=888888888888", "user=123456789012"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("load log lacks %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "BOBKEY") {
		t.Errorf("unambiguous key logged:\n%s", logs.String())
	}
	logs.Reset()
	for _, ak := range []string{"alice", "root", "888888888888"} {
		if _, _, err := m.LookupCredentials(ak); !errors.Is(err, s3err.ErrInvalidAccessKeyID) {
			t.Errorf("LookupCredentials(%q) = %v, want InvalidAccessKeyId", ak, err)
		}
	}
	if n := strings.Count(logs.String(), "refusing access key"); n != 3 {
		t.Errorf("refusals logged %d times, want 3 (once per key):\n%s", n, logs.String())
	}
	for _, ak := range []string{"alice", "root"} { // logged only once
		_, _, _ = m.LookupCredentials(ak)
	}
	if n := strings.Count(logs.String(), "refusing access key"); n != 3 {
		t.Errorf("repeated refusals logged again (%d lines)", n)
	}
	for ak, owner := range map[string]string{"ALICEKEY": "alice", "BOBKEY": "team-bob"} {
		if _, id, err := m.LookupCredentials(ak); err != nil || id.User != owner {
			t.Errorf("LookupCredentials(%q) = %+v, %v", ak, id, err)
		}
	}
	// The refused key can still be deleted.
	if err := m.DeleteAccessKey(context.Background(), "alice"); err != nil {
		t.Errorf("DeleteAccessKey: %v", err)
	}
}

// TestNotResourceNamespace checks iam-2: an Allow with NotResource only
// covers the namespaces it names, so "everything except bucket secret"
// written with Action "*" grants no admin action, as docs/security.md
// promises. A Deny keeps the AWS reading.
func TestNotResourceNamespace(t *testing.T) {
	ctx := context.Background()
	m := openMem(t)
	pols := map[string]string{
		"allbutsecret": `{"Statement":[{"Effect":"Allow","Action":"*","NotResource":["arn:aws:s3:::secret","arn:aws:s3:::secret/*"]}]}`,
		"allbutboss":   `{"Statement":[{"Effect":"Allow","Action":"*","NotResource":"arn:gos3:admin:::user/boss"}]}`,
		"mixed":        `{"Statement":[{"Effect":"Allow","Action":"*","NotResource":["arn:aws:s3:::secret/*","arn:gos3:admin:::user/boss"]}]}`,
		"denyoutside":  `{"Statement":[{"Effect":"Deny","Action":"*","NotResource":"arn:aws:s3:::public/*"}]}`,
	}
	for name, doc := range pols {
		if _, err := m.PutPolicy(ctx, name, "", mustPolicy(t, doc)); err != nil {
			t.Fatal(err)
		}
	}
	users := map[string][]string{
		"app":     {"allbutsecret"},
		"ops":     {"allbutboss"},
		"mix":     {"mixed"},
		"limited": {PolicyAdmin, "denyoutside"},
	}
	for name, p := range users {
		if _, err := m.CreateUser(ctx, UserSpec{Name: name, Policies: p}); err != nil {
			t.Fatal(err)
		}
	}
	type tc struct {
		user, action, bucket, key, resource string
		want                                bool
	}
	for _, c := range []tc{
		{"app", "s3:GetObject", "data", "k", "", true},
		{"app", "s3:GetObject", "secret", "k", "", false},
		{"app", "admin:UpdateUser", "", "", AdminUserARN("app"), false},
		{"app", "admin:PutPolicy", "", "", AdminPolicyARN("allbutsecret"), false},
		{"app", "admin:CreateAccessKey", "", "", AdminUserARN("app"), false},
		{"app", "admin:UpdateConfig", "", "", "", false},
		{"ops", "admin:UpdateUser", "", "", AdminUserARN("team-a"), true},
		{"ops", "admin:UpdateUser", "", "", AdminUserARN("boss"), false},
		{"ops", "s3:GetObject", "data", "k", "", false},
		{"mix", "s3:GetObject", "data", "k", "", true},
		{"mix", "s3:GetObject", "secret", "k", "", false},
		{"mix", "admin:GetUser", "", "", AdminUserARN("x"), true},
		{"mix", "admin:GetUser", "", "", AdminUserARN("boss"), false},
		// Deny with NotResource applies to everything not listed.
		{"limited", "s3:GetObject", "public", "k", "", true},
		{"limited", "s3:GetObject", "data", "k", "", false},
		{"limited", "admin:UpdateUser", "", "", AdminUserARN("x"), false},
	} {
		id := auth.Identity{User: c.user}
		d := m.Authorize(ctx, AuthzRequest{Identity: id, Action: c.action, Bucket: c.bucket, Key: c.key, Resource: c.resource})
		if d.Allowed != c.want {
			t.Errorf("%s %s %s%s/%s: allowed = %v (%s), want %v", c.user, c.action, c.resource, c.bucket, c.key, d.Allowed, d.Reason, c.want)
		}
	}
}

// TestOwnerBoundKeyChanges checks that UpdateUserAccessKey and
// DeleteUserAccessKey act only on a key that belongs to the given owner
// when the change is applied (the admin key routes authorize against the
// owner first, then mutate).
func TestOwnerBoundKeyChanges(t *testing.T) {
	ctx := context.Background()
	m := openMem(t)
	for _, n := range []string{"team-a", "boss"} {
		if _, err := m.CreateUser(ctx, UserSpec{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.CreateAccessKey(ctx, "boss", "", "SHAREDID", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateUserAccessKey(ctx, "team-a", "SHAREDID", KeyInactive); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("update with the wrong owner: %v", err)
	}
	if err := m.UpdateUserAccessKey(ctx, "", "SHAREDID", KeyInactive); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("update with no owner: %v", err)
	}
	if err := m.DeleteUserAccessKey(ctx, "team-a", "SHAREDID"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("delete with the wrong owner: %v", err)
	}
	if _, _, err := m.LookupCredentials("SHAREDID"); err != nil {
		t.Fatalf("key changed by a caller bound to another owner: %v", err)
	}
	if err := m.UpdateUserAccessKey(ctx, "boss", "SHAREDID", KeyInactive); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteUserAccessKey(ctx, "boss", "SHAREDID"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AccessKeyOwner(ctx, "SHAREDID"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("key still exists: %v", err)
	}
}
