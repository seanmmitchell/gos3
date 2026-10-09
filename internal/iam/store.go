package iam

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// FileName is the name of the IAM state file inside Options.Dir.
//
// The file contains secret access keys (required to verify request
// signatures) and must be treated as sensitive. It is written with mode
// 0600.
const FileName = "iam.json"

// fileVersion is the current iam.json schema version.
const fileVersion = 1

// maxStateFileSize bounds the size of iam.json read at startup.
const maxStateFileSize = 64 << 20

// stateFile is the on-disk schema of iam.json.
type stateFile struct {
	Version  int           `json:"version"`
	Users    []User        `json:"users"`    // keys include secrets
	Policies []NamedPolicy `json:"policies"` // custom policies only
}

// userRec is an immutable user record (copy-on-write).
type userRec struct {
	Name        string
	Description string
	Disabled    bool
	Policies    []string
	Keys        []string // access key ids, in creation order
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// keyRec is an immutable access key record (includes the secret).
type keyRec struct {
	User string
	Key  AccessKey
}

// state is an immutable snapshot of all IAM data. Mutations clone the maps
// (shallow) and replace records, then swap the snapshot under the lock, so
// readers can use a snapshot without holding the lock.
type state struct {
	users    map[string]*userRec
	keys     map[string]*keyRec
	policies map[string]*NamedPolicy // custom policies
}

func newState() *state {
	return &state{
		users:    map[string]*userRec{},
		keys:     map[string]*keyRec{},
		policies: map[string]*NamedPolicy{},
	}
}

func (s *state) clone() *state {
	return &state{
		users:    cloneMap(s.users),
		keys:     cloneMap(s.keys),
		policies: cloneMap(s.policies),
	}
}

func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// policy returns a built-in or custom policy, or nil.
func (s *state) policy(name string) *NamedPolicy {
	if p, ok := builtinPolicies[name]; ok {
		return p
	}
	return s.policies[name]
}

// resolveUser finds the user record of an identity (by name, else by key).
func (s *state) resolveUser(id auth.Identity) *userRec {
	if id.IsAnonymous || id.IsRoot {
		return nil
	}
	name := id.User
	if name == "" && id.AccessKey != "" {
		if k := s.keys[id.AccessKey]; k != nil {
			name = k.User
		}
	}
	if name == "" {
		return nil
	}
	return s.users[name]
}

// open implements Open.
func open(opts Options) (*Manager, error) {
	if (opts.RootAccessKey == "") != (opts.RootSecretKey == "") {
		return nil, errors.New("iam: root access key and secret key must be set together")
	}
	m := &Manager{opts: opts, st: newState()}
	if opts.Dir == "" {
		return m, nil
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("iam: create state directory: %w", err)
	}
	m.path = filepath.Join(opts.Dir, FileName)
	data, err := readLimited(m.path, maxStateFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("iam: read %s: %w", m.path, err)
	}
	st, err := decodeState(data, opts.RootAccessKey)
	if err != nil {
		return nil, fmt.Errorf("iam: load %s: %w", m.path, err)
	}
	m.st = st
	return m, nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file larger than %d bytes", limit)
	}
	return data, nil
}

// decodeState parses and validates iam.json content.
func decodeState(data []byte, rootKey string) (*state, error) {
	var f stateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("unsupported schema version %d (want %d)", f.Version, fileVersion)
	}
	st := newState()
	for i := range f.Policies {
		p := f.Policies[i]
		if !validName(p.Name, maxPolicyNameLen) {
			return nil, fmt.Errorf("invalid policy name %q", p.Name)
		}
		if _, ok := builtinPolicies[p.Name]; ok {
			return nil, fmt.Errorf("policy %q shadows a built-in policy", p.Name)
		}
		if _, dup := st.policies[p.Name]; dup {
			return nil, fmt.Errorf("duplicate policy %q", p.Name)
		}
		if reason := validateIdentityPolicy(&p.Document); reason != "" {
			return nil, fmt.Errorf("policy %q: %s", p.Name, reason)
		}
		p.Builtin = false
		st.policies[p.Name] = &p
	}
	for _, u := range f.Users {
		if !validUserName(u.Name) {
			return nil, fmt.Errorf("invalid user name %q", u.Name)
		}
		if _, dup := st.users[u.Name]; dup {
			return nil, fmt.Errorf("duplicate user %q", u.Name)
		}
		rec := &userRec{
			Name:        u.Name,
			Description: u.Description,
			Disabled:    u.Disabled,
			Policies:    dedupe(u.Policies),
			Keys:        make([]string, 0, len(u.Keys)),
			CreatedAt:   u.CreatedAt,
			UpdatedAt:   u.UpdatedAt,
		}
		for _, k := range u.Keys {
			if !validAccessKey(k.AccessKey) || !validSecretKey(k.SecretKey) {
				return nil, fmt.Errorf("user %q has an invalid access key", u.Name)
			}
			if k.AccessKey == rootKey {
				return nil, fmt.Errorf("access key %s of user %q collides with the root access key", k.AccessKey, u.Name)
			}
			if _, dup := st.keys[k.AccessKey]; dup {
				return nil, fmt.Errorf("duplicate access key %s", k.AccessKey)
			}
			if k.Status != KeyActive && k.Status != KeyInactive {
				return nil, fmt.Errorf("access key %s has invalid status %q", k.AccessKey, k.Status)
			}
			st.keys[k.AccessKey] = &keyRec{User: u.Name, Key: k}
			rec.Keys = append(rec.Keys, k.AccessKey)
		}
		st.users[u.Name] = rec
	}
	return st, nil
}

// encodeState renders st as iam.json content (deterministic order).
func encodeState(st *state) ([]byte, error) {
	f := stateFile{Version: fileVersion, Users: []User{}, Policies: []NamedPolicy{}}
	for _, name := range slices.Sorted(maps.Keys(st.users)) {
		u := st.users[name]
		pu := User{
			Name:        u.Name,
			Description: u.Description,
			Disabled:    u.Disabled,
			Policies:    nonNil(u.Policies),
			Keys:        make([]AccessKey, 0, len(u.Keys)),
			CreatedAt:   u.CreatedAt,
			UpdatedAt:   u.UpdatedAt,
		}
		for _, ak := range u.Keys {
			if k := st.keys[ak]; k != nil {
				pu.Keys = append(pu.Keys, k.Key)
			}
		}
		f.Users = append(f.Users, pu)
	}
	for _, name := range slices.Sorted(maps.Keys(st.policies)) {
		f.Policies = append(f.Policies, *st.policies[name])
	}
	return json.MarshalIndent(f, "", "  ")
}

// persist writes st to iam.json atomically (temp file + fsync + rename +
// directory fsync). It is a no-op for in-memory managers.
func (m *Manager) persist(st *state) error {
	if m.path == "" {
		return nil
	}
	data, err := encodeState(st)
	if err != nil {
		return fmt.Errorf("iam: encode state: %w", err)
	}
	if err := writeFileAtomic(m.path, data); err != nil {
		return fmt.Errorf("iam: write %s: %w", m.path, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// mutate runs fn on a copy of the current state, persists the result and
// publishes it. On any error the current state is left unchanged.
func (m *Manager) mutate(fn func(st *state) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.st.clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := m.persist(next); err != nil {
		return err
	}
	m.st = next
	return nil
}

// snapshot returns the current immutable state.
func (m *Manager) snapshot() *state {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.st
}

func (m *Manager) now() time.Time {
	if m.opts.Now != nil {
		return m.opts.Now().UTC()
	}
	return time.Now().UTC()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// dedupe removes duplicates while preserving order. It always returns a new
// non-nil slice.
func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
