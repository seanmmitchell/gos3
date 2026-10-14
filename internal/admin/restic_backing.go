package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

type resticBackingResponse struct {
	Mode                   string `json:"mode"`
	Endpoint               string `json:"endpoint"`
	Bucket                 string `json:"bucket"`
	Prefix                 string `json:"prefix"`
	Region                 string `json:"region"`
	CredentialsConfigured  bool   `json:"credentialsConfigured"`
	SessionTokenConfigured bool   `json:"sessionTokenConfigured"`
	Locked                 bool   `json:"locked"`
}

func describeResticBacking(s *storage.ResticS3, locked bool) resticBackingResponse {
	doc := resticBackingResponse{Mode: "local", Locked: locked}
	if s != nil {
		doc.Mode = "s3"
		doc.Endpoint = s.Endpoint
		doc.Bucket = s.Bucket
		doc.Prefix = s.Prefix
		doc.Region = s.Region
		doc.CredentialsConfigured = s.AccessKey != "" && s.SecretKey != ""
		doc.SessionTokenConfigured = s.SessionToken != ""
	}
	return doc
}

func (a *api) getBackingRestic(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	b, err := r.ResticBacking(rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, notFound("restic storage is available only on filesystem backings")
	}
	s, locked, err := b.ResticBackingSettings(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, describeResticBacking(s, locked), nil
}

// putBackingRestic sets the restic storage of a filesystem backing: local
// repositories, or S3 storage whose blank credentials keep the saved ones.
// The settings are validated (syntax, not this server itself, repository
// access) before they replace the old ones. Changes are serialized per
// backing.
func (a *api) putBackingRestic(rc *reqCtx) (int, any, error) {
	name := rc.r.PathValue("name")
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	b, err := r.ResticBacking(name)
	if err != nil {
		return 0, nil, notFound("restic storage is available only on filesystem backings")
	}
	var in struct {
		Mode string `json:"mode"`
		storage.ResticS3
		ClearSessionToken bool `json:"clearSessionToken"`
	}
	if err := rc.decodeBody(&in, false); err != nil {
		return 0, nil, err
	}
	defer a.configLocks.lock("backing/" + name)()
	old, _, err := b.ResticBackingSettings(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	var cfg *storage.ResticS3
	switch in.Mode {
	case "local":
		if in.Endpoint != "" || in.Bucket != "" || in.Prefix != "" || in.AccessKey != "" || in.SecretKey != "" || in.SessionToken != "" {
			return 0, nil, badRequest("local restic storage does not take S3 settings")
		}
	case "s3":
		cfg = &in.ResticS3
		if old != nil {
			if cfg.AccessKey == "" {
				cfg.AccessKey = old.AccessKey
			}
			if cfg.SecretKey == "" {
				cfg.SecretKey = old.SecretKey
			}
			if cfg.SessionToken == "" {
				cfg.SessionToken = old.SessionToken
			}
		}
		if in.ClearSessionToken {
			cfg.SessionToken = ""
		}
	default:
		return 0, nil, badRequest("mode must be local or s3")
	}
	if err := cfg.Validate(); err != nil {
		return 0, nil, badRequest("%s", err.Error())
	}
	ctx, cancel := context.WithTimeout(rc.ctx, time.Minute)
	defer cancel()
	if cfg != nil {
		if err := a.checkNotSelf(ctx, cfg.Endpoint); err != nil {
			return 0, nil, err
		}
	}
	if err := b.ConfigureResticBacking(ctx, cfg); err != nil {
		a.log.Error("admin: restic backing configuration failed", "backing", name, "err", err)
		if errors.Is(err, ctx.Err()) {
			return 0, nil, badRequest("restic backing validation timed out")
		}
		return 0, nil, badRequest("Cannot configure restic backing storage. Check repository access and server logs. Locations cannot change after initialization; move buckets to another backing instead.")
	}
	s, locked, err := b.ResticBackingSettings(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, describeResticBacking(s, locked), nil
}
