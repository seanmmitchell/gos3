package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

type encryptionDocument struct {
	Provider string   `json:"provider"`
	Backings []string `json:"backings,omitempty"`
	Ready    *bool    `json:"ready,omitempty"`
	Error    string   `json:"error,omitempty"`
}

func describeEncryption(s storage.EncryptionSettings) encryptionDocument {
	return encryptionDocument{Provider: s.Provider}
}

func (a *api) describeBucketEncryption(bucket string, s storage.EncryptionSettings) encryptionDocument {
	doc := describeEncryption(s)
	if r, err := a.router(); err == nil {
		if p, err := r.Summary(bucket); err == nil {
			doc.Backings = append([]string{p.Primary}, p.Replicas...)
		}
	}
	return doc
}

func (a *api) encryptionSettings(ctx context.Context, bucket string) (storage.EncryptionSettings, error) {
	s := storage.EncryptionSettings{Provider: storage.EncryptionAES256}
	raw, err := a.d.Backend.GetBucketConfig(ctx, bucket, storage.ConfigEncryptionProvider)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(raw, &s)
	return s, err
}

func (a *api) getBucketEncryption(rc *reqCtx) (int, any, error) {
	s, err := a.encryptionSettings(rc.ctx, rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, a.describeBucketEncryption(rc.r.PathValue("name"), s), nil
}

// PUT selects the implementation of SSE-S3, independently of whether bucket
// default SSE is enabled. Secrets never leave the protected configuration store.
// Storage settings are inherited from the bucket’s placement.
func (a *api) putBucketEncryption(rc *reqCtx) (int, any, error) {
	// Serialize read/validate/store so concurrent setup cannot replace a
	// provider after another request has started using it.
	a.encryptionMu.Lock()
	defer a.encryptionMu.Unlock()
	bucket := rc.r.PathValue("name")
	raw, err := rc.readBody()
	if err != nil {
		return 0, nil, err
	}
	var s storage.EncryptionSettings
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return 0, nil, badRequest("invalid encryption settings")
	}
	if dec.Decode(new(any)) != io.EOF {
		return 0, nil, badRequest("expected a single JSON document")
	}
	if s.Provider != storage.EncryptionAES256 && s.Provider != storage.EncryptionRestic {
		return 0, nil, badRequest("provider must be AES256 or restic")
	}
	e, ok := a.d.Backend.(storage.EncryptionBackend)
	if !ok {
		return 0, nil, badRequest("this backend does not support encryption")
	}
	if err := e.ValidateEncryptionSettings(rc.ctx, bucket, s); err != nil {
		a.log.Error("admin: encryption settings validation failed", "bucket", bucket, "err", err)
		return 0, nil, badRequest("Cannot open the encryption repository. Check the server log, restic installation, and backing storage configuration.")
	}
	data, _ := json.Marshal(s)
	if err := a.d.Backend.PutBucketConfig(rc.ctx, bucket, storage.ConfigEncryptionProvider, data); err != nil {
		return 0, nil, err
	}
	doc := a.describeBucketEncryption(bucket, s)
	ready := true
	doc.Ready = &ready
	if err := e.PrepareEncryption(rc.ctx, bucket, s.Provider); err != nil {
		// The backing retains its recovery key if init's result is uncertain.
		// Report the saved provider so preparation can safely be retried.
		ready = false
		doc.Error = "Provider settings saved, but repository preparation failed. Check the server log and retry."
		a.log.Error("admin: encryption provider preparation failed", "bucket", bucket, "err", err)
	}
	return http.StatusOK, doc, nil
}
