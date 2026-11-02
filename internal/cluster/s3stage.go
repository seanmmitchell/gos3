package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// S3StageStore uses a node-specific S3 location for durable payload copies.
// A successful Put response is the backing's durability acknowledgment. A
// failed or ambiguous Put is acknowledged only if a subsequent Get verifies
// the exact staged bytes. Cluster metadata and Raft logs remain local.
type S3StageStore struct {
	client  *s3client.Client
	bucket  string
	prefix  string
	maxUsed int64
	mu      sync.Mutex
	memory  map[string]stagedPayload
	used    int64
	probeMu sync.Mutex
	probed  bool
}

var _ DurableReplicaStore = (*S3StageStore)(nil)

var errCorruptS3Replica = errors.New("cluster: remote durable payload digest mismatch")

// ErrS3ConditionalsRequired means the remote service accepted an overwrite
// despite If-None-Match: *. Such a backing cannot safely deduplicate retries.
var ErrS3ConditionalsRequired = errors.New("cluster: S3 replica backing must honor conditional PUT")

func NewS3StageStore(client *s3client.Client, bucket, prefix string, maxMemoryBytes int64) (*S3StageStore, error) {
	if client == nil || bucket == "" || prefix == "" || !strings.HasSuffix(prefix, "/") || maxMemoryBytes < 1 {
		return nil, errors.New("cluster: S3 replica store needs a client, bucket, key prefix and positive memory limit")
	}
	return &S3StageStore{client: client, bucket: bucket, prefix: prefix, maxUsed: maxMemoryBytes, memory: make(map[string]stagedPayload)}, nil
}

func (s *S3StageStore) MaxMemoryBytes() int64 { return s.maxUsed }

func (s *S3StageStore) key(id string) string {
	return s.prefix + hex.EncodeToString([]byte(id))
}

