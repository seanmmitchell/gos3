package iam

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAuthorizeAdminResources checks per-target admin resources (iam-1):
// admin actions on users, policies and buckets are evaluated against
// arn:gos3:admin:::<type>/<name>, while "*" and arn:gos3:admin:::* keep
// granting everything and s3/admin ARNs never cross-match.
func TestAuthorizeAdminResources(t *testing.T) {
	m := newEvalManager(t)
	ctx := context.Background()
	docs := map[string]string{
		"team-users": `{"Statement":[{"Effect":"Allow","Action":"admin:UpdateUser","Resource":"arn:gos3:admin:::user/team-*"}]}`,
		"self":       `{"Statement":[{"Effect":"Allow","Action":"admin:GetUser","Resource":"arn:gos3:admin:::user/${aws:username}"}]}`,
		"attach-ro": `{"Statement":[{"Effect":"Allow","Action":"admin:UpdateUser","Resource":"arn:gos3:admin:::user/*",
			"Condition":{"ForAllValues:StringEquals":{"admin:PolicyName":["readonly"]}}}]}`,
		"pol":       `{"Statement":[{"Effect":"Allow","Action":"admin:PutPolicy","Resource":"arn:gos3:admin:::policy/app-*"}]}`,
		"bkt":       `{"Statement":[{"Effect":"Allow","Action":"admin:GetBucket","Resource":"arn:gos3:admin:::bucket/logs"}]}`,
		"backing":   `{"Statement":[{"Effect":"Allow","Action":"admin:ListRemoteBuckets","Resource":"arn:gos3:admin:::backing/cloud"}]}`,
		"any-type":  `{"Statement":[{"Effect":"Allow","Action":"admin:Get*","Resource":"arn:gos3:admin:::*/shared"}]}`,
		"s3-arns":   `{"Statement":[{"Effect":"Allow","Action":["admin:*","s3:*"],"Resource":["arn:aws:s3:::*","arn:aws:s3:::*/*","arn:*:*:::*"]}]}`,
		"admin-arn": `{"Statement":[{"Effect":"Allow","Action":["admin:*","s3:*"],"Resource":"arn:gos3:admin:::*"}]}`,
		"star":      `{"Statement":[{"Effect":"Allow","Action":["admin:*","s3:*"],"Resource":"*"}]}`,
		"not-res":   `{"Statement":[{"Effect":"Allow","Action":"admin:DeleteUser","NotResource":"arn:gos3:admin:::user/admin-*"}]}`,
		"deny-attach-admin": `{"Statement":[{"Effect":"Allow","Action":"admin:*","Resource":"*"},
			{"Effect":"Deny","Action":"admin:UpdateUser","Resource":"*","Condition":{"ForAnyValue:StringEquals":{"admin:PolicyName":"admin"}}}]}`,
	}
	for name, doc := range docs {
		if _, err := m.PutPolicy(ctx, name, "", mustPolicy(t, doc)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := m.CreateUser(ctx, UserSpec{Name: "u-" + name, Policies: []string{name}}); err != nil {
			t.Fatal(err)
		}
	}
	type tc struct {
		name, user, action, resource, bucket string
		cond                                 map[string][]string
		want, deny                           bool
	}
	cases := []tc{
		{name: "scoped user in", user: "u-team-users", action: "admin:UpdateUser", resource: AdminUserARN("team-a"), want: true},
		{name: "scoped user out", user: "u-team-users", action: "admin:UpdateUser", resource: AdminUserARN("bob")},
		{name: "scoped user self", user: "u-team-users", action: "admin:UpdateUser", resource: AdminUserARN("u-team-users")},
		{name: "scoped user service level", user: "u-team-users", action: "admin:UpdateUser"},
		{name: "scoped user other action", user: "u-team-users", action: "admin:DeleteUser", resource: AdminUserARN("team-a")},
		{name: "scoped user policy type", user: "u-team-users", action: "admin:UpdateUser", resource: AdminPolicyARN("team-a")},
		{name: "variable self", user: "u-self", action: "admin:GetUser", resource: AdminUserARN("u-self"), want: true},
		{name: "variable other", user: "u-self", action: "admin:GetUser", resource: AdminUserARN("bob")},
		{name: "policy name allowed", user: "u-attach-ro", action: "admin:UpdateUser", resource: AdminUserARN("x"),
			cond: map[string][]string{CondAdminPolicyName: {"readonly"}}, want: true},
		{name: "policy name refused", user: "u-attach-ro", action: "admin:UpdateUser", resource: AdminUserARN("x"),
			cond: map[string][]string{CondAdminPolicyName: {"readonly", "admin"}}},
		{name: "policy name absent", user: "u-attach-ro", action: "admin:UpdateUser", resource: AdminUserARN("x"), want: true},
		{name: "deny attaching admin", user: "u-deny-attach-admin", action: "admin:UpdateUser", resource: AdminUserARN("x"),
			cond: map[string][]string{CondAdminPolicyName: {"readonly", "admin"}}, deny: true},
		{name: "deny allows others", user: "u-deny-attach-admin", action: "admin:UpdateUser", resource: AdminUserARN("x"),
			cond: map[string][]string{CondAdminPolicyName: {"readwrite"}}, want: true},
		{name: "policy scoped in", user: "u-pol", action: "admin:PutPolicy", resource: AdminPolicyARN("app-1"), want: true},
		{name: "policy scoped out", user: "u-pol", action: "admin:PutPolicy", resource: AdminPolicyARN("pol")},
		{name: "bucket scoped in", user: "u-bkt", action: "admin:GetBucket", resource: AdminBucketARN("logs"), want: true},
		{name: "bucket scoped out", user: "u-bkt", action: "admin:GetBucket", resource: AdminBucketARN("logs2")},
		{name: "bucket admin ARN not s3", user: "u-bkt", action: "s3:ListBucket", bucket: "logs"},
		{name: "backing scoped in", user: "u-backing", action: "admin:ListRemoteBuckets", resource: AdminBackingARN("cloud"), want: true},
		{name: "backing scoped out", user: "u-backing", action: "admin:ListRemoteBuckets", resource: AdminBackingARN("cloud2")},
		{name: "backing scoped other action", user: "u-backing", action: "admin:AdoptRemoteBucket", resource: AdminBackingARN("cloud")},
		{name: "backing scoped service level", user: "u-backing", action: "admin:ListRemoteBuckets"},
		{name: "backing scoped bucket type", user: "u-backing", action: "admin:ListRemoteBuckets", resource: AdminBucketARN("cloud")},
		{name: "admin ARN grants backing", user: "u-admin-arn", action: "admin:ManageContainers", resource: AdminBackingARN("cloud"), want: true},
		{name: "diagnostics no remote buckets", user: "dave", action: "admin:ListRemoteBuckets", resource: AdminBackingARN("cloud")},
		{name: "any type", user: "u-any-type", action: "admin:GetPolicy", resource: AdminPolicyARN("shared"), want: true},
		{name: "s3 ARNs never grant admin", user: "u-s3-arns", action: "admin:GetUser", resource: AdminUserARN("x")},
		{name: "s3 ARNs never grant admin service", user: "u-s3-arns", action: "admin:ListUsers"},
		{name: "s3 ARNs still grant s3", user: "u-s3-arns", action: "s3:GetObject", bucket: "b", want: true},
		{name: "admin ARN never grants s3", user: "u-admin-arn", action: "s3:GetObject", bucket: "b"},
		{name: "admin ARN never grants s3 service", user: "u-admin-arn", action: "s3:ListAllMyBuckets"},
		{name: "admin ARN grants per target", user: "u-admin-arn", action: "admin:UpdateUser", resource: AdminUserARN("x"), want: true},
		{name: "admin ARN grants service", user: "u-admin-arn", action: "admin:ListUsers", want: true},
		{name: "star grants per target", user: "u-star", action: "admin:DeleteUser", resource: AdminUserARN("x"), want: true},
		{name: "star grants s3", user: "u-star", action: "s3:GetObject", bucket: "b", want: true},
		{name: "admin action ignores bucket", user: "u-s3-arns", action: "admin:GetBucket", bucket: "b"},
		{name: "s3 action ignores resource", user: "u-admin-arn", action: "s3:GetObject", resource: AdminBucketARN("b")},
		{name: "not resource out", user: "u-not-res", action: "admin:DeleteUser", resource: AdminUserARN("admin-1")},
		{name: "not resource in", user: "u-not-res", action: "admin:DeleteUser", resource: AdminUserARN("bob"), want: true},
		{name: "builtin admin per target", user: "admin", action: "admin:UpdateUser", resource: AdminUserARN("x"), want: true},
		{name: "existing arn:gos3:admin:::* grant per target", user: "uadmin", action: "admin:GetUser", resource: AdminUserARN("x"), want: true},
		{name: "diagnostics no probe", user: "dave", action: "admin:ProbeNetwork"},
		{name: "diagnostics discovery", user: "dave", action: "admin:GetDiscovery", want: true},
		// Jobs and whole-bucket snapshots: read-only, never the repository
		// credentials.
		{name: "diagnostics list jobs", user: "dave", action: "admin:ListJobs", want: true},
		{name: "diagnostics get job", user: "dave", action: "admin:GetJob", resource: AdminBucketARN("photos"), want: true},
		{name: "diagnostics get snapshots", user: "dave", action: "admin:GetBucketSnapshots", resource: AdminBucketARN("photos"), want: true},
		{name: "diagnostics no job updates", user: "dave", action: "admin:UpdateJob", resource: AdminBucketARN("photos")},
		{name: "diagnostics no copy jobs", user: "dave", action: "admin:CreateCopyJob", resource: AdminBucketARN("photos")},
		{name: "diagnostics no snapshot changes", user: "dave", action: "admin:PutBucketSnapshots", resource: AdminBucketARN("photos")},
		{name: "diagnostics no restic credentials", user: "dave", action: "admin:GetResticCredentials", resource: AdminBucketARN("photos")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := m.Authorize(ctx, AuthzRequest{Identity: user(c.user), Action: c.action, Resource: c.resource, Bucket: c.bucket, Conditions: c.cond})
			if d.Allowed != c.want || d.ExplicitDeny != c.deny {
				t.Fatalf("decision = %+v, want allowed=%v deny=%v", d, c.want, c.deny)
			}
		})
	}
}

