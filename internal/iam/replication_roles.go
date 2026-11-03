package iam

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	ErrReplicationRoleNotFound = errors.New("iam: replication role not found")
	ErrInvalidReplicationRole  = errors.New("iam: invalid replication role")
)

const (
	ReplicateObjectAction     = "s3:ReplicateObject"
	ReplicateDeleteAction     = "s3:ReplicateDelete"
	ReplicateTagsAction       = "s3:ReplicateTags"
	ReplicateObjectLockAction = "s3:ReplicateObjectLock"
)

var (
	replicationARNPattern    = regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_-]{1,64}$`)
	replicationBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// ReplicationRole is an internal IAM role with narrowly scoped grants. It
// has no access keys and cannot authenticate as a user or call the admin API.
type ReplicationRole struct {
	ARN    string             `json:"arn"`
	Grants []ReplicationGrant `json:"grants"`
}

// ReplicationGrant allows named replication actions only from one source
// bucket into one destination bucket and key prefix.
type ReplicationGrant struct {
	SourceBucket      string   `json:"sourceBucket"`
	DestinationBucket string   `json:"destinationBucket"`
	Prefix            string   `json:"prefix,omitempty"`
	Actions           []string `json:"actions"`
}

func validateReplicationRole(role ReplicationRole) error {
	if !replicationARNPattern.MatchString(role.ARN) || len(role.Grants) == 0 || len(role.Grants) > 32 {
		return ErrInvalidReplicationRole
	}
	for _, grant := range role.Grants {
		if !replicationBucketPattern.MatchString(grant.SourceBucket) || !replicationBucketPattern.MatchString(grant.DestinationBucket) || grant.SourceBucket == grant.DestinationBucket || len(grant.Prefix) > 1024 || !utf8.ValidString(grant.Prefix) || len(grant.Actions) == 0 || len(grant.Actions) > 4 {
			return fmt.Errorf("%w: invalid source, destination, prefix or action count", ErrInvalidReplicationRole)
		}
		seen := make(map[string]bool, len(grant.Actions))
		for _, action := range grant.Actions {
			switch action {
			case ReplicateObjectAction, ReplicateDeleteAction, ReplicateTagsAction, ReplicateObjectLockAction:
			default:
				return fmt.Errorf("%w: unsupported action %q", ErrInvalidReplicationRole, action)
			}
			if seen[action] {
				return fmt.Errorf("%w: duplicate action %q", ErrInvalidReplicationRole, action)
			}
			seen[action] = true
		}
	}
	return nil
}

func cloneReplicationRole(role ReplicationRole) *ReplicationRole {
	copy := role
	copy.Grants = slices.Clone(role.Grants)
	for i := range copy.Grants {
		copy.Grants[i].Actions = slices.Clone(copy.Grants[i].Actions)
	}
	return &copy
}

// PutReplicationRole creates or replaces a limited role, persisting it in
// iam.json before publishing it to concurrent authorization checks.
func (m *Manager) PutReplicationRole(ctx context.Context, role ReplicationRole) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateReplicationRole(role); err != nil {
		return err
	}
	copy := cloneReplicationRole(role)
	return m.mutate(ctx, func(st *state) error {
		st.roles[role.ARN] = copy
		return nil
	})
}

func (m *Manager) GetReplicationRole(ctx context.Context, arn string) (ReplicationRole, error) {
	if err := ctx.Err(); err != nil {
		return ReplicationRole{}, err
	}
	st, err := m.readSnapshot(ctx)
	if err != nil {
		return ReplicationRole{}, err
	}
	role := st.roles[arn]
	if role == nil {
		return ReplicationRole{}, ErrReplicationRoleNotFound
	}
	return *cloneReplicationRole(*role), nil
}

func (m *Manager) ListReplicationRoles(ctx context.Context) ([]ReplicationRole, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := m.readSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	roles := st.roles
	result := make([]ReplicationRole, 0, len(roles))
	for _, arn := range slices.Sorted(maps.Keys(roles)) {
		result = append(result, *cloneReplicationRole(*roles[arn]))
	}
	return result, nil
}

func (m *Manager) DeleteReplicationRole(ctx context.Context, arn string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.mutate(ctx, func(st *state) error {
		if st.roles[arn] == nil {
			return ErrReplicationRoleNotFound
		}
		delete(st.roles, arn)
		return nil
	})
}

// CanReplicate checks every action needed for an individual destination
// write. An absent role or any missing action denies the copy.
func (m *Manager) CanReplicate(ctx context.Context, arn, source, destination, key string, actions ...string) bool {
	if ctx.Err() != nil || len(actions) == 0 {
		return false
	}
	st, err := m.readSnapshot(ctx)
	if err != nil {
		return false
	}
	role := st.roles[arn]
	if role == nil {
		return false
	}
	for _, grant := range role.Grants {
		if grant.SourceBucket != source || grant.DestinationBucket != destination || !strings.HasPrefix(key, grant.Prefix) {
			continue
		}
		allowed := true
		for _, action := range actions {
			if !slices.Contains(grant.Actions, action) {
				allowed = false
				break
			}
		}
		if allowed {
			return true
		}
	}
	return false
}