func (s *S3StageStore) Stage(ctx context.Context, id string, payload []byte, digest [32]byte) error {
	if id == "" || len(id) > 128 {
		return errors.New("cluster: invalid staging operation ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sha256.Sum256(payload) != digest {
		return errors.New("cluster: staged payload digest mismatch")
	}
	s.mu.Lock()
	if prior, ok := s.memory[id]; ok {
		s.mu.Unlock()
		if prior.digest != digest || !bytes.Equal(prior.data, payload) {
			return errors.New("cluster: operation ID reused with different payload")
		}
		return nil
	}
	defer s.mu.Unlock()
	// Memory acceptance remains available during an S3 outage. Persist
	// resolves a prior durable copy or an operation-ID conflict by reading
	// back the exact bytes after a conditional PUT.
	if int64(len(payload)) > s.maxUsed-s.used {
		return ErrStageFull
	}
	s.memory[id] = stagedPayload{digest: digest, data: bytes.Clone(payload)}
	s.used += int64(len(payload))
	return nil
}

func (s *S3StageStore) Persist(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	staged, ok := s.memory[id]
	s.mu.Unlock()
	if !ok {
		if _, err := s.Read(ctx, id); err != nil {
			return fmt.Errorf("cluster: operation was not staged or remote copy is unavailable: %w", err)
		}
		return nil
	}
	if err := s.probeConditionals(ctx); err != nil {
		return err
	}
	header := http.Header{}
	header.Set("X-Amz-Meta-Gos3-Sha256", hex.EncodeToString(staged.digest[:]))
	_, err := s.client.Put(ctx, s.bucket, s.key(id), bytes.NewReader(staged.data), int64(len(staged.data)), s3client.PutOptions{Header: header, IfNoneMatch: true, NoRetry: true})
	if err != nil {
		// A timeout or 412 can mean an earlier attempt committed. Only the
		// complete, verified remote bytes can resolve that uncertainty.
		prior, readErr := s.Read(ctx, id)
		if readErr != nil || !bytes.Equal(prior, staged.data) {
			return fmt.Errorf("cluster: remote durable commit unconfirmed: %w", errors.Join(err, readErr))
		}
	}
	s.mu.Lock()
	if prior, ok := s.memory[id]; ok && prior.digest == staged.digest && bytes.Equal(prior.data, staged.data) {
		delete(s.memory, id)
		s.used -= int64(len(staged.data))
	}
	s.mu.Unlock()
	return nil
}

func (s *S3StageStore) probeConditionals(ctx context.Context) error {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probed {
		return nil
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := s.prefix + "probe-" + hex.EncodeToString(nonce[:])
	payload := []byte("gos3 cluster conditional-write probe\n")
	opts := s3client.PutOptions{IfNoneMatch: true, NoRetry: true}
	if _, err := s.client.Put(ctx, s.bucket, key, bytes.NewReader(payload), int64(len(payload)), opts); err != nil {
		return fmt.Errorf("cluster: probing S3 replica conditional writes: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.client.DeleteObject(cleanup, s.bucket, key)
	}()
	_, err := s.client.Put(ctx, s.bucket, key, bytes.NewReader(payload), int64(len(payload)), opts)
	switch {
	case err == nil:
		return ErrS3ConditionalsRequired
	case s3client.IsPreconditionFailed(err):
		s.probed = true
		return nil
	default:
		return fmt.Errorf("cluster: probing S3 replica conditional writes: %w", err)
	}
}

func (s *S3StageStore) Read(ctx context.Context, id string) ([]byte, error) {
	if id == "" || len(id) > 128 {
		return nil, errors.New("cluster: invalid staging operation ID")
	}
	body, attrs, err := s.client.GetObject(ctx, s.bucket, s.key(id), s3client.GetOptions{})
	if err != nil {
		return nil, err
	}
	defer body.Close()
	if attrs.Size > s.maxUsed {
		return nil, ErrStageFull
	}
	data, err := io.ReadAll(io.LimitReader(body, s.maxUsed+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxUsed {
		return nil, ErrStageFull
	}
	expected := attrs.Header.Get("X-Amz-Meta-Gos3-Sha256")
	digest := sha256.Sum256(data)
	if expected == "" || expected != hex.EncodeToString(digest[:]) {
		return nil, errCorruptS3Replica
	}
	return data, nil
}

func (s *S3StageStore) Repair(ctx context.Context, id string, payload []byte) error {
	if id == "" || len(id) > 128 || int64(len(payload)) > s.maxUsed {
		return errors.New("cluster: invalid remote repair payload")
	}
	if prior, err := s.Read(ctx, id); err == nil {
		if !bytes.Equal(prior, payload) {
			return errors.New("cluster: operation ID reused with different durable payload")
		}
		return nil
	} else if !s3client.IsNotFound(err) && !errors.Is(err, errCorruptS3Replica) {
		return fmt.Errorf("cluster: checking remote repair target: %w", err)
	}
	digest := sha256.Sum256(payload)
	header := http.Header{}
	header.Set("X-Amz-Meta-Gos3-Sha256", hex.EncodeToString(digest[:]))
	if _, err := s.client.Put(ctx, s.bucket, s.key(id), bytes.NewReader(payload), int64(len(payload)), s3client.PutOptions{Header: header, NoRetry: true}); err != nil {
		return fmt.Errorf("cluster: repairing remote replica: %w", err)
	}
	return nil
}

// RepairCommitted may replace a self-consistent but divergent remote object.
// The caller must supply the digest from the committed Raft version record.
func (s *S3StageStore) RepairCommitted(ctx context.Context, id string, payload []byte, expected [32]byte) error {
	if id == "" || len(id) > 128 || int64(len(payload)) > s.maxUsed || sha256.Sum256(payload) != expected {
		return errors.New("cluster: invalid committed remote repair payload")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if prior, err := s.Read(ctx, id); err == nil && bytes.Equal(prior, payload) {
		return nil
	}
	header := http.Header{}
	header.Set("X-Amz-Meta-Gos3-Sha256", hex.EncodeToString(expected[:]))
	_, err := s.client.Put(ctx, s.bucket, s.key(id), bytes.NewReader(payload), int64(len(payload)), s3client.PutOptions{Header: header, NoRetry: true})
	if err != nil {
		prior, readErr := s.Read(ctx, id)
		if readErr != nil || !bytes.Equal(prior, payload) {
			return fmt.Errorf("cluster: committed remote repair unconfirmed: %w", errors.Join(err, readErr))
		}
	}
	s.mu.Lock()
	if staged, ok := s.memory[id]; ok {
		delete(s.memory, id)
		s.used -= int64(len(staged.data))
	}
	s.mu.Unlock()
	return nil
}

func (*S3StageStore) Close() error { return nil }
