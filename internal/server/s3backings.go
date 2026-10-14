package server

import (
	"fmt"
	"path/filepath"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	fsbackend "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/gateway"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/routed"
)

// GatewayStateDirName is the directory below an s3 gateway backing's
// system directory holding its local state (multipart upload records).
const GatewayStateDirName = "gateway"

// openS3Backing opens an s3 backing (docs/backings.md). Its path holds an
// fs backend that keeps the bucket records and configuration documents:
//
//   - blob mode: that fs backend also keeps every version's metadata and
//     stores the object data as blobs in the remote bucket;
//   - gateway mode: objects are native remote objects (internal/storage/
//     gateway), routed to by internal/storage/routed.
//
// The remote service is not contacted here: an unreachable service makes
// only this backing's buckets fail (503).
func (s *Server) openS3Backing(bc config.BackingConfig) (multi.Backing, error) {
	c := bc.S3
	client, err := s3client.New(s3client.Config{
		Endpoint:  c.Endpoint,
		Region:    c.Region,
		AccessKey: c.AccessKey,
		SecretKey: c.SecretKey,
		PathStyle: c.PathStyle(),
	})
	if err != nil {
		return multi.Backing{}, err
	}
	log := s.log.With("component", "storage", "backing", bc.Name)
	opts := fsbackend.Options{FsyncData: s.cfg.Storage.FsyncData, Logger: log}
	if c.Mode == config.S3ModeBlob {
		opts.Remotes = map[string]fsbackend.RemoteStore{
			bc.Name: {Client: client, Bucket: c.Bucket, Prefix: c.Prefix, PartSize: c.PartSize},
		}
		opts.DefaultBacking = bc.Name
	}
	meta, err := fsbackend.Open(bc.Path, opts)
	if err != nil {
		return multi.Backing{}, err
	}
	b := multi.Backing{Name: bc.Name, Kind: config.BackendS3, Mode: c.Mode, Path: meta.Root(), Backend: meta}
	if c.Mode == config.S3ModeBlob {
		return b, nil
	}
	state := filepath.Join(meta.Root(), config.SystemDirName, GatewayStateDirName)
	store, err := gateway.Open(gateway.Options{
		Name:     bc.Name,
		Client:   client,
		Bucket:   c.Bucket,
		Prefix:   c.Prefix,
		PartSize: c.PartSize,
		StateDir: state,
		SpoolDir: filepath.Join(state, "tmp"),
		Logger:   log,
	})
	if err != nil {
		_ = meta.Close()
		return multi.Backing{}, fmt.Errorf("gateway: %w", err)
	}
	b.Backend = routed.New(routed.Options{
		Primary: meta,
		Stores:  map[string]routed.DataStore{bc.Name: store},
		Default: bc.Name,
		Logger:  log,
	})
	return b, nil
}
