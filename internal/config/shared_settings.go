package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// ErrSettingsConflict asks a cluster settings update to reread and retry.
var ErrSettingsConflict = errors.New("config: shared settings changed")

// SettingsStateStore must give linearizable reads and commit a successful
// compare-and-swap through the cluster before returning.
type SettingsStateStore interface {
	Load(context.Context) (data []byte, found bool, err error)
	CompareAndSwap(context.Context, []byte, bool, []byte) error
}

// NewSharedStore keeps settings in consensus. The server must call Refresh
// after a read barrier and before dispatching each request, since Get keeps
// its existing synchronous, local-snapshot API.
func NewSharedStore(initial Settings, shared SettingsStateStore) (*Store, error) {
	if shared == nil {
		return nil, errors.New("config: nil shared settings store")
	}
	initial = normalizeSettings(initial)
	if err := ValidateSettings(initial); err != nil {
		return nil, err
	}
	s := NewStore(initial, "")
	s.shared = shared
	return s, nil
}

// Initialize commits the first node's initial settings exactly once. Later
// nodes adopt that document, even when their local defaults differ.
func (s *Store) Initialize(ctx context.Context) error {
	if s.shared == nil {
		return nil
	}
	_, found, err := s.shared.Load(ctx)
	if err != nil {
		return err
	}
	if !found {
		data, err := json.MarshalIndent(s.initial, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := s.shared.CompareAndSwap(ctx, nil, false, data); err != nil && !errors.Is(err, ErrSettingsConflict) {
			return err
		}
	}
	return s.Refresh(ctx)
}

func (s *Store) loadShared(ctx context.Context) (Settings, []byte, bool, error) {
	data, found, err := s.shared.Load(ctx)
	if err != nil {
		return Settings{}, nil, false, err
	}
	next := cloneSettings(s.initial)
	if found {
		if err := decodeSettings(data, &next); err != nil {
			return Settings{}, nil, false, fmt.Errorf("config: corrupt shared settings: %w", err)
		}
	}
	next = normalizeSettings(next)
	if err := ValidateSettings(next); err != nil {
		return Settings{}, nil, false, fmt.Errorf("config: invalid shared settings: %w", err)
	}
	return next, data, found, nil
}

// Refresh publishes the latest consensus settings and notifies subscribers
// before the caller serves a request. On an unavailable quorum it leaves
// local settings unchanged and returns the error; the caller must return 503.
func (s *Store) Refresh(ctx context.Context) error {
	if s.shared == nil {
		return nil
	}
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	next, _, _, err := s.loadShared(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := cloneSettings(s.settings)
	if reflect.DeepEqual(old, next) {
		s.mu.Unlock()
		return nil
	}
	s.settings = cloneSettings(next)
	subs := append([]func(old, new Settings){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(cloneSettings(old), cloneSettings(next))
	}
	return nil
}

func (s *Store) updateShared(fn func(*Settings) error) (Settings, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 32 {
		current, raw, found, err := s.loadShared(ctx)
		if err != nil {
			return s.Get(), err
		}
		next := cloneSettings(current)
		if err := fn(&next); err != nil {
			return current, err
		}
		next = normalizeSettings(next)
		if err := ValidateSettings(next); err != nil {
			return current, err
		}
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return current, err
		}
		data = append(data, '\n')
		if err := s.shared.CompareAndSwap(ctx, raw, found, data); errors.Is(err, ErrSettingsConflict) {
			continue
		} else if err != nil {
			return current, err
		}
		s.mu.Lock()
		old := cloneSettings(s.settings)
		s.settings = cloneSettings(next)
		subs := append([]func(old, new Settings){}, s.subs...)
		s.mu.Unlock()
		for _, f := range subs {
			f(cloneSettings(old), cloneSettings(next))
		}
		return cloneSettings(next), nil
	}
	return s.Get(), ErrSettingsConflict
}
