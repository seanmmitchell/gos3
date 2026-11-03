package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

const sharedIAMKey = "state"

// IAMStateStore places the entire IAM document, including access-key
// secrets, in the authenticated and durable consensus namespace.
type IAMStateStore struct{ Node *Node }

var _ iam.StateStore = IAMStateStore{}

func (s IAMStateStore) Load(ctx context.Context) ([]byte, bool, error) {
	if s.Node == nil {
		return nil, false, errors.New("cluster: IAM state has no node")
	}
	data, found, err := s.Node.GetMetadata(ctx, MetadataIAM, sharedIAMKey)
	return data, found, clusterSharedStateError(err)
}

func (s IAMStateStore) CompareAndSwap(ctx context.Context, prior []byte, found bool, next []byte) error {
	if s.Node == nil || len(next) == 0 {
		return errors.New("cluster: invalid IAM state write")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	tx := MetadataTransaction{
		OperationID: hex.EncodeToString(id[:]),
		Conditions:  []MetadataCondition{{Namespace: MetadataIAM, Key: sharedIAMKey, Exists: found, Value: prior}},
		Changes:     []MetadataCommand{{Namespace: MetadataIAM, Key: sharedIAMKey, Value: next}},
	}
	err := s.Node.AdmitTransactionWrite(ctx, id, "system", "iam", next, tx)
	if errors.Is(err, ErrMetadataCondition) {
		return iam.ErrStateConflict
	}
	return clusterSharedStateError(err)
}

func clusterSharedStateError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", s3err.ErrServiceUnavailable, clusterBackendError(err))
}
