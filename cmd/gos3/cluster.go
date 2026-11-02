package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/server"
)

const clusterImportMaxObjectBytes = 256 << 20

func cmdCluster(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage: gos3 cluster import export --source-config FILE --output DIR\n       gos3 cluster import seed --cluster-config FILE --bundle DIR\n       gos3 cluster migrate export --source-config FILE [--source-config FILE...] --output DIR")
		return nil
	}
	if args[0] == "migrate" {
		if len(args) == 1 || args[1] == "help" || args[1] == "-h" || args[1] == "--help" {
			fmt.Fprintln(stdout, "Usage: gos3 cluster migrate export --source-config FILE [--source-config FILE...] --output DIR")
			return nil
		}
		if args[1] == "export" {
			return cmdClusterMigrateExport(ctx, args[2:], stdout, stderr)
		}
		fmt.Fprintln(stderr, "Usage: gos3 cluster migrate export [flags]")
		return errUsage
	}
	if args[0] != "import" || len(args) < 2 {
		fmt.Fprintln(stderr, "Usage: gos3 cluster import export|seed [flags]")
		return errUsage
	}
	if args[1] == "help" || args[1] == "-h" || args[1] == "--help" {
		fmt.Fprintln(stdout, "Usage: gos3 cluster import export --source-config FILE --output DIR\n       gos3 cluster import seed --cluster-config FILE --bundle DIR")
		return nil
	}
	switch args[1] {
	case "export":
		return cmdClusterImportExport(ctx, args[2:], stdout, stderr)
	case "seed":
		return cmdClusterImportSeed(ctx, args[2:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "Usage: gos3 cluster import export|seed [flags]")
		return errUsage
	}
}

// cmdClusterMigrateExport reads every stopped old voter and emits an import
// bundle. Existing cluster import seed then initializes the new static set.
func cmdClusterMigrateExport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("cluster migrate export", "gos3 cluster migrate export --source-config FILE [--source-config FILE...] --output DIR", stderr)
	var configPaths listFlag
	fs.Var(&configPaths, "source-config", "configuration of each stopped old voter; repeat for every voter")
	output := fs.String("output", "", "new private bundle directory; must not already exist")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if len(configPaths) < 2 || *output == "" {
		fs.Usage()
		return errUsage
	}
	members := make([]cluster.OfflineClusterMember, 0, len(configPaths))
	var locks []io.Closer
	var stores []cluster.DurableReplicaStore
	defer func() {
		for i := len(stores) - 1; i >= 0; i-- {
			_ = stores[i].Close()
		}
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Close()
		}
	}()
	var rootAccessKey, rootSecretKey string
	for _, path := range configPaths {
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg.Normalize()
		if err := cfg.LoadBackings(); err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid source configuration %s: %w", path, err)
		}
		if !cfg.Cluster.Enabled || cfg.SystemDir() == "" {
			return fmt.Errorf("cluster migrate: %s must describe a persistent cluster voter", path)
		}
		if rootAccessKey == "" {
			rootAccessKey, rootSecretKey = cfg.Auth.RootAccessKey, cfg.Auth.RootSecretKey
		} else if rootAccessKey != cfg.Auth.RootAccessKey || rootSecretKey != cfg.Auth.RootSecretKey {
			return errors.New("cluster migrate: root credentials differ between old voters")
		}
		lock, err := server.LockSystemDirForImport(cfg.SystemDir())
		if err != nil {
			return fmt.Errorf("locking stopped source %s: %w", path, err)
		}
		locks = append(locks, lock)
		if err := verifyOfflineAuxiliaryState(cfg.SystemDir()); err != nil {
			return err
		}
		var store cluster.DurableReplicaStore
		if cfg.Storage.Backend == config.BackendS3 {
			st := cfg.Storage.S3
			client, err := s3client.New(s3client.Config{
				Endpoint: st.Endpoint, Region: st.Region, AccessKey: st.AccessKey, SecretKey: st.SecretKey, PathStyle: st.PathStyle(),
			})
			if err != nil {
				return err
			}
			prefix := st.HomePrefix + "_gos3-cluster/" + cfg.Cluster.ID + "/" + cfg.Cluster.NodeID + "/"
			store, err = cluster.NewS3StageStore(client, st.HomeBucket, prefix, cfg.Cluster.StageMemoryBytes)
			if err != nil {
				return err
			}
		} else {
			stagePath := filepath.Join(cfg.SystemDir(), "cluster", "stages.db")
			if _, err := os.Stat(stagePath); err != nil {
				return fmt.Errorf("cluster migrate: missing local replica store %s: %w", stagePath, err)
			}
			store, err = cluster.OpenStageStore(stagePath, cfg.Cluster.StageMemoryBytes)
			if err != nil {
				return err
			}
		}
		stores = append(stores, store)
		members = append(members, cluster.OfflineClusterMember{Config: cfg.Cluster, SystemDir: cfg.SystemDir(), Store: store})
	}
	source, settings, iamDocument, err := cluster.OpenOfflineClusterSource(ctx, members)
	if err != nil {
		return err
	}
	bundle, err := cluster.ExportStandaloneBundle(ctx, source, settings, iamDocument,
		rootAccessKey, rootSecretKey, *output, clusterImportMaxObjectBytes)
	if err != nil {
		return err
	}
	var versions int
	for _, bucket := range bundle.Buckets {
		versions += len(bucket.Versions)
	}
	_, err = fmt.Fprintf(stdout, "exported %d buckets and %d versions from old cluster to %s\n", len(bundle.Buckets), versions, *output)
	return err
}

