package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ClusterConfig describes the fixed members of an opt-in cluster. Peers
// include this node. Cluster state and object placement are deliberately
// separate from discovery, whose unauthenticated hints cannot grant access.
type ClusterConfig struct {
	Enabled          bool          `yaml:"enabled"`
	ID               string        `yaml:"id"`
	NodeID           string        `yaml:"node_id"`
	Peers            []ClusterPeer `yaml:"peers"`
	TLSCert          string        `yaml:"tls_cert"`
	TLSKey           string        `yaml:"tls_key"`
	TLSCA            string        `yaml:"tls_ca"`
	ReplicaCount     int           `yaml:"replica_count"`
	MinMemoryAcks    int           `yaml:"min_memory_acks"`
	MinDurableAcks   int           `yaml:"min_durable_acks"`
	StageMemoryBytes int64         `yaml:"stage_memory_bytes"`
}

// ClusterPeer is a voting member with a stable ID and HTTPS peer endpoint.
type ClusterPeer struct {
	ID  string `yaml:"id"`
	URL string `yaml:"url"`
}

const DefaultClusterCopies = 2
const DefaultClusterStageMemoryBytes int64 = 256 << 20
const MinClusterStageMemoryBytes int64 = 1 << 20

var clusterIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func clusterPeersEnv(c *Config, v string) error {
	if strings.TrimSpace(v) == "" {
		c.Cluster.Peers = nil
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(v), "[") {
		var peers []ClusterPeer
		if err := json.Unmarshal([]byte(v), &peers); err != nil {
			return fmt.Errorf("cluster.peers: %w", err)
		}
		c.Cluster.Peers = peers
		return nil
	}
	peers := make([]ClusterPeer, 0, strings.Count(v, ",")+1)
	for _, part := range strings.Split(v, ",") {
		id, endpoint, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || id == "" || endpoint == "" {
			return fmt.Errorf("cluster.peers: expected id=https://host:port, got %q", part)
		}
		peers = append(peers, ClusterPeer{ID: id, URL: endpoint})
	}
	c.Cluster.Peers = peers
	return nil
}

// validateCluster checks that an enabled cluster cannot claim write safety
// which its configured membership could never provide.
func (c Config) validateCluster() error {
	x := c.Cluster
	if !x.Enabled {
		return nil
	}
	if !clusterIDPattern.MatchString(x.ID) {
		return errors.New("cluster.id: must be 1-64 characters of letters, digits, dot, underscore or hyphen, starting with a letter or digit")
	}
	if !clusterIDPattern.MatchString(x.NodeID) {
		return errors.New("cluster.node_id: must be 1-64 characters of letters, digits, dot, underscore or hyphen, starting with a letter or digit")
	}
	if len(x.Peers) < 2 || len(x.Peers) > 64 {
		return errors.New("cluster.peers: must contain 2-64 voting nodes, including this node")
	}
	ids := make(map[string]bool, len(x.Peers))
	urls := make(map[string]bool, len(x.Peers))
	for i, p := range x.Peers {
		if !clusterIDPattern.MatchString(p.ID) {
			return fmt.Errorf("cluster.peers[%d].id: invalid node ID", i)
		}
		if ids[p.ID] {
			return fmt.Errorf("cluster.peers[%d].id: duplicate node ID %q", i, p.ID)
		}
		ids[p.ID] = true
		u, err := url.Parse(p.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("cluster.peers[%d].url: must be an HTTPS origin without credentials, path, query or fragment", i)
		}
		if urls[p.URL] {
			return fmt.Errorf("cluster.peers[%d].url: duplicate endpoint %q", i, p.URL)
		}
		urls[p.URL] = true
	}
	if !ids[x.NodeID] {
		return errors.New("cluster.node_id: must appear in cluster.peers")
	}
	if x.TLSCert == "" || x.TLSKey == "" || x.TLSCA == "" {
		return errors.New("cluster.tls_cert, cluster.tls_key and cluster.tls_ca: all are required for mutual TLS")
	}
	if x.ReplicaCount < 2 || x.ReplicaCount > len(x.Peers) {
		return fmt.Errorf("cluster.replica_count: must be 2-%d", len(x.Peers))
	}
	if x.MinMemoryAcks < 1 || x.MinMemoryAcks > x.ReplicaCount {
		return fmt.Errorf("cluster.min_memory_acks: must be 1-%d", x.ReplicaCount)
	}
	if x.MinDurableAcks < 1 || x.MinDurableAcks > x.ReplicaCount {
		return fmt.Errorf("cluster.min_durable_acks: must be 1-%d", x.ReplicaCount)
	}
	if x.StageMemoryBytes < MinClusterStageMemoryBytes {
		return fmt.Errorf("cluster.stage_memory_bytes: must be at least %d bytes", MinClusterStageMemoryBytes)
	}
	if c.Storage.Path == "" {
		return errors.New("storage.path: a persistent system directory is required for cluster state")
	}
	switch c.Storage.Backend {
	case BackendFS:
		if !c.Storage.FsyncData {
			return errors.New("storage.fsync_data: cluster filesystem durability requires fsync")
		}
	case BackendS3:
		if c.Storage.S3 == nil || c.Storage.S3.HomeBucket == "" {
			return errors.New("storage.s3.home_bucket: cluster S3 durability requires a configured home bucket")
		}
	default:
		return errors.New("storage.backend: cluster durability requires an fs or s3 backing")
	}
	if len(c.Storage.Backings) != 0 {
		return errors.New("storage.backings: additional per-node backings are not yet supported in cluster mode")
	}
	if c.Auth.RootAccessKey == "" || c.Auth.RootSecretKey == "" {
		return errors.New("auth.root_access_key and auth.root_secret_key: configure identical root credentials on every cluster node")
	}
	return nil
}
