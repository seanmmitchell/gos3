package native

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// Writes with an unknown outcome
//
// A write the remote answered with a 5xx or not at all (a transport
// error) may have been applied. putObject and completeRemote resolve that
// by reading the key back and comparing its ETag with the one the write
// produces: the MD5 of the content for a single PUT, and for a multipart
// upload the S3 multipart ETag (the MD5 of the parts' MD5s, "-" and the
// number of parts). Services whose ETags are not MD5-based (AWS with
// SSE-KMS) never match, and the write then fails as before.

// ambiguous reports a failure after which a write may have been applied:
// a 5xx answer (not 501) or a transport error.
func ambiguous(err error) bool {
	return s3client.IsUnavailable(err) && !s3client.IsNotImplemented(err)
}

// landed reports whether the latest version of t/key has the ETag
// expected ("" never matches), returning it as a write result.
func (s *Store) landed(ctx context.Context, t *target, key, expected string) (s3client.WriteResult, bool) {
	if expected == "" {
		return s3client.WriteResult{}, false
	}
	var a s3client.ObjectAttrs
	err := s.call(context.WithoutCancel(ctx), t, func(c *s3client.Client) error {
		var err error
		a, err = c.HeadObjectWith(context.WithoutCancel(ctx), t.b.Remote, key, s3client.HeadOptions{})
		return err
	})
	if err != nil || a.ETag != expected {
		return s3client.WriteResult{}, false
	}
	s.log.Info("native: a write whose outcome was unknown was applied", "backing", s.name, "bucket", t.bucket, "key", key)
	return s3client.WriteResult{ETag: a.ETag, VersionID: a.VersionID}, true
}

// expectedETag returns the ETag a write of r[0:size] gets: the MD5 (sum,
// or computed when nil) up to partSize, the multipart ETag of partSize
// parts above it ("" when r cannot be read).
func expectedETag(r io.ReaderAt, size int64, sum []byte, partSize int64) string {
	if size <= partSize {
		if sum != nil {
			return hex.EncodeToString(sum)
		}
		d, err := md5Section(r, 0, size)
		if err != nil {
			return ""
		}
		return hex.EncodeToString(d)
	}
	var sums [][]byte
	for off := int64(0); off < size; off += partSize {
		d, err := md5Section(r, off, min(partSize, size-off))
		if err != nil {
			return ""
		}
		sums = append(sums, d)
	}
	return multipartETag(sums)
}

// completedETag returns the multipart ETag of a completion with parts (""
// when a part ETag is not an MD5).
func completedETag(parts []s3client.CompletedPart) string {
	sums := make([][]byte, len(parts))
	for i, p := range parts {
		d, err := hex.DecodeString(strings.Trim(p.ETag, `"`))
		if err != nil || len(d) != md5.Size {
			return ""
		}
		sums[i] = d
	}
	return multipartETag(sums)
}

// multipartETag returns the S3 multipart ETag of parts with the given
// MD5s.
func multipartETag(sums [][]byte) string {
	h := md5.New()
	for _, d := range sums {
		h.Write(d)
	}
	return fmt.Sprintf("%s-%d", hex.EncodeToString(h.Sum(nil)), len(sums))
}

// md5Section returns the MD5 of r[off:off+n].
func md5Section(r io.ReaderAt, off, n int64) ([]byte, error) {
	h := md5.New()
	if _, err := io.Copy(h, io.NewSectionReader(r, off, n)); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
