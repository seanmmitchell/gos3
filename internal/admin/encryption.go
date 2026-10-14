package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

type encryptionDocument struct {
	Provider              string `json:"provider"`
	Repository            string `json:"repository"`
	Region                string `json:"region"`
	CredentialsConfigured bool   `json:"credentialsConfigured"`
	PasswordConfigured    bool   `json:"passwordConfigured"`
	Ready                 *bool  `json:"ready,omitempty"`
	Error                 string `json:"error,omitempty"`
}

func describeEncryption(s storage.EncryptionSettings) encryptionDocument {
	return encryptionDocument{Provider: s.Provider, Repository: s.Repository, Region: s.Region,
		CredentialsConfigured: s.AccessKey != "" && s.SecretKey != "", PasswordConfigured: s.Password != ""}
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
	return http.StatusOK, describeEncryption(s), nil
}

// PUT selects the implementation of SSE-S3, independently of whether bucket
// default SSE is enabled. Secrets never leave the protected configuration store.
// Empty secret fields preserve the previous value, including after a failed init.
func (a *api) putBucketEncryption(rc *reqCtx) (int, any, error) {
	// Serialize read/validate/store so concurrent setup cannot replace a
	// repository after another request has started using it.
	a.encryptionMu.Lock()
	defer a.encryptionMu.Unlock()
	bucket := rc.r.PathValue("name")
	old, err := a.encryptionSettings(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
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
	if len(s.Repository) > 2048 || len(s.AccessKey) > 1024 || len(s.SecretKey) > 4096 || len(s.SessionToken) > 16384 || len(s.Password) > 4096 || len(s.Region) > 128 {
		return 0, nil, badRequest("encryption setting exceeds its length limit")
	}
	if s.Provider == storage.EncryptionAES256 {
		// Preserve repository settings: existing snapshots still need them.
		s = old
		s.Provider = storage.EncryptionAES256
	} else {
		if old.Provider == storage.EncryptionRestic || old.Password != "" {
			if s.Repository != old.Repository {
				return 0, nil, badRequest("the repository location is fixed once restic is configured; use a separate bucket for a new repository")
			}
		}
		if s.Repository != "" {
			u, e := url.Parse(strings.TrimPrefix(s.Repository, "s3:"))
			if e != nil || !strings.HasPrefix(s.Repository, "s3:") || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") == "" {
				return 0, nil, badRequest("repository must be empty for local storage or s3:https://host/bucket/prefix")
			}
		}
		if s.AccessKey == "" {
			s.AccessKey = old.AccessKey
		}
		if s.SecretKey == "" {
			s.SecretKey = old.SecretKey
		}
		if s.SessionToken == "" {
			s.SessionToken = old.SessionToken
		}
		if s.Password == "" {
			s.Password = old.Password
		}
		if s.Password == "" {
			key := make([]byte, 32)
			_, _ = rand.Read(key)
			s.Password = hex.EncodeToString(key)
		}
		if s.Repository != "" && (s.AccessKey == "" || s.SecretKey == "") {
			return 0, nil, badRequest("remote repositories require an access key and secret key")
		}
	}
	e, ok := a.d.Backend.(storage.EncryptionBackend)
	if !ok {
		return 0, nil, badRequest("this backend does not support encryption")
	}
	if err := e.ValidateEncryptionSettings(rc.ctx, bucket, s); err != nil {
		a.log.Error("admin: encryption settings validation failed", "bucket", bucket, "err", err)
		return 0, nil, badRequest("Cannot open the encryption repository. Check the server log, restic installation, repository credentials, and password.")
	}
	data, _ := json.Marshal(s)
	if err := a.d.Backend.PutBucketConfig(rc.ctx, bucket, storage.ConfigEncryptionProvider, data); err != nil {
		return 0, nil, err
	}
	doc := describeEncryption(s)
	ready := true
	doc.Ready = &ready
	if err := e.PrepareEncryption(rc.ctx, bucket, s.Provider); err != nil {
		// Keep the password even if init's result is uncertain. Returning this
		// state explicitly allows retrying without losing access to the repo.
		ready = false
		doc.Error = "Provider settings saved, but repository preparation failed. Check the server log and retry."
		a.log.Error("admin: encryption provider preparation failed", "bucket", bucket, "err", err)
	}
	return http.StatusOK, doc, nil
}
