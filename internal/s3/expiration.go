package s3

import (
	"context"
	"net/http"

	"github.com/seanmmitchell/gos3/internal/lifecycle"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// cacheKindLifecycle is the parsed-config cache kind of lifecycle
// configurations used for the informational lifecycle response headers.
const cacheKindLifecycle = "s3/lifecycle-headers"

// bucketLifecycle returns the bucket's parsed lifecycle configuration, or
// nil when there is none or it cannot be read or parsed: the headers it
// feeds are informational and must never fail a request.
func (s *Server) bucketLifecycle(ctx context.Context, bucket string) *lifecycle.Configuration {
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigLifecycle)
	if err != nil || raw == nil {
		return nil
	}
	v, err := s.configs.get(bucket, cacheKindLifecycle, raw, func(b []byte) (any, error) {
		return lifecycle.Parse(b)
	})
	if err != nil {
		return nil
	}
	cfg, _ := v.(*lifecycle.Configuration)
	return cfg
}

// setExpirationHeader sets x-amz-expiration (expiry-date and rule-id) when
// an enabled lifecycle Expiration rule (Days or Date) of the bucket applies
// to info, as S3 does on PutObject, GetObject and HeadObject. Only current
// versions get the header (noncurrent versions and delete markers expire by
// other rules).
func (s *Server) setExpirationHeader(ctx context.Context, h http.Header, info storage.ObjectInfo) {
	if info.DeleteMarker || !info.IsLatest {
		return
	}
	cfg := s.bucketLifecycle(ctx, info.Bucket)
	if t, id, ok := cfg.CurrentExpiration(info.Key, info.Tags, info.Size, info.LastModified); ok {
		h.Set("X-Amz-Expiration", lifecycle.ExpirationHeader(t, id))
	}
}

// setAbortHeaders sets x-amz-abort-date and x-amz-abort-rule-id when an
// enabled AbortIncompleteMultipartUpload rule applies to the upload, as S3
// does on CreateMultipartUpload and ListParts.
func (s *Server) setAbortHeaders(ctx context.Context, h http.Header, up storage.UploadInfo) {
	cfg := s.bucketLifecycle(ctx, up.Bucket)
	if t, id, ok := cfg.AbortDate(up.Key, up.Initiated); ok {
		h.Set("X-Amz-Abort-Date", t.UTC().Format(http.TimeFormat))
		h.Set("X-Amz-Abort-Rule-Id", id)
	}
}
