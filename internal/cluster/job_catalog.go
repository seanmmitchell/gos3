package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const backgroundJobPrefix = "background\x00"

// ErrJobRecordConflict means the shared job record changed since the caller
// read it. The rejected mutation is a committed no-op.
var ErrJobRecordConflict = errors.New("cluster: background job record changed")

// JobRecord is a quorum-confirmed view of one persistent background job.
// Data is opaque to Raft; the jobs package owns its JSON schema.
type JobRecord struct {
	ID   string
	Data []byte
}

// JobMutation carries one stable operation ID across retries. A caller must
// use the exact same Expected and Next bytes when retrying an uncertain write.
type JobMutation struct {
	OperationID [16]byte
	ID          string
	Expected    []byte
	Exists      bool
	Next        []byte // nil deletes the record
}

func NewJobMutation(id string, expected []byte, exists bool, next []byte) (JobMutation, error) {
	m := JobMutation{ID: id, Expected: bytes.Clone(expected), Exists: exists, Next: bytes.Clone(next)}
	_, err := rand.Read(m.OperationID[:])
	return m, err
}

// JobCatalog stores background job records in the ordered metadata state
// machine. Multipart records use separate prefixes in the same namespace.
type JobCatalog struct{ node *Node }

func NewJobCatalog(node *Node) *JobCatalog { return &JobCatalog{node: node} }

func jobRecordKey(id string) string { return backgroundJobPrefix + id }

func validJobRecordID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i, c := range id {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '-' || c == '_') {
			continue
		}
		return false
	}
	return true
}

func (c *JobCatalog) Get(ctx context.Context, id string) ([]byte, bool, error) {
	if c == nil || c.node == nil || !validJobRecordID(id) {
		return nil, false, errors.New("cluster: invalid background job lookup")
	}
	return c.node.GetMetadata(ctx, MetadataJobs, jobRecordKey(id))
}

func (c *JobCatalog) List(ctx context.Context) ([]JobRecord, error) {
	if c == nil || c.node == nil {
		return nil, errors.New("cluster: background job catalog unavailable")
	}
	records, err := c.node.ListMetadata(ctx, MetadataJobs, backgroundJobPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]JobRecord, 0, len(records))
	for _, record := range records {
		id := strings.TrimPrefix(record.Key, backgroundJobPrefix)
		if !validJobRecordID(id) {
			return nil, fmt.Errorf("cluster: corrupt background job ID %q", id)
		}
		out = append(out, JobRecord{ID: id, Data: bytes.Clone(record.Value)})
	}
	return out, nil
}

func (c *JobCatalog) CompareAndSwap(ctx context.Context, mutation JobMutation) error {
	if c == nil || c.node == nil || !validJobRecordID(mutation.ID) || mutation.OperationID == [16]byte{} ||
		(!mutation.Exists && len(mutation.Expected) != 0) || (mutation.Exists && !json.Valid(mutation.Expected)) ||
		(mutation.Next != nil && !json.Valid(mutation.Next)) || len(mutation.Next) > 1<<20 ||
		(mutation.Next == nil && !mutation.Exists) {
		return errors.New("cluster: invalid background job mutation")
	}
	key := jobRecordKey(mutation.ID)
	change := MetadataCommand{Namespace: MetadataJobs, Key: key, Value: mutation.Next, Delete: mutation.Next == nil}
	transaction := MetadataTransaction{
		OperationID: hex.EncodeToString(mutation.OperationID[:]),
		Conditions:  []MetadataCondition{{Namespace: MetadataJobs, Key: key, Exists: mutation.Exists, Value: mutation.Expected}},
		Changes:     []MetadataCommand{change},
	}
	payload := mutation.Next
	if payload == nil {
		payload = mutation.Expected
	}
	err := c.node.AdmitTransactionWrite(ctx, mutation.OperationID, "system", key, payload, transaction)
	if errors.Is(err, ErrMetadataCondition) {
		return ErrJobRecordConflict
	}
	return err
}
