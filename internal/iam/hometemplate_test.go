package iam

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestHomeFolderTemplate evaluates the web console's "Home folder per user"
// policy template (prefix-per-user for the bucket "home"; the unit test
// web/test/unit/admin-policies.test.ts checks that the template renders
// testdata/prefix-per-user.json). A user lists the top level one level
// deep (delimiter "/") and everything under its own folder, never another
// user's keys: before, the template allowed the prefix "" without a
// delimiter (every key in the bucket) and the bare prefix "alice", which
// also matches alice2/.
func TestHomeFolderTemplate(t *testing.T) {
	data, err := os.ReadFile("testdata/prefix-per-user.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Options{RootAccessKey: "ROOTKEY", RootSecretKey: "rootsecret", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.PutPolicy(ctx, "home-per-user", "", mustPolicy(t, string(data))); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "alice2"} {
		if _, err := m.CreateUser(ctx, UserSpec{Name: name, Policies: []string{"home-per-user"}}); err != nil {
			t.Fatal(err)
		}
	}
	list := func(query ...string) map[string][]string {
		c := map[string][]string{}
		for i := 0; i+1 < len(query); i += 2 {
			c["s3:"+query[i]] = []string{query[i+1]}
		}
		return c
	}
	alice := user("alice")
	runAuthzCases(t, m, []authzCase{
		{name: "top level with delimiter", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("prefix", "", "delimiter", "/"), want: true},
		{name: "top level without delimiter", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("prefix", ""), want: false},
		{name: "top level other delimiter", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("prefix", "", "delimiter", "a"), want: false},
		{name: "no prefix", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("delimiter", "/"), want: false},
		{name: "own folder", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("prefix", "alice/"), want: true},
		{name: "own subfolder", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("prefix", "alice/docs/", "delimiter", "/"), want: true},
		{name: "bare user name", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("prefix", "alice"), want: false},
		{name: "other user sharing the name's start", id: alice, action: "s3:ListBucket", bucket: "home", cond: list("prefix", "alice2/"), want: false},
		{name: "other user", id: user("alice2"), action: "s3:ListBucket", bucket: "home", cond: list("prefix", "alice/"), want: false},
		{name: "other user's own folder", id: user("alice2"), action: "s3:ListBucket", bucket: "home", cond: list("prefix", "alice2/"), want: true},
		{name: "own object", id: alice, action: "s3:GetObject", bucket: "home", key: "alice/a.txt", want: true},
		{name: "other user's object", id: alice, action: "s3:GetObject", bucket: "home", key: "alice2/a.txt", want: false},
	})
}
