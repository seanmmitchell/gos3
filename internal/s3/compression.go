package s3

import (
	"context"
	"errors"
	"slices"

	"github.com/seanmmitchell/gos3/internal/compression"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

// Transparent compression of new objects (package compression). Whether
// a new version is stored compressed depends on the server's compression
// policy (Settings.Compression, set by the administrator), the bucket's
// compression setting and the object itself; clients never see the
// difference: content, ETags, checksums and sizes are those of the
// uncompressed data.

// compressionPolicy returns the server's compression policy. Settings
// without one (the store New creates when Options.Settings is nil, which
// is never validated) get the default policy: buckets decide.
func (s *Server) compressionPolicy() config.CompressionSettings {
	if p := s.settings.Get().Compression; p.Mode != "" && compress.ValidLevel(p.Level) {
		return p
	}
	return config.CompressionSettings{
		Mode:             config.CompressionOptional,
		Level:            config.DefaultCompressionLevel,
		MinSize:          config.DefaultCompressionMinSize,
		SkipContentTypes: slices.Clone(config.DefaultSkipContentTypes),
	}
}

// objectCompression returns how a new object of bucket with metadata meta
// and content size size (-1 = unknown) is to be stored. A bucket setting
// that cannot be read leaves the object uncompressed (and is logged): it
// must not fail the write.
func (s *Server) objectCompression(ctx context.Context, bucket string, meta storage.ObjectMeta, size int64) storage.Compression {
	if _, ok := s.backend.(storage.CompressingBackend); !ok {
		return storage.Compression{}
	}
	p := s.compressionPolicy()
	var bs compression.BucketSetting
	if p.Mode == config.CompressionOptional {
		var err error
		if bs, err = compression.Load(ctx, s.backend, bucket); err != nil {
			if !errors.Is(err, storage.ErrBucketNotFound) && ctx.Err() == nil {
				s.log.Warn("s3: cannot read the bucket's compression setting; storing uncompressed",
					"bucket", bucket, "err", err)
			}
			return storage.Compression{}
		}
	}
	return compression.Decide(p, compression.Effective(p, bs), compression.Object{
		ContentType:     meta.ContentType,
		ContentEncoding: meta.ContentEncoding,
		Size:            size,
	})
}

// newObjectWriter starts a new version of bucket/key, compressed as c
// says when the backend supports it.
func (s *Server) newObjectWriter(ctx context.Context, bucket, key string, c storage.Compression) (storage.ObjectWriter, error) {
	if cb, ok := s.backend.(storage.CompressingBackend); ok && c.Enabled() {
		return cb.NewObjectWriterWith(ctx, bucket, key, storage.WriteOptions{Compression: c})
	}
	return s.backend.NewObjectWriter(ctx, bucket, key)
}

// completeMultipartUpload is Backend.CompleteMultipartUpload storing the
// object as the compression policy says.
func (s *Server) completeMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, cond storage.CommitOptions) (storage.ObjectInfo, error) {
	var size int64
	for _, n := range meta.PartSizes {
		size += n
	}
	if cb, ok := s.backend.(storage.CompressingBackend); ok {
		if c := s.objectCompression(ctx, bucket, meta, size); c.Enabled() {
			return cb.CompleteMultipartUploadWith(ctx, bucket, key, uploadID, partNumbers, meta, cond,
				storage.WriteOptions{Compression: c})
		}
	}
	return s.backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, cond)
}

// declaredSize returns the content length the client declared (-1 when
// unknown).
func (p *payloadReader) declaredSize() int64 { return p.expected }
