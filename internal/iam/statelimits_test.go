package iam

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// isLimitErr reports whether err is the limit error, mapped by callers that
// only know the contract errors like ErrTooManyKeys (400).
func isLimitErr(err error) bool {
	return errors.Is(err, ErrLimitExceeded) && errors.Is(err, ErrTooManyKeys)
}

// TestUserLimit: CreateUser refuses a user beyond maxUsers (N-07).
func TestUserLimit(t *testing.T) {
	m := openMem(t)
	ctx := context.Background()
	// Seed the state directly: creating maxUsers users through the API
	// clones the maps each time, which is needlessly slow for a test.
	for i := 0; i < maxUsers-1; i++ {
		name := fmt.Sprintf("seed%d", i)
		m.st.users[name] = &userRec{Name: name}
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "last"}); err != nil {
		t.Fatalf("user %d refused: %v", maxUsers, err)
	}
	_, err := m.CreateUser(ctx, UserSpec{Name: "extra"})
	if !isLimitErr(err) {
		t.Fatalf("user %d: err = %v, want ErrLimitExceeded", maxUsers+1, err)
	}
	if _, err := m.GetUser(ctx, "extra"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("refused user created: %v", err)
	}
	if err := m.DeleteUser(ctx, "last"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "extra"}); err != nil {
		t.Fatalf("user refused after a delete: %v", err)
	}
}

// TestCustomPolicyLimit: PutPolicy refuses a new custom policy beyond
// maxCustomPolicies but still replaces existing ones (N-07). Built-in
// policies do not count.
func TestCustomPolicyLimit(t *testing.T) {
	m := openMem(t)
	ctx := context.Background()
	doc := mustPolicy(t, `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)
	for i := 0; i < maxCustomPolicies-1; i++ {
		name := fmt.Sprintf("seed%d", i)
		m.st.policies[name] = &NamedPolicy{Name: name, Document: cloneDocument(doc)}
	}
	if _, err := m.PutPolicy(ctx, "last", "", doc); err != nil {
		t.Fatalf("policy %d refused: %v", maxCustomPolicies, err)
	}
	_, err := m.PutPolicy(ctx, "extra", "", doc)
	if !isLimitErr(err) {
		t.Fatalf("policy %d: err = %v, want ErrLimitExceeded", maxCustomPolicies+1, err)
	}
	if _, err := m.GetPolicy(ctx, "extra"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("refused policy created: %v", err)
	}
	// Replacing a policy at the cap is not growth.
	if _, err := m.PutPolicy(ctx, "seed0", "updated", doc); err != nil {
		t.Fatalf("replace at the cap refused: %v", err)
	}
	if err := m.DeletePolicy(ctx, "last"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutPolicy(ctx, "extra", "", doc); err != nil {
		t.Fatalf("policy refused after a delete: %v", err)
	}
}

// TestConditionValueLimit: identity policies with more than
// maxConditionValues values for one condition key are refused (N-07).
func TestConditionValueLimit(t *testing.T) {
	doc := func(n int) []byte {
		vals := strings.TrimSuffix(strings.Repeat(`"",`, n), ",")
		return []byte(`{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*",` +
			`"Condition":{"StringEquals":{"aws:UserAgent":[` + vals + `]}}}]}`)
	}
	if _, err := ParsePolicy(doc(maxConditionValues)); err != nil {
		t.Fatalf("%d values refused: %v", maxConditionValues, err)
	}
	if _, err := ParsePolicy(doc(maxConditionValues + 1)); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("%d values: err = %v, want ErrInvalidPolicy", maxConditionValues+1, err)
	}
	// PutPolicy validates documents built in Go the same way.
	d := mustPolicy(t, string(doc(1)))
	vals := make(StringOrSlice, maxConditionValues+1)
	d.Statement[0].Condition["StringEquals"]["aws:UserAgent"] = vals
	if _, err := openMem(t).PutPolicy(context.Background(), "big", "", d); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("PutPolicy with %d values: err = %v, want ErrInvalidPolicy", maxConditionValues+1, err)
	}
}

