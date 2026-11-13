package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// checkClusterDataDir prevents a cluster node from silently starting with a
// fresh namespace over an existing standalone server's data. A completed
// offline import creates the cluster Raft database before normal startup.
func (s *Server) checkClusterDataDir() error {
	raftDB := filepath.Join(s.systemDir, "cluster", "raft.db")
	if _, err := os.Stat(raftDB); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking cluster state %s: %w", raftDB, err)
	}
	for _, name := range []string{
		"meta.db", "iam.json", "settings.json", "placements.json",
		"backings.json", "root-credentials.json", "server-id",
	} {
		path := filepath.Join(s.systemDir, name)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("cluster data path contains standalone state %s; stop the standalone server and run gos3 cluster import before starting this node", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking standalone state %s: %w", path, err)
		}
	}
	return nil
}

// startCluster binds the mTLS peer listener before waiting for consensus.
// This lets nodes started in any order elect a leader and initialize the
// settings namespace before their public listeners accept requests.
func (s *Server) startCluster() error {
	if s.clusterNode == nil {
		return nil
	}
	var origin string
	for _, peer := range s.cfg.Cluster.Peers {
		if peer.ID == s.cfg.Cluster.NodeID {
			origin = peer.URL
			break
		}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return errors.New("cluster: local peer origin is invalid")
	}
	listener := s.peerListener
	if listener == nil {
		listener, err = net.Listen("tcp", u.Host)
		if err != nil {
			return fmt.Errorf("cluster peer listener %s: %w", u.Host, err)
		}
	} else if listener.Addr().String() != u.Host {
		return fmt.Errorf("cluster peer listener %s does not match configured origin %s", listener.Addr(), u.Host)
	}
	s.peerListener = listener
	handler := s.clusterNode.PeerHandler()
	if s.peerHandlerWrap != nil {
		handler = s.peerHandlerWrap(handler)
	}
	s.peerServer = &http.Server{
		Handler:           handler,
		TLSConfig:         s.clusterNode.PeerTLSConfig(),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	s.peerDone = make(chan error, 1)
	go func() { s.peerDone <- s.peerServer.ServeTLS(listener, "", "") }()
	s.clusterDone = make(chan error, 1)
	go func() { s.clusterDone <- s.clusterNode.Run(s.lifeCtx) }()
	ctx, cancel := context.WithTimeout(s.lifeCtx, 30*time.Second)
	defer cancel()
	if err := s.settings.Initialize(ctx); err != nil {
		return fmt.Errorf("cluster settings initialization: %w", err)
	}
	return nil
}
