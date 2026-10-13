package admin

import (
	"net/http"

	"github.com/seanmmitchell/gos3/internal/compression"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// bucketCompression is the BucketCompression document of
// docs/admin-api.md: the bucket's own setting, whether new objects are
// actually compressed under the server's policy, and that policy.
type bucketCompression struct {
	// Enabled is the bucket's setting (false when none is stored).
	Enabled bool `json:"enabled"`
	// Effective tells whether new objects of the bucket are stored
	// compressed (subject to minSize and the skipped media types).
	Effective bool `json:"effective"`
	// ServerMode is the administrator's compression.mode: "optional"
	// (Enabled decides), "enforced" or "disabled".
	ServerMode string `json:"serverMode"`
	Algorithm  string `json:"algorithm"`
	Level      int    `json:"level"`
	MinSize    int64  `json:"minSize"`
}

// bucketCompressionSummary is the "compression" member of a BucketSummary.
type bucketCompressionSummary struct {
	Enabled   bool `json:"enabled"`
	Effective bool `json:"effective"`
}

func describeCompression(p config.CompressionSettings, s compression.BucketSetting) bucketCompression {
	return bucketCompression{
		Enabled:    s.Enabled,
		Effective:  compression.Effective(p, s),
		ServerMode: p.Mode,
		Algorithm:  storage.CompressionDeflate,
		Level:      p.Level,
		MinSize:    p.MinSize,
	}
}

// getBucketCompression implements GET /buckets/{name}/compression.
func (a *api) getBucketCompression(rc *reqCtx) (int, any, error) {
	s, err := compression.Load(rc.ctx, a.d.Backend, rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, describeCompression(a.d.Settings.Get().Compression, s), nil
}

// putBucketCompression implements PUT /buckets/{name}/compression. A
// setting that contradicts the administrator's policy (enabling it while
// compression.mode is "disabled", or disabling it while it is
// "enforced") is refused with 409.
func (a *api) putBucketCompression(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	data, err := rc.readBody()
	if err != nil {
		return 0, nil, err
	}
	s, err := compression.Parse(data)
	if err != nil {
		return 0, nil, badRequest("%v", err)
	}
	p := a.d.Settings.Get().Compression
	if msg := compression.Conflict(p, s); msg != "" {
		return 0, nil, errorf(http.StatusConflict, codeConflict, "%s", msg)
	}
	if err := a.d.Backend.PutBucketConfig(rc.ctx, bucket, storage.ConfigCompression, s.Encode()); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: bucket compression updated", "bucket", bucket, "enabled", s.Enabled, "user", rc.id.User)
	return http.StatusOK, describeCompression(p, s), nil
}

// deleteBucketCompression implements DELETE /buckets/{name}/compression:
// the bucket goes back to the server default (compressed only when the
// administrator enforces it). It is idempotent.
func (a *api) deleteBucketCompression(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	if _, err := a.d.Backend.GetBucket(rc.ctx, bucket); err != nil {
		return 0, nil, err
	}
	if err := a.d.Backend.DeleteBucketConfig(rc.ctx, bucket, storage.ConfigCompression); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: bucket compression setting removed", "bucket", bucket, "user", rc.id.User)
	return http.StatusNoContent, nil, nil
}
