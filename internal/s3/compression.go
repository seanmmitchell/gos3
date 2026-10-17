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

// withOptions returns the backend's write-options interface when opts ask
// for anything (compression or encryption), nil when the plain Backend
// methods suffice. A backend that cannot encrypt fails with
// errors.ErrUnsupported: encryption is never dropped. (Compression is only
// requested from CompressingBackends: see objectCompression.)
func (s *Server) withOptions(opts storage.WriteOptions) (storage.CompressingBackend, error) {
	if opts == (storage.WriteOptions{}) {
		return nil, nil
	}
	cb, ok := s.backend.(storage.CompressingBackend)
	if !ok && opts.Encryption != "" {
		return nil, errors.ErrUnsupported
	}
	return cb, nil
}

// newObjectWriter starts a new version of bucket/key, compressed as c says
// and encrypted with the provider encryption ("" = unencrypted; see
// prepareEncryption).
func (s *Server) newObjectWriter(ctx context.Context, bucket, key string, c storage.Compression, encryption string) (storage.ObjectWriter, error) {
	opts := storage.WriteOptions{Compression: c, Encryption: encryption}
	cb, err := s.withOptions(opts)
	if err != nil {
		return nil, err
	}
	if cb != nil {
		return cb.NewObjectWriterWith(ctx, bucket, key, opts)
	}
	return s.backend.NewObjectWriter(ctx, bucket, key)
}

// completeMultipartUpload is Backend.CompleteMultipartUpload storing the
// object as the compression policy says, encrypted when the upload
// requested SSE (prepareEncryption decides the provider; uploads created
// by older builds are encrypted too). userAgent is the completing
// request's User-Agent: the encryption is decided again at completion (an
// upload without SSE is archived when the bucket's settings say so by
// then), so restic's own uploads must be refused here too.
func (s *Server) completeMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, cond storage.CommitOptions, userAgent string) (storage.ObjectInfo, error) {
	if err := s.prepareEncryption(ctx, bucket, &meta, userAgent); err != nil {
		return storage.ObjectInfo{}, err
	}
	var size int64
	for _, n := range meta.PartSizes {
		size += n
	}
	opts := storage.WriteOptions{Compression: s.objectCompression(ctx, bucket, meta, size), Encryption: meta.Encryption}
	cb, err := s.withOptions(opts)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if cb != nil {
		return cb.CompleteMultipartUploadWith(ctx, bucket, key, uploadID, partNumbers, meta, cond, opts)
	}
	return s.backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, cond)
}

// declaredSize returns the content length the client declared (-1 when
// unknown).
func (p *payloadReader) declaredSize() int64 { return p.expected }
