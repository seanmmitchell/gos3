package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"

	"go.etcd.io/bbolt"
)

// ErrMetadataCondition means a committed transaction found that one of its
// expected records had changed. The transaction is a durable no-op.
var ErrMetadataCondition = errors.New("cluster: metadata condition failed")

var metadataOutcomes = []byte("outcomes")

// Allow an exact-byte CAS of two maximum-size (1 MiB) records plus their
// base64 encoding and envelope. Individual records retain their 1 MiB limit.
const maxMetadataTransactionBytes = 3 << 20

// MetadataCondition compares the exact current bytes of a shared record.
// Exists=false requires absence; Exists=true also compares Value, including
// an empty value. The checks and writes run in one bbolt transaction.
type MetadataCondition struct {
	Namespace MetadataNamespace `json:"namespace"`
	Key       string            `json:"key"`
	Exists    bool              `json:"exists"`
	Value     []byte            `json:"value,omitempty"`
}

// MetadataPrefix selects shared records by byte prefix. It is used for
// atomic empty-bucket checks and removal of all bucket configurations.
type MetadataPrefix struct {
	Namespace MetadataNamespace `json:"namespace"`
	Prefix    string            `json:"prefix"`
}

// MetadataPrefixDigest compares the complete ordered key/value set under a
// prefix without putting thousands of multipart part records in a proposal.
type MetadataPrefixDigest struct {
	Namespace MetadataNamespace `json:"namespace"`
	Prefix    string            `json:"prefix"`
	SHA256    string            `json:"sha256"`
}

