package config

import (
	"strings"
	"testing"
)

func validClusterConfig() Config {
	c := Default()
	c.Auth.RootAccessKey = "cluster-root-key"
	c.Auth.RootSecretKey = "cluster-root-secret-0123456789"
	c.Cluster = ClusterConfig{
		Enabled:          true,
		ID:               "test-cluster",
		NodeID:           "a",
		Peers:            []ClusterPeer{{ID: "a", URL: "https://a.example:9443"}, {ID: "b", URL: "https://b.example:9443"}},
		TLSCert:          "peer.crt",
		TLSKey:           "peer.key",
		TLSCA:            "ca.crt",
		ReplicaCount:     2,
		MinMemoryAcks:    2,
		MinDurableAcks:   2,
		StageMemoryBytes: DefaultClusterStageMemoryBytes,
	}
	return c
}

func TestClusterConfigurationBounds(t *testing.T) {
	if err := validClusterConfig().Validate(); err != nil {
		t.Fatalf("valid cluster: %v", err)
	}
	independent := validClusterConfig()
	independent.Cluster.MinMemoryAcks = 1
	if err := independent.Validate(); err != nil {
		t.Fatalf("durable threshold may exceed memory threshold: %v", err)
	}
	cases := []struct {
		name  string
		edit  func(*Config)
		field string
	}{
		{"missing ID", func(c *Config) { c.Cluster.ID = "" }, "cluster.id"},
		{"missing node ID", func(c *Config) { c.Cluster.NodeID = "" }, "cluster.node_id"},
		{"one node", func(c *Config) { c.Cluster.Peers = c.Cluster.Peers[:1] }, "cluster.peers"},
		{"duplicate IDs", func(c *Config) { c.Cluster.Peers[1].ID = "a" }, "cluster.peers[1].id"},
		{"duplicate URLs", func(c *Config) { c.Cluster.Peers[1].URL = c.Cluster.Peers[0].URL }, "cluster.peers[1].url"},
		{"unlisted node", func(c *Config) { c.Cluster.NodeID = "c" }, "cluster.node_id"},
		{"insecure peer URL", func(c *Config) { c.Cluster.Peers[1].URL = "http://b.example:9443" }, "cluster.peers[1].url"},
		{"URL credentials", func(c *Config) { c.Cluster.Peers[1].URL = "https://u:p@b.example:9443" }, "cluster.peers[1].url"},
		{"URL path", func(c *Config) { c.Cluster.Peers[1].URL = "https://b.example:9443/api" }, "cluster.peers[1].url"},
		{"URL without port", func(c *Config) { c.Cluster.Peers[1].URL = "https://b.example" }, "cluster.peers[1].url"},
		{"missing CA", func(c *Config) { c.Cluster.TLSCA = "" }, "cluster.tls_ca"},
		{"too many replicas", func(c *Config) { c.Cluster.ReplicaCount = 3 }, "cluster.replica_count"},
		{"too many memory acks", func(c *Config) { c.Cluster.MinMemoryAcks = 3 }, "cluster.min_memory_acks"},
		{"too many durable acks", func(c *Config) { c.Cluster.MinDurableAcks = 3 }, "cluster.min_durable_acks"},
		{"negative staging memory", func(c *Config) { c.Cluster.StageMemoryBytes = -1 }, "cluster.stage_memory_bytes"},
		{"insufficient staging memory", func(c *Config) { c.Cluster.StageMemoryBytes = MinClusterStageMemoryBytes - 1 }, "cluster.stage_memory_bytes"},
		{"ephemeral system state", func(c *Config) { c.Storage.Backend = BackendMemory; c.Storage.Path = "" }, "storage.path"},
		{"memory backing", func(c *Config) { c.Storage.Backend = BackendMemory }, "storage.backend"},
		{"unsynced filesystem", func(c *Config) { c.Storage.FsyncData = false }, "storage.fsync_data"},
		{"S3 without home", func(c *Config) { c.Storage.Backend = BackendS3; c.Storage.S3 = &S3Config{} }, "storage.s3.home_bucket"},
		{"extra backing", func(c *Config) { c.Storage.Backings = []BackingConfig{{Name: "other", Backend: BackendMemory}} }, "storage.backings"},
		{"node-specific root credentials", func(c *Config) { c.Auth.RootSecretKey = "" }, "auth.root_access_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validClusterConfig()
			tc.edit(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("Validate() = %v, want %s error", err, tc.field)
			}
		})
	}
}

func TestClusterConfigEnvironmentAndCopy(t *testing.T) {
	c := Default()
	if c.Cluster.StageMemoryBytes != DefaultClusterStageMemoryBytes {
		t.Fatalf("default staging memory = %d", c.Cluster.StageMemoryBytes)
	}
	if err := c.Set("GOS3_CLUSTER_PEERS", "a=https://a.example:9443,b=https://b.example:9443"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("GOS3_CLUSTER_STAGE_MEMORY_BYTES", "512MiB"); err != nil {
		t.Fatal(err)
	}
	if c.Cluster.StageMemoryBytes != 512<<20 {
		t.Fatalf("staging memory = %d", c.Cluster.StageMemoryBytes)
	}
	if len(c.Cluster.Peers) != 2 || c.Cluster.Peers[1].ID != "b" {
		t.Fatalf("peers = %+v", c.Cluster.Peers)
	}
	copy := c.Redacted()
	copy.Cluster.Peers[0].ID = "changed"
	if c.Cluster.Peers[0].ID != "a" {
		t.Fatal("Redacted shared the cluster peer slice")
	}
}
