package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

func openMem(t *testing.T) *Manager {
	t.Helper()
	m, err := Open(Options{RootAccessKey: "ROOTKEY", RootSecretKey: "rootsecret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestOpenOptions(t *testing.T) {
	if _, err := Open(Options{RootAccessKey: "x"}); err == nil {
		t.Fatal("root access key without secret accepted")
	}
	m, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.LookupCredentials(""); !errors.Is(err, s3err.ErrInvalidAccessKeyID) {
		t.Fatalf("empty key: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal("Close not idempotent")
	}
}

func TestLookupCredentials(t *testing.T) {
	m := openMem(t)
	ctx := context.Background()
	secret, id, err := m.LookupCredentials("ROOTKEY")
	if err != nil || secret != "rootsecret" || !id.IsRoot || id.User != auth.RootUser || id.AccessKey != "ROOTKEY" {
		t.Fatalf("root lookup = %q %+v %v", secret, id, err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice", Policies: []string{PolicyReadOnly}}); err != nil {
		t.Fatal(err)
	}
	k, err := m.CreateAccessKey(ctx, "alice", "laptop", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Z0-9]{20}$`).MatchString(k.AccessKey) {
		t.Fatalf("generated access key %q", k.AccessKey)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{40}$`).MatchString(k.SecretKey) {
		t.Fatalf("generated secret %q", k.SecretKey)
	}
	if k.Status != KeyActive || k.Description != "laptop" || k.CreatedAt.IsZero() {
		t.Fatalf("key = %+v", k)
	}
	secret, id, err = m.LookupCredentials(k.AccessKey)
	if err != nil || secret != k.SecretKey || id != (auth.Identity{AccessKey: k.AccessKey, User: "alice"}) {
		t.Fatalf("lookup = %q %+v %v", secret, id, err)
	}
	if _, _, err := m.LookupCredentials("NOPE"); !errors.Is(err, s3err.ErrInvalidAccessKeyID) {
		t.Fatalf("unknown key: %v", err)
	}

	// Inactive key.
	if err := m.UpdateAccessKey(ctx, k.AccessKey, KeyInactive); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.LookupCredentials(k.AccessKey); !errors.Is(err, s3err.ErrInvalidAccessKeyID) {
		t.Fatalf("inactive key: %v", err)
	}
	if err := m.UpdateAccessKey(ctx, k.AccessKey, "active"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.LookupCredentials(k.AccessKey); err != nil {
		t.Fatalf("reactivated key: %v", err)
	}

	// Disabled user.
	disabled := true
	if _, err := m.UpdateUser(ctx, "alice", UserUpdate{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.LookupCredentials(k.AccessKey); !errors.Is(err, s3err.ErrInvalidAccessKeyID) {
		t.Fatalf("disabled user: %v", err)
	}
}

func TestUserCRUD(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m, err := Open(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	u, err := m.CreateUser(ctx, UserSpec{Name: "alice", Description: "Alice", Policies: []string{PolicyReadOnly, PolicyReadOnly}})
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "alice" || u.Description != "Alice" || len(u.Policies) != 1 || u.Keys == nil || len(u.Keys) != 0 ||
		!u.CreatedAt.Equal(now) || !u.UpdatedAt.Equal(now) || u.Disabled {
		t.Fatalf("created user = %+v", u)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice"}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if u, err := m.CreateUser(ctx, UserSpec{Name: "bob"}); err != nil || u.Policies == nil {
		t.Fatalf("no policies: %+v %v", u, err)
	}

	for _, bad := range []string{"", "root", "ROOT", "anonymous", "has space", "slash/x", strings.Repeat("a", 65), "ünïcode"} {
		if _, err := m.CreateUser(ctx, UserSpec{Name: bad}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("CreateUser(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "a+b=c,d.e@f_g-h"}); err != nil {
		t.Fatalf("valid punctuation name rejected: %v", err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: strings.Repeat("a", 64)}); err != nil {
		t.Fatalf("64-char name rejected: %v", err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "carol", Policies: []string{"nope"}}); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("unknown policy: %v", err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "carol", Description: strings.Repeat("x", 2000)}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("long description: %v", err)
	}

	got, err := m.GetUser(ctx, "alice")
	if err != nil || got.Name != "alice" {
		t.Fatalf("GetUser: %+v %v", got, err)
	}
	if _, err := m.GetUser(ctx, "ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("GetUser ghost: %v", err)
	}

	list, err := m.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range list {
		names = append(names, u.Name)
	}
	if want := []string{"a+b=c,d.e@f_g-h", strings.Repeat("a", 64), "alice", "bob"}; fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("ListUsers = %v, want %v", names, want)
	}

	// Update.
	now = now.Add(time.Hour)
	desc := "new"
	pols := []string{PolicyReadWrite, PolicyDiagnostics}
	dis := true
	u, err = m.UpdateUser(ctx, "alice", UserUpdate{Description: &desc, Policies: &pols, Disabled: &dis})
	if err != nil {
		t.Fatal(err)
	}
	if u.Description != "new" || fmt.Sprint(u.Policies) != fmt.Sprint(pols) || !u.Disabled || !u.UpdatedAt.Equal(now) || u.CreatedAt.Equal(now) {
		t.Fatalf("updated = %+v", u)
	}
	u, err = m.UpdateUser(ctx, "alice", UserUpdate{})
	if err != nil || u.Description != "new" || len(u.Policies) != 2 {
		t.Fatalf("empty update changed fields: %+v %v", u, err)
	}
	bad := []string{"missing"}
	if _, err := m.UpdateUser(ctx, "alice", UserUpdate{Policies: &bad}); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("update unknown policy: %v", err)
	}
	if u, _ := m.GetUser(ctx, "alice"); len(u.Policies) != 2 {
		t.Fatal("failed update was applied")
	}
	if _, err := m.UpdateUser(ctx, "ghost", UserUpdate{}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("update ghost: %v", err)
	}
	if _, err := m.UpdateUser(ctx, "root", UserUpdate{}); !errors.Is(err, ErrRootImmutable) {
		t.Fatalf("update root: %v", err)
	}
	empty := []string{}
	if u, err := m.UpdateUser(ctx, "alice", UserUpdate{Policies: &empty}); err != nil || len(u.Policies) != 0 || u.Policies == nil {
		t.Fatalf("clear policies: %+v %v", u, err)
	}

	// Delete removes keys.
	k, err := m.CreateAccessKey(ctx, "bob", "", "BOBKEY", "bobsecret1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.LookupCredentials(k.AccessKey); !errors.Is(err, s3err.ErrInvalidAccessKeyID) {
		t.Fatalf("deleted user's key still valid: %v", err)
	}
	if err := m.DeleteAccessKey(ctx, k.AccessKey); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("deleted user's key still exists: %v", err)
	}
	if err := m.DeleteUser(ctx, "bob"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := m.DeleteUser(ctx, "root"); !errors.Is(err, ErrRootImmutable) {
		t.Fatalf("delete root: %v", err)
	}
	// The key id can be reused after deletion.
	if _, err := m.CreateUser(ctx, UserSpec{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateAccessKey(ctx, "bob", "", "BOBKEY", "bobsecret1"); err != nil {
		t.Fatalf("reuse key id: %v", err)
	}
}

func TestAccessKeyCRUD(t *testing.T) {
	ctx := context.Background()
	m := openMem(t)
	for _, n := range []string{"alice", "bob"} {
		if _, err := m.CreateUser(ctx, UserSpec{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	k, err := m.CreateAccessKey(ctx, "alice", "d", "alice.key-1_X", "")
	if err != nil {
		t.Fatal(err)
	}
	if k.AccessKey != "alice.key-1_X" || len(k.SecretKey) != 40 {
		t.Fatalf("key = %+v", k)
	}
	k2, err := m.CreateAccessKey(ctx, "alice", "", "", "my-own-secret!")
	if err != nil || k2.SecretKey != "my-own-secret!" || len(k2.AccessKey) != 20 {
		t.Fatalf("caller secret: %+v %v", k2, err)
	}

	tests := []struct {
		name, user, ak, sk string
		want               error
	}{
		{"unknown user", "ghost", "", "", ErrUserNotFound},
		{"root user", "root", "", "", ErrRootImmutable},
		{"dup across users", "bob", "alice.key-1_X", "", ErrUserExists},
		{"dup root key", "bob", "ROOTKEY", "", ErrKeyExists},
		{"short key", "bob", "ab", "", ErrInvalidName},
		{"long key", "bob", strings.Repeat("a", 129), "", ErrInvalidKey},
		{"bad key chars", "bob", "abc/def", "", ErrInvalidKey},
		{"key spaces", "bob", "abc def", "", ErrInvalidKey},
		{"short secret", "bob", "", "1234567", ErrInvalidKey},
		{"long secret", "bob", "", strings.Repeat("s", 129), ErrInvalidKey},
		{"secret space", "bob", "", "secret with space", ErrInvalidKey},
		{"secret control", "bob", "", "secret\x00\x01xx", ErrInvalidKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := m.CreateAccessKey(ctx, tt.user, "", tt.ak, tt.sk); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}

	// Key limit.
	for i := 0; i < 8; i++ {
		if _, err := m.CreateAccessKey(ctx, "alice", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.CreateAccessKey(ctx, "alice", "", "", ""); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("11th key: %v", err)
	}
	u, _ := m.GetUser(ctx, "alice")
	if len(u.Keys) != 10 {
		t.Fatalf("keys = %d", len(u.Keys))
	}
	for _, k := range u.Keys {
		if k.SecretKey != "" {
			t.Fatal("GetUser exposed a secret")
		}
	}
	if u.Keys[0].AccessKey != "alice.key-1_X" {
		t.Fatalf("key order: %v", u.Keys[0].AccessKey)
	}
	users, _ := m.ListUsers(ctx)
	for _, u := range users {
		for _, k := range u.Keys {
			if k.SecretKey != "" {
				t.Fatal("ListUsers exposed a secret")
			}
		}
	}

	// Update.
	if err := m.UpdateAccessKey(ctx, k.AccessKey, "Disabled"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad status: %v", err)
	}
	if err := m.UpdateAccessKey(ctx, "nope", KeyInactive); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := m.UpdateAccessKey(ctx, "ROOTKEY", KeyInactive); !errors.Is(err, ErrRootImmutable) {
		t.Fatalf("root key update: %v", err)
	}
	if err := m.UpdateAccessKey(ctx, k.AccessKey, KeyInactive); err != nil {
		t.Fatal(err)
	}
	u, _ = m.GetUser(ctx, "alice")
	if u.Keys[0].Status != KeyInactive {
		t.Fatalf("status = %s", u.Keys[0].Status)
	}

	// Delete.
	if err := m.DeleteAccessKey(ctx, "ROOTKEY"); !errors.Is(err, ErrRootImmutable) {
		t.Fatalf("root key delete: %v", err)
	}
	if err := m.DeleteAccessKey(ctx, k.AccessKey); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteAccessKey(ctx, k.AccessKey); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	u, _ = m.GetUser(ctx, "alice")
	if len(u.Keys) != 9 {
		t.Fatalf("keys after delete = %d", len(u.Keys))
	}
	if _, err := m.CreateAccessKey(ctx, "alice", "", "", ""); err != nil {
		t.Fatalf("key after delete: %v", err)
	}
}

func TestPolicyCRUD(t *testing.T) {
	ctx := context.Background()
	m := openMem(t)
	list, err := m.ListPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("builtins = %d", len(list))
	}
	for i, name := range []string{PolicyAdmin, PolicyReadWrite, PolicyReadOnly, PolicyWriteOnly, PolicyDiagnostics} {
		if list[i].Name != name || !list[i].Builtin {
			t.Fatalf("builtin %d = %+v", i, list[i])
		}
		if reason := validateIdentityPolicy(&list[i].Document); reason != "" {
			t.Fatalf("builtin %s invalid: %s", name, reason)
		}
	}
	// Built-ins are read-only, and returned copies cannot alter them.
	list[0].Document.Statement[0].Action[0] = "s3:Nothing"
	if p, _ := m.GetPolicy(ctx, PolicyAdmin); p.Document.Statement[0].Action[0] != "s3:*" {
		t.Fatal("builtin mutated through a returned copy")
	}
	doc := mustPolicy(t, `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)
	if _, err := m.PutPolicy(ctx, PolicyAdmin, "", doc); !errors.Is(err, ErrPolicyBuiltin) {
		t.Fatalf("put builtin: %v", err)
	}
	if err := m.DeletePolicy(ctx, PolicyReadOnly); !errors.Is(err, ErrPolicyBuiltin) {
		t.Fatalf("delete builtin: %v", err)
	}

	p, err := m.PutPolicy(ctx, "reader", "reads", doc)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "reader" || p.Builtin || p.Description != "reads" || p.CreatedAt.IsZero() {
		t.Fatalf("put = %+v", p)
	}
	doc.Statement[0].Action[0] = "s3:PutObject" // caller mutation must not leak in
	got, err := m.GetPolicy(ctx, "reader")
	if err != nil || got.Document.Statement[0].Action[0] != "s3:GetObject" {
		t.Fatalf("stored policy aliased caller memory: %+v %v", got, err)
	}
	time.Sleep(time.Millisecond)
	p2, err := m.PutPolicy(ctx, "reader", "v2", doc)
	if err != nil || !p2.CreatedAt.Equal(p.CreatedAt) || !p2.UpdatedAt.After(p.UpdatedAt) || p2.Description != "v2" {
		t.Fatalf("replace = %+v %v", p2, err)
	}

	for _, bad := range []PolicyDocument{
		{},
		{Statement: []Statement{{Effect: "Allow", Action: StringOrSlice{"s3:*"}}}},
		{Statement: []Statement{{Effect: "Allow", Principal: &Principal{Any: true}, Action: StringOrSlice{"s3:*"}, Resource: StringOrSlice{"*"}}}},
		{Version: "1", Statement: []Statement{{Effect: "Allow", Action: StringOrSlice{"s3:*"}, Resource: StringOrSlice{"*"}}}},
		{Statement: []Statement{{Sid: strings.Repeat("x", MaxPolicySize), Effect: "Allow", Action: StringOrSlice{"s3:*"}, Resource: StringOrSlice{"*"}}}},
	} {
		if _, err := m.PutPolicy(ctx, "bad", "", bad); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("PutPolicy(%+v) = %v, want ErrInvalidPolicy", bad, err)
		}
	}
	for _, name := range []string{"", "a b", strings.Repeat("p", 129)} {
		if _, err := m.PutPolicy(ctx, name, "", doc); !errors.Is(err, ErrInvalidName) {
			t.Errorf("PutPolicy name %q = %v", name, err)
		}
	}
	if _, err := m.GetPolicy(ctx, "bad"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("invalid policy stored: %v", err)
	}

	// In use.
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice", Policies: []string{"reader"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeletePolicy(ctx, "reader"); !errors.Is(err, ErrPolicyInUse) || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("delete in use: %v", err)
	}
	empty := []string{}
	if _, err := m.UpdateUser(ctx, "alice", UserUpdate{Policies: &empty}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeletePolicy(ctx, "reader"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeletePolicy(ctx, "reader"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("delete twice: %v", err)
	}

	// Ordering: builtins, then custom by name.
	for _, n := range []string{"zeta", "alpha"} {
		if _, err := m.PutPolicy(ctx, n, "", doc); err != nil {
			t.Fatal(err)
		}
	}
	list, _ = m.ListPolicies(ctx)
	if len(list) != 7 || list[5].Name != "alpha" || list[6].Name != "zeta" {
		t.Fatalf("order: %v", list)
	}
}

func TestEffectivePolicies(t *testing.T) {
	ctx := context.Background()
	m := openMem(t)
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice", Policies: []string{PolicyReadOnly, PolicyDiagnostics}}); err != nil {
		t.Fatal(err)
	}
	k, _ := m.CreateAccessKey(ctx, "alice", "", "", "")
	ps, err := m.EffectivePolicies(ctx, auth.Identity{User: "alice"})
	if err != nil || len(ps) != 2 || ps[0].Name != PolicyReadOnly || ps[1].Name != PolicyDiagnostics {
		t.Fatalf("alice: %+v %v", ps, err)
	}
	ps, err = m.EffectivePolicies(ctx, auth.Identity{AccessKey: k.AccessKey})
	if err != nil || len(ps) != 2 {
		t.Fatalf("by key: %+v %v", ps, err)
	}
	ps, err = m.EffectivePolicies(ctx, auth.Identity{User: auth.RootUser, IsRoot: true})
	if err != nil || len(ps) != 1 || ps[0].Name != PolicyAdmin {
		t.Fatalf("root: %+v %v", ps, err)
	}
	ps, err = m.EffectivePolicies(ctx, auth.Anonymous)
	if err != nil || ps == nil || len(ps) != 0 {
		t.Fatalf("anonymous: %+v %v", ps, err)
	}
	if _, err := m.EffectivePolicies(ctx, auth.Identity{User: "ghost"}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("ghost: %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	m := openMem(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := m.ListUsers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListUsers: %v", err)
	}
	if _, err := m.ListUsers(nil); err != nil { //nolint:staticcheck // nil context tolerated
		t.Fatalf("nil ctx: %v", err)
	}
}

func TestPersistence(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state", ".gos3")
	opts := Options{Dir: dir, RootAccessKey: "ROOTKEY", RootSecretKey: "rootsecret"}
	m, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("iam.json created before first mutation: %v", err)
	}
	doc := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Sid":"S","Effect":"Allow","Action":["s3:GetObject"],"Resource":"arn:aws:s3:::b/${aws:username}/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8"]}}}]}`)
	if _, err := m.PutPolicy(ctx, "custom", "desc", doc); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice", Description: "A", Policies: []string{"custom", PolicyReadOnly}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "bob", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	k1, _ := m.CreateAccessKey(ctx, "alice", "one", "", "")
	k2, _ := m.CreateAccessKey(ctx, "alice", "two", "ALICE2", "alice-secret-2")
	if err := m.UpdateAccessKey(ctx, k2.AccessKey, KeyInactive); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("iam.json mode = %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if string(f["version"]) != "1" || f["users"] == nil || f["policies"] == nil {
		t.Fatalf("schema: %s", raw)
	}
	if strings.Contains(string(raw), "rootsecret") || strings.Contains(string(raw), "ROOTKEY") {
		t.Fatal("root credentials persisted")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}

	m2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := m2.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice.Description != "A" || fmt.Sprint(alice.Policies) != "[custom readonly]" || len(alice.Keys) != 2 ||
		alice.Keys[0].AccessKey != k1.AccessKey || alice.Keys[1].Status != KeyInactive || alice.Keys[0].Description != "one" {
		t.Fatalf("reloaded alice = %+v", alice)
	}
	secret, id, err := m2.LookupCredentials(k1.AccessKey)
	if err != nil || secret != k1.SecretKey || id.User != "alice" {
		t.Fatalf("reloaded key: %q %+v %v", secret, id, err)
	}
	if _, _, err := m2.LookupCredentials(k2.AccessKey); err == nil {
		t.Fatal("inactive key active after reload")
	}
	if bob, _ := m2.GetUser(ctx, "bob"); !bob.Disabled {
		t.Fatal("bob not disabled after reload")
	}
	p, err := m2.GetPolicy(ctx, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if p.Description != "desc" || fmt.Sprint(p.Document) != fmt.Sprint(doc) {
		t.Fatalf("reloaded policy = %+v", p)
	}
	d := m2.Authorize(ctx, AuthzRequest{Identity: id, Action: "s3:GetObject", Bucket: "b", Key: "alice/x",
		Conditions: map[string][]string{"aws:SourceIp": {"10.0.0.1"}}})
	if !d.Allowed {
		t.Fatalf("reloaded policy not effective: %+v", d)
	}

	// Further mutation after reload, then reload again.
	if err := m2.DeleteUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	m3, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m3.GetUser(ctx, "bob"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("bob after delete+reload: %v", err)
	}

	// Root key colliding with a stored user key is a configuration error.
	if _, err := Open(Options{Dir: dir, RootAccessKey: "ALICE2", RootSecretKey: "x"}); err == nil {
		t.Fatal("root key collision not detected")
	}
}

func TestPersistenceErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	cases := map[string]string{
		"garbage":         `not json`,
		"version":         `{"version":2,"users":[],"policies":[]}`,
		"bad user":        `{"version":1,"users":[{"name":"root"}],"policies":[]}`,
		"dup user":        `{"version":1,"users":[{"name":"a"},{"name":"a"}],"policies":[]}`,
		"bad key":         `{"version":1,"users":[{"name":"a","keys":[{"accessKey":"x","secretKey":"12345678","status":"Active"}]}],"policies":[]}`,
		"no secret":       `{"version":1,"users":[{"name":"a","keys":[{"accessKey":"abc","status":"Active"}]}],"policies":[]}`,
		"bad status":      `{"version":1,"users":[{"name":"a","keys":[{"accessKey":"abc","secretKey":"12345678","status":"On"}]}],"policies":[]}`,
		"dup key":         `{"version":1,"users":[{"name":"a","keys":[{"accessKey":"abc","secretKey":"12345678","status":"Active"}]},{"name":"b","keys":[{"accessKey":"abc","secretKey":"12345678","status":"Active"}]}],"policies":[]}`,
		"builtin shadow":  `{"version":1,"users":[],"policies":[{"name":"admin","document":{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}}]}`,
		"invalid policy":  `{"version":1,"users":[],"policies":[{"name":"p","document":{"Statement":[]}}]}`,
		"bad policy name": `{"version":1,"users":[],"policies":[{"name":"a b","document":{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}}]}`,
		"dup policy":      `{"version":1,"users":[],"policies":[{"name":"p","document":{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}},{"name":"p","document":{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(Options{Dir: dir}); err == nil {
				t.Fatal("expected load error")
			}
		})
	}

	// A valid minimal file loads.
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir}); err != nil {
		t.Fatalf("minimal file: %v", err)
	}
}

func TestPersistFailureLeavesStateUnchanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx := context.Background()
	dir := t.TempDir()
	m, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := m.CreateUser(ctx, UserSpec{Name: "bob"}); err == nil {
		t.Fatal("write to read-only dir succeeded")
	}
	if _, err := m.GetUser(ctx, "bob"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("failed mutation visible: %v", err)
	}
}

func TestPersistFailureRollbackViaFile(t *testing.T) {
	// Replace the state directory with a file so writes fail even as root.
	ctx := context.Background()
	base := t.TempDir()
	dir := filepath.Join(base, "d")
	m, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteUser(ctx, "alice"); err == nil {
		t.Fatal("persist into a file path succeeded")
	}
	if _, err := m.GetUser(ctx, "alice"); err != nil {
		t.Fatalf("failed delete was applied: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	m, err := Open(Options{Dir: t.TempDir(), RootAccessKey: "ROOTKEY", RootSecretKey: "rootsecret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutPolicy(ctx, "p", "", mustPolicy(t, `{"Statement":[{"Effect":"Allow","Action":"s3:Get*","Resource":"*"}]}`)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	const workers = 8
	errs := make(chan error, workers*4)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			name := fmt.Sprintf("user%d", w)
			if _, err := m.CreateUser(ctx, UserSpec{Name: name, Policies: []string{"p"}}); err != nil {
				errs <- err
				return
			}
			for i := 0; i < 5; i++ {
				k, err := m.CreateAccessKey(ctx, name, "", "", "")
				if err != nil {
					errs <- err
					return
				}
				_, id, err := m.LookupCredentials(k.AccessKey)
				if err != nil {
					errs <- err
					return
				}
				d := m.Authorize(ctx, AuthzRequest{Identity: id, Action: "s3:GetObject", Bucket: "b", Key: "k"})
				if !d.Allowed {
					errs <- fmt.Errorf("denied: %+v", d)
					return
				}
				if err := m.DeleteAccessKey(ctx, k.AccessKey); err != nil {
					errs <- err
					return
				}
			}
			if _, err := m.ListUsers(ctx); err != nil {
				errs <- err
			}
			if _, err := m.ListPolicies(ctx); err != nil {
				errs <- err
			}
		}(w)
	}
	// Concurrent readers.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _, _ = m.LookupCredentials("ROOTKEY")
				_ = m.Authorize(ctx, AuthzRequest{Identity: auth.Identity{User: "user1"}, Action: "s3:GetObject", Bucket: "b", Key: "k"})
				_, _ = m.EffectivePolicies(ctx, auth.Identity{User: "user2"})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	users, _ := m.ListUsers(ctx)
	if len(users) != workers {
		t.Fatalf("users = %d", len(users))
	}
	m2, err := Open(Options{Dir: m.opts.Dir})
	if err != nil {
		t.Fatal(err)
	}
	if users2, _ := m2.ListUsers(ctx); len(users2) != workers {
		t.Fatalf("reloaded users = %d", len(users2))
	}
}

func TestGenerateKeys(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		a, err := GenerateAccessKey()
		if err != nil || len(a) != 20 || strings.Trim(a, accessKeyAlphabet) != "" {
			t.Fatalf("access key %q %v", a, err)
		}
		s, err := GenerateSecretKey()
		if err != nil || len(s) != 40 || strings.Trim(s, secretKeyAlphabet) != "" {
			t.Fatalf("secret %q %v", s, err)
		}
		if seen[a] || seen[s] {
			t.Fatal("duplicate random value")
		}
		seen[a], seen[s] = true, true
	}
}
