// Package compression decides which new object versions are stored
// compressed: the bucket's compression setting combined with the server
// administrator's policy (config.CompressionSettings), refined per object
// by size, media type and content encoding.
//
// The bucket setting is a JSON document stored under the bucket
// configuration key storage.ConfigCompression and managed through the admin
// API (GET/PUT/DELETE /_gos3/admin/v1/buckets/{name}/compression). The S3
// layer asks Decide for every object it writes; backends implementing
// storage.CompressingBackend then store the data compressed, transparently
// for clients.
package compression

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// BucketSetting is a bucket's compression setting (the JSON document
// stored under storage.ConfigCompression).
type BucketSetting struct {
	// Enabled asks for new objects of the bucket to be stored compressed
	// while the server's compression mode is "optional".
	Enabled bool `json:"enabled"`
}

// maxSettingSize bounds a stored setting document.
const maxSettingSize = 4 << 10

// Parse decodes a stored (or submitted) setting document strictly:
// unknown fields and trailing data are errors.
func Parse(data []byte) (BucketSetting, error) {
	var s BucketSetting
	if len(data) > maxSettingSize {
		return s, errors.New("compression setting too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return BucketSetting{}, fmt.Errorf("invalid compression setting: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return BucketSetting{}, errors.New("invalid compression setting: unexpected data after the JSON object")
	}
	return s, nil
}

// Encode returns the stored form of s.
func (s BucketSetting) Encode() []byte {
	data, _ := json.Marshal(s) // cannot fail
	return data
}

// Load returns the setting of bucket (the zero value when none is
// stored). A stored document that does not parse is an error.
func Load(ctx context.Context, b storage.Backend, bucket string) (BucketSetting, error) {
	data, err := b.GetBucketConfig(ctx, bucket, storage.ConfigCompression)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return BucketSetting{}, nil
	}
	if err != nil {
		return BucketSetting{}, err
	}
	return Parse(data)
}

// Effective reports whether a bucket with setting s compresses new
// objects under the server policy p: always when p enforces compression,
// never when p disables it, and as the bucket says otherwise.
func Effective(p config.CompressionSettings, s BucketSetting) bool {
	switch p.Mode {
	case config.CompressionEnforced:
		return true
	case config.CompressionDisabled:
		return false
	default:
		return s.Enabled
	}
}

// Conflict explains why the setting s cannot be stored under the server
// policy p ("" when it can): a bucket cannot turn compression on while the
// administrator disables it, nor off while the administrator enforces it.
func Conflict(p config.CompressionSettings, s BucketSetting) string {
	switch {
	case p.Mode == config.CompressionDisabled && s.Enabled:
		return "compression is disabled on this server by its administrator (compression.mode is \"disabled\")"
	case p.Mode == config.CompressionEnforced && !s.Enabled:
		return "compression is enforced on this server by its administrator (compression.mode is \"enforced\")"
	}
	return ""
}

// Object describes a new object version for Decide.
type Object struct {
	ContentType     string
	ContentEncoding string
	// Size is the content size when known up front, -1 otherwise.
	Size int64
}

// Decide returns how a new object of a bucket whose compression is
// enabled (Effective) is to be stored under policy p: compressed with the
// policy's level, unless the object is empty or smaller than p.MinSize, or
// its content is compressed already (a compressing Content-Encoding, or a
// media type in p.SkipContentTypes). A disabled bucket gets the zero
// Compression.
func Decide(p config.CompressionSettings, enabled bool, o Object) storage.Compression {
	if !enabled || o.Size == 0 || (o.Size > 0 && o.Size < p.MinSize) {
		return storage.Compression{}
	}
	if compressedEncoding(o.ContentEncoding) || config.MatchContentType(p.SkipContentTypes, o.ContentType) {
		return storage.Compression{}
	}
	return storage.Compression{Algorithm: storage.CompressionDeflate, Level: p.Level}
}

// compressedEncodings are Content-Encoding codings whose data is
// compressed already.
var compressedEncodings = map[string]bool{
	"gzip": true, "x-gzip": true, "deflate": true, "br": true, "zstd": true,
	"compress": true, "x-compress": true, "lz4": true, "xz": true, "bzip2": true,
}

// compressedEncoding reports whether a Content-Encoding value lists a
// compressing coding.
func compressedEncoding(ce string) bool {
	for c := range strings.SplitSeq(ce, ",") {
		if compressedEncodings[strings.ToLower(strings.TrimSpace(c))] {
			return true
		}
	}
	return false
}
