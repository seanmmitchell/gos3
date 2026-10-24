package iam

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// lookupCredentials implements Manager.LookupCredentials.
func (m *Manager) lookupCredentials(accessKey string) (string, auth.Identity, error) {
	if accessKey == "" {
		return "", auth.Identity{}, s3err.ErrInvalidAccessKeyID
	}
	if m.opts.RootAccessKey != "" && accessKey == m.opts.RootAccessKey {
		return m.opts.RootSecretKey, auth.Identity{AccessKey: accessKey, User: auth.RootUser, IsRoot: true}, nil
	}
	st := m.snapshot()
	k := st.keys[accessKey]
	if k == nil || k.Key.Status != KeyActive {
		return "", auth.Identity{}, s3err.ErrInvalidAccessKeyID
	}
	if why := keyIDConflict(st, accessKey); why != "" {
		// Only an iam.json written before the namespaces were separated can
		// hold such a key: refuse it rather than let it match bucket-policy
		// principals naming the user, root or the account.
		m.warnOnce("conflict:"+accessKey, "iam: refusing access key: its ID is ambiguous in bucket-policy principals; "+
			"delete it and create a new key", "accessKey", accessKey, "user", k.User, "reason", why)
		return "", auth.Identity{}, s3err.ErrInvalidAccessKeyID
	}
	u := st.users[k.User]
	if u == nil || u.Disabled {
		return "", auth.Identity{}, s3err.ErrInvalidAccessKeyID
	}
	return k.Key.SecretKey, auth.Identity{AccessKey: accessKey, User: u.Name}, nil
}

