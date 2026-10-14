package server

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/seanmmitchell/gos3/internal/admin"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	fsbackend "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/gateway"
	"github.com/seanmmitchell/gos3/internal/storage/routed"
)

// GatewayStateDirName is the directory below the system directory holding
// the local state of gateway backings (one subdirectory per backing).
const GatewayStateDirName = "gateway"

// backings are the remote backings of storage.backings.
type backings struct {
	blobs    map[string]fsbackend.RemoteStore // blob mode, handled by the fs backend
	gateways []gateway.Options                // gateway mode, opened by openStores
	stores   map[string]routed.DataStore      // opened gateway stores
	info     []admin.Backing                  // for the admin API (no secrets)
}

// newBackings creates the clients of the configured backings. Remote
// services are not contacted here: an unreachable backing makes only its
// own buckets fail (503), not the server start. Gateway stores keep state
// in the data directory, so they are opened separately (openStores) once
// the primary backend holds the directory.
func newBackings(cfg config.Config, log *slog.Logger) (*backings, error) {
	bk := &backings{
		blobs:  make(map[string]fsbackend.RemoteStore),
		stores: make(map[string]routed.DataStore),
	}
	sysDir := cfg.SystemDir()
	for _, b := range cfg.Storage.Backings {
		client, err := s3client.New(s3client.Config{
			Endpoint:  b.Endpoint,
			Region:    b.Region,
			AccessKey: b.AccessKey,
			SecretKey: b.SecretKey,
			PathStyle: b.PathStyle(),
		})
		if err != nil {
			return nil, fmt.Errorf("storage.backings %q: %w", b.Name, err)
		}
		bk.info = append(bk.info, admin.Backing{
			Name: b.Name, Mode: b.Mode, Endpoint: b.Endpoint, Region: b.Region, Bucket: b.Bucket, Prefix: b.Prefix,
		})
		switch b.Mode {
		case config.BackingModeBlob:
			bk.blobs[b.Name] = fsbackend.RemoteStore{Client: client, Bucket: b.Bucket, Prefix: b.Prefix, PartSize: b.PartSize}
		case config.BackingModeGateway:
			opts := gateway.Options{
				Name:     b.Name,
				Client:   client,
				Bucket:   b.Bucket,
				Prefix:   b.Prefix,
				PartSize: b.PartSize,
				Logger:   log.With("component", "storage", "backing", b.Name),
			}
			if sysDir != "" {
				opts.StateDir = filepath.Join(sysDir, GatewayStateDirName, b.Name)
				opts.SpoolDir = filepath.Join(opts.StateDir, "tmp")
			}
			bk.gateways = append(bk.gateways, opts)
		}
	}
	return bk, nil
}

// openStores opens the gateway stores. On failure the stores opened so far
// are closed again.
func (bk *backings) openStores() error {
	for _, opts := range bk.gateways {
		store, err := gateway.Open(opts)
		if err != nil {
			bk.close()
			return fmt.Errorf("storage.backings %q: %w", opts.Name, err)
		}
		bk.stores[opts.Name] = store
	}
	return nil
}

// blobNames returns the names of the blob backings.
func (bk *backings) blobNames() []string {
	names := make([]string, 0, len(bk.blobs))
	for name := range bk.blobs {
		names = append(names, name)
	}
	return names
}

// close closes the opened gateway stores (when the server fails to start
// before the router owns them).
func (bk *backings) close() {
	for _, s := range bk.stores {
		_ = s.Close()
	}
}