func cmdClusterImportExport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("cluster import export", "gos3 cluster import export --source-config FILE --output DIR", stderr)
	sourceConfig := fs.String("source-config", "", "configuration of the stopped standalone source")
	output := fs.String("output", "", "new private bundle directory; must not already exist")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *sourceConfig == "" || *output == "" {
		fs.Usage()
		return errUsage
	}
	cfg, err := config.Load(*sourceConfig)
	if err != nil {
		return err
	}
	if cfg.Cluster.Enabled || cfg.SystemDir() == "" {
		return errors.New("cluster import: source must be a persistent standalone server")
	}
	logger, logControl := server.NewLogger(io.Discard, "error", "text")
	source, err := server.New(cfg, logger, server.WithLogControl(logControl), server.WithOfflineInspection())
	if err != nil {
		return fmt.Errorf("opening stopped source: %w", err)
	}
	defer source.Close()
	resolved := source.Config()
	if err := verifyOfflineAuxiliaryState(resolved.SystemDir()); err != nil {
		return err
	}
	iamDocument, err := os.ReadFile(filepath.Join(resolved.SystemDir(), "iam.json"))
	if errors.Is(err, os.ErrNotExist) {
		iamDocument, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("reading source IAM: %w", err)
	}
	bundle, err := cluster.ExportStandaloneBundle(ctx, source.Backend(), source.Settings().Get(), iamDocument,
		resolved.Auth.RootAccessKey, resolved.Auth.RootSecretKey, *output, clusterImportMaxObjectBytes)
	if err != nil {
		return err
	}
	var versions int
	for _, bucket := range bundle.Buckets {
		versions += len(bucket.Versions)
	}
	_, err = fmt.Fprintf(stdout, "exported %d buckets and %d versions to %s\n", len(bundle.Buckets), versions, *output)
	return err
}

// Background jobs and snapshot scheduler state are not representable in an
// imported cluster yet. Refuse them explicitly instead of silently dropping
// a source's pending work or recovery state.
func verifyOfflineAuxiliaryState(systemDir string) error {
	for _, item := range []struct {
		file  string
		field string
	}{
		{filepath.Join("jobs", "jobs.json"), "jobs"},
		{"restic-snapshots.json", "buckets"},
	} {
		path := filepath.Join(systemDir, item.file)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cluster import: reading %s: %w", path, err)
		}
		if len(raw) > 16<<20 {
			return fmt.Errorf("cluster import: %s is too large to verify", path)
		}
		var state struct {
			Version int                        `json:"version"`
			Jobs    []json.RawMessage          `json:"jobs"`
			Buckets map[string]json.RawMessage `json:"buckets"`
		}
		if err := json.Unmarshal(raw, &state); err != nil || state.Version != 1 {
			return fmt.Errorf("cluster import: %s has invalid or unsupported state", path)
		}
		if item.field == "jobs" && len(state.Jobs) != 0 || item.field == "buckets" && len(state.Buckets) != 0 {
			return fmt.Errorf("cluster import: %s contains background work that cannot yet be imported", path)
		}
	}
	return nil
}

func cmdClusterImportSeed(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("cluster import seed", "gos3 cluster import seed --cluster-config FILE --bundle DIR", stderr)
	clusterConfig := fs.String("cluster-config", "", "configuration for this new cluster voter")
	bundleDir := fs.String("bundle", "", "verified offline bundle shared by every voter")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *clusterConfig == "" || *bundleDir == "" {
		fs.Usage()
		return errUsage
	}
	cfg, err := config.Load(*clusterConfig)
	if err != nil {
		return err
	}
	cfg.Normalize()
	if err := cfg.LoadBackings(); err != nil {
		return err
	}
	if !cfg.Cluster.Enabled {
		return errors.New("cluster import: target configuration must enable cluster mode")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid cluster configuration: %w", err)
	}
	lock, err := server.LockSystemDirForImport(cfg.SystemDir())
	if err != nil {
		return fmt.Errorf("locking offline target: %w", err)
	}
	defer lock.Close()
	var external cluster.DurableReplicaStore
	if cfg.Storage.Backend == config.BackendS3 {
		st := cfg.Storage.S3
		client, err := s3client.New(s3client.Config{
			Endpoint: st.Endpoint, Region: st.Region, AccessKey: st.AccessKey, SecretKey: st.SecretKey, PathStyle: st.PathStyle(),
		})
		if err != nil {
			return err
		}
		prefix := st.HomePrefix + "_gos3-cluster/" + cfg.Cluster.ID + "/" + cfg.Cluster.NodeID + "/"
		external, err = cluster.NewS3StageStore(client, st.HomeBucket, prefix, cfg.Cluster.StageMemoryBytes)
		if err != nil {
			return err
		}
	}
	fingerprint, err := cluster.SeedOfflineNode(ctx, cfg.Cluster, *bundleDir, cfg.SystemDir(),
		cfg.Auth.RootAccessKey, cfg.Auth.RootSecretKey, cfg.Cluster.StageMemoryBytes, external)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "seeded node %s from bundle %s (fingerprint %s)\n", cfg.Cluster.NodeID, *bundleDir, fingerprint)
	return err
}
