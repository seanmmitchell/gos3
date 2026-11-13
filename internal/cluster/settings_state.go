package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/seanmmitchell/gos3/internal/config"
)

const sharedSettingsKey = "state"

// SettingsStateStore commits mutable server settings through Raft and the
// same configured memory/durability policy as other cluster mutations.
type SettingsStateStore struct{ Node *Node }

var _ config.SettingsStateStore = SettingsStateStore{}

func (s SettingsStateStore) Load(ctx context.Context) ([]byte, bool, error) {
	if s.Node == nil {
		return nil, false, errors.New("cluster: settings state has no node")
	}
	data, found, err := s.Node.GetMetadata(ctx, MetadataSettings, sharedSettingsKey)
	return data, found, clusterSharedStateError(err)
}

func (s SettingsStateStore) CompareAndSwap(ctx context.Context, prior []byte, found bool, next []byte) error {
	if s.Node == nil || len(next) == 0 {
		return errors.New("cluster: invalid settings state write")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	tx := MetadataTransaction{
		OperationID: hex.EncodeToString(id[:]),
		Conditions:  []MetadataCondition{{Namespace: MetadataSettings, Key: sharedSettingsKey, Exists: found, Value: prior}},
		Changes:     []MetadataCommand{{Namespace: MetadataSettings, Key: sharedSettingsKey, Value: next}},
	}
	err := s.Node.AdmitTransactionWrite(ctx, id, "system", "settings", next, tx)
	if errors.Is(err, ErrMetadataCondition) {
		return config.ErrSettingsConflict
	}
	return clusterSharedStateError(err)
}
