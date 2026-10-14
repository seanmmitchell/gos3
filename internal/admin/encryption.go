package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// encryptionDocument is the response of GET/PUT
// /buckets/{name}/encryption-provider: the bucket's SSE-S3 implementation,
// the providers it can use right now (omitted when the backend cannot
// tell) and the backings its restic repositories live on.
type encryptionDocument struct {
	Provider  string   `json:"provider"`
	Available []string `json:"available,omitempty"`
	Backings  []string `json:"backings,omitempty"`
}

// describeBucketEncryption builds the encryptionDocument of bucket with
// provider. Availability and placement are informational: failing to read
// them is logged, not fatal.
func (a *api) describeBucketEncryption(ctx context.Context, bucket, provider string) encryptionDocument {
	doc := encryptionDocument{Provider: provider}
	if p, err := a.encryptionProviders(ctx, bucket); err != nil {
		a.log.Warn("admin: cannot list encryption providers", "bucket", bucket, "err", err)
	} else {
		doc.Available = p
	}
	if r, err := a.router(); err == nil {
		if p, err := r.Summary(bucket); err == nil {
			doc.Backings = append([]string{p.Primary}, p.Replicas...)
		}
	}
	return doc
}

// encryptionProviders returns the providers bucket can use, or nil when
// the backend does not implement storage.EncryptionProvidersBackend.
func (a *api) encryptionProviders(ctx context.Context, bucket string) ([]string, error) {
	pb, ok := a.d.Backend.(storage.EncryptionProvidersBackend)
	if !ok {
		return nil, nil
	}
	p, err := pb.EncryptionProviders(ctx, bucket)
	if p == nil && err == nil {
		p = []string{}
	}
	return p, err
}

// getBucketEncryption returns the bucket's provider. It reads the document
// with storage.EncryptionProvider, the parser writes use, so an invalid
// stored provider is an error here too instead of being shown as valid.
func (a *api) getBucketEncryption(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	provider, err := storage.EncryptionProvider(rc.ctx, a.d.Backend, bucket)
	if err != nil {
		if errors.Is(err, storage.ErrBucketNotFound) {
			return 0, nil, err
		}
		return 0, nil, fmt.Errorf("reading the encryption provider: %w (PUT a provider to replace it)", err)
	}
	return http.StatusOK, a.describeBucketEncryption(rc.ctx, bucket, provider), nil
}

// putBucketEncryption selects the implementation of SSE-S3, independently
// of whether bucket default SSE is enabled. Storage locations and secrets
// are not bucket state: each backing holding a copy of the bucket keeps its
// keys and restic repository itself.
//
// The new provider is stored only once it is ready: the backend validates
// it and prepares its keys or repository first, so a failure leaves the
// previous provider in effect instead of switching the bucket's writes to
// a provider that cannot store them. Changes are serialized per bucket.
func (a *api) putBucketEncryption(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	var in storage.EncryptionSettings
	if err := rc.decodeBody(&in, false); err != nil {
		return 0, nil, err
	}
	if !storage.ValidEncryptionProvider(in.Provider) {
		return 0, nil, badRequest("provider must be %s or %s", storage.EncryptionAES256, storage.EncryptionRestic)
	}
	e, ok := a.d.Backend.(storage.EncryptionBackend)
	if !ok {
		return 0, nil, badRequest("this storage backend does not support encryption")
	}
	defer a.configLocks.lock("bucket/" + bucket)()
	available, err := a.encryptionProviders(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	if available != nil && !slices.Contains(available, in.Provider) {
		return 0, nil, badRequest("%s", unavailableProviderMessage(in.Provider))
	}
	if err := e.ValidateEncryptionSettings(rc.ctx, bucket, in); err != nil {
		return 0, nil, a.encryptionSetupError(bucket, in.Provider, "validation", err)
	}
	if err := e.PrepareEncryption(rc.ctx, bucket, in.Provider); err != nil {
		return 0, nil, a.encryptionSetupError(bucket, in.Provider, "preparation", err)
	}
	data, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	if err := a.d.Backend.PutBucketConfig(rc.ctx, bucket, storage.ConfigEncryptionProvider, data); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: bucket encryption provider set", "bucket", bucket, "provider", in.Provider, "by", rc.id.User)
	return http.StatusOK, a.describeBucketEncryption(rc.ctx, bucket, in.Provider), nil
}

// unavailableProviderMessage explains why provider is not in a bucket's
// available list (only restic can be missing).
func unavailableProviderMessage(provider string) string {
	if provider == storage.EncryptionRestic {
		return "restic is not available for this bucket: the restic executable must be installed on the server, " +
			"and every backing holding the bucket (primary, replicas, move target) must be a filesystem or S3 backing. " +
			"The provider was not changed."
	}
	return fmt.Sprintf("the %s provider is not available for this bucket; the provider was not changed", provider)
}

// encryptionSetupError maps a failed validation or preparation (stage) of
// provider for bucket to the API error. A missing bucket and canceled
// requests keep their own errors; the rest are logged with their cause,
// which can mention repository locations.
func (a *api) encryptionSetupError(bucket, provider, stage string, err error) error {
	switch {
	case errors.Is(err, storage.ErrBucketNotFound), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, errors.ErrUnsupported):
		return badRequest("the storage backing of this bucket does not support the %s provider; the provider was not changed", provider)
	}
	a.log.Error("admin: encryption provider "+stage+" failed", "bucket", bucket, "provider", provider, "err", err)
	if provider == storage.EncryptionRestic {
		return badRequest("Cannot prepare the restic repository of this bucket; the provider was not changed. " +
			"Check the server log, the restic installation and the storage of the bucket's backings.")
	}
	return badRequest("Cannot prepare encryption keys for this bucket; the provider was not changed. Check the server log.")
}

// keyedMutex serializes work per key (one bucket's encryption provider)
// without blocking other keys: preparing a restic repository can take
// long. The zero value is ready to use.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

// keyedEntry is the mutex of one key and the number of its holders and
// waiters; it is dropped when the last one unlocks.
type keyedEntry struct {
	mu sync.Mutex
	n  int
}

// lock locks key and returns the unlock function.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.n++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.n--; e.n == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