// TestPersistSizeLimitLeavesStateUnchanged: a mutation whose encoded state
// exceeds maxStateWriteSize is refused with ErrLimitExceeded, leaving the
// in-memory state and iam.json unchanged, so the server can always load
// what it wrote (N-07). Mutations that shrink an oversized state are still
// written.
func TestPersistSizeLimitLeavesStateUnchanged(t *testing.T) {
	if maxStateWriteSize != maxStateFileSize/2 {
		t.Fatalf("maxStateWriteSize = %d, want maxStateFileSize/2", maxStateWriteSize)
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
	path := filepath.Join(dir, FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := maxStateWriteSize
	t.Cleanup(func() { maxStateWriteSize = old })
	maxStateWriteSize = len(before) + 16

	_, err = m.CreateUser(ctx, UserSpec{Name: "bob", Description: strings.Repeat("d", maxDescriptionLen)})
	if !isLimitErr(err) {
		t.Fatalf("oversized state: err = %v, want ErrLimitExceeded", err)
	}
	if _, err := m.GetUser(ctx, "bob"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("refused mutation visible: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("iam.json changed by a refused mutation")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("state dir has %d entries, want only %s", len(entries), FileName)
	}

	// A state already over the limit (written by an older version) can
	// still shrink.
	maxStateWriteSize = 1
	if err := m.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("shrinking an oversized state refused: %v", err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "carol"}); !isLimitErr(err) {
		t.Fatalf("growing an oversized state: err = %v, want ErrLimitExceeded", err)
	}
	maxStateWriteSize = old
	m2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if users, _ := m2.ListUsers(ctx); len(users) != 0 {
		t.Fatalf("reloaded users = %+v", users)
	}
}

// TestReadsDoNotWaitForPersist: authentication and authorization use the
// published state while a mutation is being written, instead of waiting for
// the write and fsync to finish (N-07). Mutations stay serialized and only
// become visible once written.
func TestReadsDoNotWaitForPersist(t *testing.T) {
	ctx := context.Background()
	m, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser(ctx, UserSpec{Name: "alice", Policies: []string{PolicyReadOnly}}); err != nil {
		t.Fatal(err)
	}
	k, err := m.CreateAccessKey(ctx, "alice", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var writes atomic.Int32
	old := writeStateFile
	t.Cleanup(func() { writeStateFile = old })
	writeStateFile = func(path string, data []byte) error {
		writes.Add(1)
		once.Do(func() { close(started) })
		<-release
		return old(path, data)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	bobDone := make(chan error, 1)
	go func() {
		_, err := m.CreateUser(ctx, UserSpec{Name: "bob"})
		bobDone <- err
	}()
	select {
	case <-started:
	case err := <-bobDone:
		t.Fatalf("CreateUser bob finished without writing: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("persist did not start")
	}
	carolDone := make(chan error, 1)
	go func() {
		_, err := m.CreateUser(ctx, UserSpec{Name: "carol"})
		carolDone <- err
	}()

	readsDone := make(chan string, 1)
	go func() {
		secret, id, err := m.LookupCredentials(k.AccessKey)
		if err != nil || secret != k.SecretKey || id.User != "alice" {
			readsDone <- fmt.Sprintf("LookupCredentials = %q %+v %v", secret, id, err)
			return
		}
		if d := m.Authorize(ctx, AuthzRequest{Identity: id, Action: "s3:GetObject", Bucket: "b", Key: "k"}); !d.Allowed {
			readsDone <- fmt.Sprintf("Authorize = %+v", d)
			return
		}
		if _, err := m.GetUser(ctx, "bob"); !errors.Is(err, ErrUserNotFound) {
			readsDone <- fmt.Sprintf("unwritten user visible: %v", err)
			return
		}
		readsDone <- ""
	}()
	select {
	case msg := <-readsDone:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reads blocked while a mutation was being persisted")
	}
	// The second mutation waits for the first one's write.
	select {
	case err := <-carolDone:
		t.Fatalf("concurrent mutation finished while another was being written: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if n := writes.Load(); n != 1 {
		t.Fatalf("%d writes in progress, want 1", n)
	}

	unblock()
	for name, ch := range map[string]chan error{"bob": bobDone, "carol": carolDone} {
		if err := <-ch; err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
	}
	m2, err := Open(Options{Dir: m.opts.Dir})
	if err != nil {
		t.Fatal(err)
	}
	if users, _ := m2.ListUsers(ctx); len(users) != 3 {
		t.Fatalf("reloaded users = %d, want 3", len(users))
	}
}
