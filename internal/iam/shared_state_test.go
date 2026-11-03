package iam

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
)

type testSharedState struct {
	mu   sync.Mutex
	data []byte
	down bool
}

var errSharedStateDown = errors.New("shared IAM unavailable")

func (s *testSharedState) Load(context.Context) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, false, errSharedStateDown
	}
	return bytes.Clone(s.data), s.data != nil, nil
}

func (s *testSharedState) CompareAndSwap(_ context.Context, prior []byte, found bool, next []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return errSharedStateDown
	}
	if (s.data != nil) != found || !bytes.Equal(s.data, prior) {
		return ErrStateConflict
	}
	s.data = bytes.Clone(next)
	return nil
}

func TestSharedIAMAcrossManagers(t *testing.T) {
	store := &testSharedState{}
	open := func() *Manager {
		m, err := Open(Options{RootAccessKey: "root-key", RootSecretKey: "root-secret", SharedState: store})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := open(), open()
	ctx := context.Background()
	if _, err := a.CreateUser(ctx, UserSpec{Name: "alice", Policies: []string{PolicyReadWrite}}); err != nil {
		t.Fatal(err)
	}
	if user, err := b.GetUser(ctx, "alice"); err != nil || user.Name != "alice" {
		t.Fatalf("follower user read: %+v, %v", user, err)
	}
	key, err := a.CreateAccessKey(ctx, "alice", "", "alice-key", "a-long-enough-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	secret, identity, err := b.LookupCredentials(key.AccessKey)
	if err != nil || secret != key.SecretKey || identity.User != "alice" {
		t.Fatalf("follower credential read: %q, %+v, %v", secret, identity, err)
	}
	if dec := b.Authorize(ctx, AuthzRequest{Identity: identity, Action: "s3:PutObject", Resource: "arn:aws:s3:::photos/a"}); !dec.Allowed {
		t.Fatalf("follower authorization: %+v", dec)
	}
	if err := a.UpdateAccessKey(ctx, key.AccessKey, KeyInactive); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.LookupCredentials(key.AccessKey); err == nil {
		t.Fatal("revoked key remained usable on the other node")
	}
	var wg sync.WaitGroup
	for _, manager := range []*Manager{a, b} {
		manager := manager
		wg.Go(func() {
			name := "bob"
			if manager == b {
				name = "carol"
			}
			if _, err := manager.CreateUser(ctx, UserSpec{Name: name}); err != nil {
				t.Errorf("concurrent shared mutation: %v", err)
			}
		})
	}
	wg.Wait()
	users, err := a.ListUsers(ctx)
	if err != nil || len(users) != 3 {
		t.Fatalf("concurrent users: %+v, %v", users, err)
	}
	store.mu.Lock()
	store.down = true
	store.mu.Unlock()
	if _, _, err := b.LookupCredentials(key.AccessKey); !errors.Is(err, errSharedStateDown) {
		t.Fatalf("unavailable credential lookup: %v", err)
	}
	if dec := b.Authorize(ctx, AuthzRequest{Identity: auth.Identity{User: "alice"}, Action: "s3:PutObject", Resource: "arn:aws:s3:::photos/a"}); dec.Allowed || !dec.ExplicitDeny || !dec.Unavailable {
		t.Fatalf("unavailable authorization did not fail closed: %+v", dec)
	}
	if _, err := a.CreateUser(ctx, UserSpec{Name: "dave"}); !errors.Is(err, errSharedStateDown) {
		t.Fatalf("unavailable mutation: %v", err)
	}
}