func TestAdminResourceValidation(t *testing.T) {
	for _, r := range []string{
		"arn:gos3:admin:::*", "arn:gos3:admin:::user/alice", "arn:gos3:admin:::user/team-*",
		"arn:gos3:admin:::policy/*", "arn:gos3:admin:::bucket/logs", "arn:gos3:admin:::*/x",
		"arn:gos3:admin:::user/${aws:username}", "arn:gos3:admin:::**",
		"arn:gos3:admin:::backing/cloud", "arn:gos3:admin:::backing/*", "arn:gos3:admin:::back*/c-?",
	} {
		doc := `{"Statement":[{"Effect":"Allow","Action":"admin:*","Resource":"` + r + `"}]}`
		if _, err := ParsePolicy([]byte(doc)); err != nil {
			t.Errorf("%s: %v", r, err)
		}
	}
	m := newEvalManager(t)
	for _, r := range []string{
		"arn:gos3:admin:::users/alice", "arn:gos3:admin:::user/", "arn:gos3:admin:::alice",
		"arn:gos3:admin:::backings/cloud", "arn:gos3:admin:::backing/",
		"arn:gos3:admin:us-east-1::*", "arn:gos3:s3:::*",
	} {
		doc := `{"Statement":[{"Effect":"Deny","Action":"admin:*","NotResource":"` + r + `"}]}`
		_, err := ParsePolicy([]byte(doc))
		if !errors.Is(err, ErrInvalidPolicy) || !strings.Contains(err.Error(), "admin resources are") {
			t.Errorf("%s: err = %v", r, err)
		}
		if _, err := m.PutPolicy(context.Background(), "p", "", PolicyDocument{Statement: []Statement{{
			Effect: "Allow", Action: StringOrSlice{"admin:*"}, Resource: StringOrSlice{r},
		}}}); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("PutPolicy %s: err = %v", r, err)
		}
	}
	// Stored documents that predate the check still load.
	data := []byte(`{"version":1,"policies":[{"name":"old","document":{"Statement":[{"Effect":"Allow","Action":"admin:*","Resource":"arn:gos3:admin:::users/x"}]}}],"users":[]}`)
	if _, err := decodeState(data, "ROOT"); err != nil {
		t.Errorf("decodeState: %v", err)
	}

	for r, want := range map[string]bool{
		"": true, AdminUserARN("a"): true, AdminPolicyARN("p"): true, AdminBucketARN("b"): true, AdminBackingARN("cloud"): true,
		"arn:gos3:admin:::backing/": false,
		"arn:gos3:admin:::*":        false, "arn:gos3:admin:::user/": false, "arn:aws:s3:::b": false, "arn:gos3:admin:::key/x": false,
	} {
		if got := ValidAdminRequestResource(r); got != want {
			t.Errorf("ValidAdminRequestResource(%q) = %v", r, got)
		}
	}
}

func TestAccessKeyOwner(t *testing.T) {
	m := newEvalManager(t)
	ctx := context.Background()
	k, err := m.CreateAccessKey(ctx, "alice", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateAccessKey(ctx, k.AccessKey, KeyInactive); err != nil {
		t.Fatal(err)
	}
	if owner, err := m.AccessKeyOwner(ctx, k.AccessKey); err != nil || owner != "alice" {
		t.Errorf("owner = %q, %v", owner, err)
	}
	for _, ak := range []string{"NOSUCHKEY", "ROOTKEY", ""} {
		if _, err := m.AccessKeyOwner(ctx, ak); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("%q: err = %v", ak, err)
		}
	}
}
