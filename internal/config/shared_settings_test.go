package config

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeSettingsState struct {
	mu   sync.Mutex
	data []byte
	down bool
}

var errSettingsDown = errors.New("settings quorum unavailable")

func (f *fakeSettingsState) Load(context.Context) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, false, errSettingsDown
	}
	return bytes.Clone(f.data), f.data != nil, nil
}

func (f *fakeSettingsState) CompareAndSwap(_ context.Context, prior []byte, found bool, next []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errSettingsDown
	}
	if (f.data != nil) != found || !bytes.Equal(f.data, prior) {
		return ErrSettingsConflict
	}
	f.data = bytes.Clone(next)
	return nil
}

func TestSharedSettingsInitializeUpdateRefresh(t *testing.T) {
	ctx := context.Background()
	state := &fakeSettingsState{}
	first, err := NewSharedStore(validSettings(), state)
	if err != nil {
		t.Fatal(err)
	}
	otherDefault := validSettings()
	otherDefault.Name = "different-local-default"
	second, err := NewSharedStore(otherDefault, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if second.Get().Name != first.Get().Name {
		t.Fatalf("second node retained local defaults: %q", second.Get().Name)
	}
	var seen []string
	second.Subscribe(func(old, next Settings) { seen = append(seen, old.Name+">"+next.Name) })
	if _, err := first.Update(func(s *Settings) error { s.Name = "shared-name"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := second.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if second.Get().Name != "shared-name" || len(seen) != 1 {
		t.Fatalf("peer refresh: %+v, notifications %v", second.Get(), seen)
	}
	var wg sync.WaitGroup
	for _, store := range []*Store{first, second} {
		store := store
		wg.Go(func() {
			if _, err := store.Update(func(s *Settings) error {
				if store == first {
					s.PublicInfo = true
				} else {
					s.MetricsPublic = true
				}
				return nil
			}); err != nil {
				t.Errorf("concurrent update: %v", err)
			}
		})
	}
	wg.Wait()
	if err := second.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !second.Get().PublicInfo || !second.Get().MetricsPublic {
		t.Fatalf("concurrent settings update lost a field: %+v", second.Get())
	}
	state.mu.Lock()
	state.down = true
	state.mu.Unlock()
	if err := second.Refresh(ctx); !errors.Is(err, errSettingsDown) {
		t.Fatalf("unavailable refresh: %v", err)
	}
	if _, err := first.Update(func(s *Settings) error { s.Name = "unsafe"; return nil }); !errors.Is(err, errSettingsDown) {
		t.Fatalf("unavailable write: %v", err)
	}
}