// publicUser renders a user record without secrets.
func publicUser(st *state, u *userRec) User {
	out := User{
		Name:        u.Name,
		Description: u.Description,
		Disabled:    u.Disabled,
		Policies:    append([]string{}, u.Policies...),
		Keys:        make([]AccessKey, 0, len(u.Keys)),
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
	for _, ak := range u.Keys {
		if k := st.keys[ak]; k != nil {
			pk := k.Key
			pk.SecretKey = ""
			out.Keys = append(out.Keys, pk)
		}
	}
	return out
}

func (m *Manager) listUsers(ctx context.Context) ([]User, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	st := m.snapshot()
	out := make([]User, 0, len(st.users))
	for _, name := range slices.Sorted(maps.Keys(st.users)) {
		out = append(out, publicUser(st, st.users[name]))
	}
	return out, nil
}

func (m *Manager) getUser(ctx context.Context, name string) (User, error) {
	if err := ctxErr(ctx); err != nil {
		return User{}, err
	}
	st := m.snapshot()
	u := st.users[name]
	if u == nil {
		return User{}, ErrUserNotFound
	}
	return publicUser(st, u), nil
}

// checkPolicies verifies that every named policy exists.
func checkPolicies(st *state, names []string) error {
	for _, p := range names {
		if st.policy(p) == nil {
			return wrapf(ErrPolicyNotFound, p)
		}
	}
	return nil
}

func checkDescription(d string) error {
	if len(d) > maxDescriptionLen {
		return wrapf(ErrInvalidName, fmt.Sprintf("description longer than %d bytes", maxDescriptionLen))
	}
	return nil
}

func (m *Manager) createUser(ctx context.Context, spec UserSpec) (User, error) {
	if err := ctxErr(ctx); err != nil {
		return User{}, err
	}
	if isReservedUserName(spec.Name) {
		return User{}, wrapf(ErrInvalidName, fmt.Sprintf("user name %q is reserved", spec.Name))
	}
	if !validName(spec.Name, maxUserNameLen) {
		return User{}, wrapf(ErrInvalidName, "user names must be 1-64 characters of [A-Za-z0-9+=,.@_-]")
	}
	if isDotSegment(spec.Name) {
		return User{}, wrapf(ErrInvalidName, fmt.Sprintf("user name %q is not allowed", spec.Name))
	}
	if isAccountID(spec.Name) {
		// A bare 12-digit principal names the account, never a user.
		return User{}, wrapf(ErrInvalidName, "user names must not be 12-digit account IDs")
	}
	if err := checkDescription(spec.Description); err != nil {
		return User{}, err
	}
	var out User
	err := m.mutate(func(st *state) error {
		// A name taken by an access key (see keyIDConflict) is reported like
		// an existing user, so the answer does not tell which one it is.
		if st.users[spec.Name] != nil || st.keys[spec.Name] != nil || spec.Name == m.opts.RootAccessKey {
			return ErrUserExists
		}
		if len(st.users) >= maxUsers {
			return wrapf(ErrLimitExceeded, fmt.Sprintf("at most %d users", maxUsers))
		}
		pols := dedupe(spec.Policies)
		if err := checkPolicies(st, pols); err != nil {
			return err
		}
		now := m.now()
		u := &userRec{
			Name:        spec.Name,
			Description: spec.Description,
			Disabled:    spec.Disabled,
			Policies:    pols,
			Keys:        []string{},
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		st.users[u.Name] = u
		out = publicUser(st, u)
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return out, nil
}

func (m *Manager) updateUser(ctx context.Context, name string, upd UserUpdate) (User, error) {
	if err := ctxErr(ctx); err != nil {
		return User{}, err
	}
	if strings.EqualFold(name, auth.RootUser) {
		return User{}, ErrRootImmutable
	}
	if upd.Description != nil {
		if err := checkDescription(*upd.Description); err != nil {
			return User{}, err
		}
	}
	var out User
	err := m.mutate(func(st *state) error {
		old := st.users[name]
		if old == nil {
			return ErrUserNotFound
		}
		u := *old
		if upd.Description != nil {
			u.Description = *upd.Description
		}
		if upd.Disabled != nil {
			u.Disabled = *upd.Disabled
		}
		if upd.Policies != nil {
			pols := dedupe(*upd.Policies)
			if err := checkPolicies(st, pols); err != nil {
				return err
			}
			u.Policies = pols
		}
		u.UpdatedAt = m.now()
		st.users[name] = &u
		out = publicUser(st, &u)
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return out, nil
}

func (m *Manager) deleteUser(ctx context.Context, name string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if strings.EqualFold(name, auth.RootUser) {
		return ErrRootImmutable
	}
	return m.mutate(func(st *state) error {
		u := st.users[name]
		if u == nil {
			return ErrUserNotFound
		}
		for _, ak := range u.Keys {
			delete(st.keys, ak)
		}
		delete(st.users, name)
		return nil
	})
}

func (m *Manager) createAccessKey(ctx context.Context, user, description, accessKey, secretKey string) (AccessKey, error) {
	if err := ctxErr(ctx); err != nil {
		return AccessKey{}, err
	}
	if strings.EqualFold(user, auth.RootUser) {
		return AccessKey{}, ErrRootImmutable
	}
	if err := checkDescription(description); err != nil {
		return AccessKey{}, err
	}
	if accessKey != "" && !validAccessKey(accessKey) {
		return AccessKey{}, wrapf(ErrInvalidKey, "access keys must be 3-128 characters of [A-Za-z0-9._-]")
	}
	if accessKey != "" && (isReservedUserName(accessKey) || isAccountID(accessKey)) {
		return AccessKey{}, wrapf(ErrInvalidKey, "access keys must not be a reserved user name (root, anonymous) or a 12-digit account ID")
	}
	if secretKey != "" && !validNewSecretKey(secretKey) {
		return AccessKey{}, wrapf(ErrInvalidKey, fmt.Sprintf("secret keys must be %d-128 printable ASCII characters without spaces", minNewSecretKeyLen))
	}
	if secretKey == "" {
		var err error
		if secretKey, err = GenerateSecretKey(); err != nil {
			return AccessKey{}, err
		}
	}
	var out AccessKey
	err := m.mutate(func(st *state) error {
		old := st.users[user]
		if old == nil {
			return ErrUserNotFound
		}
		if len(old.Keys) >= maxKeysPerUser {
			return ErrTooManyKeys
		}
		ak := accessKey
		if ak == "" {
			// Generated keys collide with negligible probability; retry anyway.
			for range 8 {
				g, err := GenerateAccessKey()
				if err != nil {
					return err
				}
				if st.keys[g] == nil && g != m.opts.RootAccessKey && keyIDConflict(st, g) == "" {
					ak = g
					break
				}
			}
			if ak == "" {
				return ErrKeyExists
			}
		} else if st.keys[ak] != nil || ak == m.opts.RootAccessKey {
			return ErrKeyExists
		} else if keyIDConflict(st, ak) != "" {
			return wrapf(ErrInvalidKey, "access keys must not equal a user name")
		}
		key := AccessKey{
			AccessKey:   ak,
			SecretKey:   secretKey,
			Status:      KeyActive,
			Description: description,
			CreatedAt:   m.now(),
		}
		st.keys[ak] = &keyRec{User: user, Key: key}
		u := *old
		u.Keys = append(append(make([]string, 0, len(old.Keys)+1), old.Keys...), ak)
		u.UpdatedAt = key.CreatedAt
		st.users[user] = &u
		out = key
		return nil
	})
	if err != nil {
		return AccessKey{}, err
	}
	return out, nil
}

// updateAccessKey sets the status of a key. A non-empty owner binds the
// change to that user: if the key does not belong to owner when the change
// is applied (it was deleted and re-created for someone else after the
// caller was authorized against owner), ErrKeyNotFound is returned.
func (m *Manager) updateAccessKey(ctx context.Context, owner, accessKey, status string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if m.opts.RootAccessKey != "" && accessKey == m.opts.RootAccessKey {
		return ErrRootImmutable
	}
	norm, ok := normalizeKeyStatus(status)
	if !ok {
		return wrapf(ErrInvalidKey, fmt.Sprintf("status must be %s or %s", KeyActive, KeyInactive))
	}
	return m.mutate(func(st *state) error {
		k := st.keys[accessKey]
		if k == nil || (owner != "" && k.User != owner) {
			return ErrKeyNotFound
		}
		nk := *k
		nk.Key.Status = norm
		st.keys[accessKey] = &nk
		return nil
	})
}

func (m *Manager) accessKeyOwner(ctx context.Context, accessKey string) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	k := m.snapshot().keys[accessKey]
	if k == nil {
		return "", ErrKeyNotFound
	}
	return k.User, nil
}

// deleteAccessKey deletes a key; owner binds it like updateAccessKey.
func (m *Manager) deleteAccessKey(ctx context.Context, owner, accessKey string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if m.opts.RootAccessKey != "" && accessKey == m.opts.RootAccessKey {
		return ErrRootImmutable
	}
	return m.mutate(func(st *state) error {
		k := st.keys[accessKey]
		if k == nil || (owner != "" && k.User != owner) {
			return ErrKeyNotFound
		}
		delete(st.keys, accessKey)
		if old := st.users[k.User]; old != nil {
			u := *old
			u.Keys = slices.DeleteFunc(slices.Clone(old.Keys), func(s string) bool { return s == accessKey })
			u.UpdatedAt = m.now()
			st.users[k.User] = &u
		}
		return nil
	})
}

// UpdateUserAccessKey is UpdateAccessKey for a key that must belong to
// owner: the ownership check and the change are one atomic step, so a
// caller authorized against owner cannot change a key that was meanwhile
// re-created under the same ID for another user (ErrKeyNotFound).
func (m *Manager) UpdateUserAccessKey(ctx context.Context, owner, accessKey, status string) error {
	if owner == "" {
		return ErrKeyNotFound
	}
	return m.updateAccessKey(ctx, owner, accessKey, status)
}

// DeleteUserAccessKey is DeleteAccessKey for a key that must belong to
// owner (checked atomically; see UpdateUserAccessKey).
func (m *Manager) DeleteUserAccessKey(ctx context.Context, owner, accessKey string) error {
	if owner == "" {
		return ErrKeyNotFound
	}
	return m.deleteAccessKey(ctx, owner, accessKey)
}

// warnOnce logs msg at Warn level the first time it is called with id.
func (m *Manager) warnOnce(id, msg string, args ...any) {
	if _, seen := m.warned.LoadOrStore(id, true); !seen {
		slog.Warn(msg, args...)
	}
}

func (m *Manager) listPolicies(ctx context.Context) ([]NamedPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	st := m.snapshot()
	out := make([]NamedPolicy, 0, len(builtinOrder)+len(st.policies))
	for _, name := range builtinOrder {
		out = append(out, copyPolicy(builtinPolicies[name]))
	}
	for _, name := range slices.Sorted(maps.Keys(st.policies)) {
		out = append(out, copyPolicy(st.policies[name]))
	}
	return out, nil
}

func (m *Manager) getPolicy(ctx context.Context, name string) (NamedPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return NamedPolicy{}, err
	}
	p := m.snapshot().policy(name)
	if p == nil {
		return NamedPolicy{}, ErrPolicyNotFound
	}
	return copyPolicy(p), nil
}

func (m *Manager) putPolicy(ctx context.Context, name, description string, doc PolicyDocument) (NamedPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return NamedPolicy{}, err
	}
	if _, ok := builtinPolicies[name]; ok {
		return NamedPolicy{}, ErrPolicyBuiltin
	}
	if !validName(name, maxPolicyNameLen) {
		return NamedPolicy{}, wrapf(ErrInvalidName, "policy names must be 1-128 characters of [A-Za-z0-9+=,.@_-]")
	}
	if isDotSegment(name) {
		return NamedPolicy{}, wrapf(ErrInvalidName, fmt.Sprintf("policy name %q is not allowed", name))
	}
	if err := checkDescription(description); err != nil {
		return NamedPolicy{}, err
	}
	d := cloneDocument(doc)
	if reason := validateNewIdentityPolicy(&d); reason != "" {
		return NamedPolicy{}, wrapf(ErrInvalidPolicy, reason)
	}
	if b, err := json.Marshal(d); err != nil || len(b) > MaxPolicySize {
		return NamedPolicy{}, wrapf(ErrInvalidPolicy, fmt.Sprintf("Policy exceeds the maximum allowed document size of %d bytes.", MaxPolicySize))
	}
	var out NamedPolicy
	err := m.mutate(func(st *state) error {
		now := m.now()
		p := &NamedPolicy{Name: name, Description: description, Document: d, CreatedAt: now, UpdatedAt: now}
		if old := st.policies[name]; old != nil {
			p.CreatedAt = old.CreatedAt
		} else if len(st.policies) >= maxCustomPolicies {
			return wrapf(ErrLimitExceeded, fmt.Sprintf("at most %d custom policies", maxCustomPolicies))
		}
		st.policies[name] = p
		out = copyPolicy(p)
		return nil
	})
	if err != nil {
		return NamedPolicy{}, err
	}
	return out, nil
}

func (m *Manager) deletePolicy(ctx context.Context, name string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if _, ok := builtinPolicies[name]; ok {
		return ErrPolicyBuiltin
	}
	return m.mutate(func(st *state) error {
		if st.policies[name] == nil {
			return ErrPolicyNotFound
		}
		var users []string
		for _, u := range st.users {
			if slices.Contains(u.Policies, name) {
				users = append(users, u.Name)
			}
		}
		if len(users) > 0 {
			slices.Sort(users)
			return wrapf(ErrPolicyInUse, "attached to "+strings.Join(users, ", "))
		}
		delete(st.policies, name)
		return nil
	})
}

func (m *Manager) effectivePolicies(ctx context.Context, id auth.Identity) ([]NamedPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	switch {
	case id.IsAnonymous:
		return []NamedPolicy{}, nil
	case id.IsRoot:
		return []NamedPolicy{copyPolicy(builtinPolicies[PolicyAdmin])}, nil
	}
	st := m.snapshot()
	u := st.resolveUser(id)
	if u == nil {
		return nil, ErrUserNotFound
	}
	out := make([]NamedPolicy, 0, len(u.Policies))
	for _, name := range u.Policies {
		if p := st.policy(name); p != nil {
			out = append(out, copyPolicy(p))
		}
	}
	return out, nil
}