func digestPrefixRecord(h hash.Hash, key, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(key)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(key)
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

// MetadataTransaction applies related changes atomically after all checks.
// OperationID is the hexadecimal ID also passed to Raft ProposeWithID, so
// retry outcomes survive an uncertain response or a process restart.
type MetadataTransaction struct {
	OperationID    string                 `json:"operationId"`
	PayloadSHA256  string                 `json:"payloadSha256,omitempty"`
	Conditions     []MetadataCondition    `json:"conditions,omitempty"`
	AbsentPrefixes []MetadataPrefix       `json:"absentPrefixes,omitempty"`
	PrefixDigests  []MetadataPrefixDigest `json:"prefixDigests,omitempty"`
	DeletePrefixes []MetadataPrefix       `json:"deletePrefixes,omitempty"`
	Changes        []MetadataCommand      `json:"changes"`
}

func validMetadataPrefix(prefix MetadataPrefix) bool {
	return validMetadataNamespace(prefix.Namespace) && prefix.Prefix != "" && len(prefix.Prefix) <= 4096
}

func validOptionalPayloadHash(value string) bool {
	if value == "" {
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func (t MetadataTransaction) validate() error {
	id, err := hex.DecodeString(t.OperationID)
	if err != nil || len(id) != 16 || !validOptionalPayloadHash(t.PayloadSHA256) || len(t.Changes) == 0 || len(t.Changes) > 128 || len(t.Conditions) > 128 || len(t.AbsentPrefixes) > 16 || len(t.DeletePrefixes) > 16 || len(t.PrefixDigests) > 16 {
		return errInvalidMetadataCommand
	}
	seen := make(map[string]bool, len(t.Changes))
	for _, change := range t.Changes {
		if err := change.validate(); err != nil || change.PayloadSHA256 != "" {
			return errInvalidMetadataCommand
		}
		key := string(change.Namespace) + "\x00" + change.Key
		if seen[key] {
			return errInvalidMetadataCommand
		}
		seen[key] = true
	}
	seen = make(map[string]bool, len(t.Conditions))
	for _, condition := range t.Conditions {
		if err := (MetadataCommand{Namespace: condition.Namespace, Key: condition.Key, Value: condition.Value}).validate(); err != nil || (!condition.Exists && len(condition.Value) != 0) {
			return errInvalidMetadataCommand
		}
		key := string(condition.Namespace) + "\x00" + condition.Key
		if seen[key] {
			return errInvalidMetadataCommand
		}
		seen[key] = true
	}
	for _, prefix := range append(append([]MetadataPrefix{}, t.AbsentPrefixes...), t.DeletePrefixes...) {
		if !validMetadataPrefix(prefix) {
			return errInvalidMetadataCommand
		}
	}
	for _, check := range t.PrefixDigests {
		if !validMetadataPrefix(MetadataPrefix{Namespace: check.Namespace, Prefix: check.Prefix}) || !validOptionalPayloadHash(check.SHA256) || check.SHA256 == "" {
			return errInvalidMetadataCommand
		}
	}
	return nil
}

func EncodeMetadataTransaction(t MetadataTransaction) ([]byte, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(struct {
		Transaction MetadataTransaction `json:"transaction"`
	}{Transaction: t})
	if len(data) > maxMetadataTransactionBytes {
		return nil, errInvalidMetadataCommand
	}
	return data, err
}

func decodeMetadataTransaction(data []byte) (MetadataTransaction, bool, error) {
	if len(data) > maxMetadataTransactionBytes {
		return MetadataTransaction{}, false, errInvalidMetadataCommand
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return MetadataTransaction{}, false, fmt.Errorf("%w: %v", errInvalidMetadataCommand, err)
	}
	if _, ok := fields["transaction"]; !ok {
		return MetadataTransaction{}, false, nil
	}
	var envelope struct {
		Transaction MetadataTransaction `json:"transaction"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&envelope); err != nil {
		return MetadataTransaction{}, true, fmt.Errorf("%w: %v", errInvalidMetadataCommand, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return MetadataTransaction{}, true, errInvalidMetadataCommand
	}
	return envelope.Transaction, true, envelope.Transaction.validate()
}

// applyTransaction records the exact outcome and Raft index in one fsynced
// bbolt commit. A retry with the same ID and bytes reads the prior outcome;
// reusing an ID with different bytes fails closed.
func (s *MetadataStore) applyTransaction(index uint64, data []byte, command MetadataTransaction) error {
	hash := sha256.Sum256(data)
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metadataBucket)
		applied := metadataIndex(meta)
		if index < applied {
			return nil
		}
		if index == applied {
			if bytes.Equal(meta.Get(metadataHash), hash[:]) {
				return nil
			}
			return errors.New("cluster: conflicting command at applied metadata index")
		}
		outcomes := tx.Bucket(metadataOutcomes)
		if prior := outcomes.Get([]byte(command.OperationID)); prior != nil {
			if len(prior) != 33 || !bytes.Equal(prior[:32], hash[:]) {
				return errors.New("cluster: operation ID reused with different metadata transaction")
			}
		} else {
			matched := true
			for _, check := range command.Conditions {
				raw := tx.Bucket(stateBucket(check.Namespace)).Get([]byte(check.Key))
				if (raw != nil) != check.Exists || (check.Exists && !bytes.Equal(raw, check.Value)) {
					matched = false
					break
				}
			}
			if matched {
				for _, prefix := range command.AbsentPrefixes {
					cursor := tx.Bucket(stateBucket(prefix.Namespace)).Cursor()
					if key, _ := cursor.Seek([]byte(prefix.Prefix)); bytes.HasPrefix(key, []byte(prefix.Prefix)) {
						matched = false
						break
					}
				}
			}
			if matched {
				for _, check := range command.PrefixDigests {
					h := sha256.New()
					cursor := tx.Bucket(stateBucket(check.Namespace)).Cursor()
					for key, value := cursor.Seek([]byte(check.Prefix)); bytes.HasPrefix(key, []byte(check.Prefix)); key, value = cursor.Next() {
						digestPrefixRecord(h, key, value)
					}
					if hex.EncodeToString(h.Sum(nil)) != check.SHA256 {
						matched = false
						break
					}
				}
			}
			if matched {
				for _, change := range command.Changes {
					bucket := tx.Bucket(stateBucket(change.Namespace))
					var err error
					if change.Delete {
						err = bucket.Delete([]byte(change.Key))
					} else {
						err = bucket.Put([]byte(change.Key), change.Value)
					}
					if err != nil {
						return err
					}
				}
				for _, prefix := range command.DeletePrefixes {
					cursor := tx.Bucket(stateBucket(prefix.Namespace)).Cursor()
					for key, _ := cursor.Seek([]byte(prefix.Prefix)); bytes.HasPrefix(key, []byte(prefix.Prefix)); key, _ = cursor.Seek([]byte(prefix.Prefix)) {
						if err := cursor.Delete(); err != nil {
							return err
						}
					}
				}
			}
			outcome := make([]byte, 33)
			copy(outcome, hash[:])
			if matched {
				outcome[32] = 1
			}
			if err := outcomes.Put([]byte(command.OperationID), outcome); err != nil {
				return err
			}
		}
		if err := putMetadataIndex(meta, index); err != nil {
			return err
		}
		return meta.Put(metadataHash, hash[:])
	})
}

// Outcome reports whether the transaction with operationID committed its
// changes. found=false means it has not yet been applied here.
func (s *MetadataStore) Outcome(operationID string) (applied, found bool, err error) {
	id, decodeErr := hex.DecodeString(operationID)
	if decodeErr != nil || len(id) != 16 {
		return false, false, errInvalidMetadataCommand
	}
	err = s.db.View(func(tx *bbolt.Tx) error {
		outcome := tx.Bucket(metadataOutcomes).Get([]byte(operationID))
		if outcome != nil {
			if len(outcome) != 33 || (outcome[32] != 0 && outcome[32] != 1) {
				return errors.New("cluster: corrupt metadata transaction outcome")
			}
			found, applied = true, outcome[32] == 1
		}
		return nil
	})
	return applied, found, err
}
