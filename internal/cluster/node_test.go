package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/bbolt"
	"go.etcd.io/raft/v3"
)

func TestTwoNodesAdmitMetadataOnlyWithBothDurableCopies(t *testing.T) {
	const clusterID = "admission-node"
	ca, certs := peerTestCertificates(t, clusterID, "a", "b")
	nodes := map[string]*Node{}
	servers := map[string]*httptest.Server{}
	for _, name := range []string{"a", "b"} {
		name := name
		servers[name] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nodes[name].PeerHandler().ServeHTTP(w, r)
		}))
	}
	peers := []config.ClusterPeer{{ID: "a", URL: "https://" + servers["a"].Listener.Addr().String()}, {ID: "b", URL: "https://" + servers["b"].Listener.Addr().String()}}
	for _, name := range []string{"a", "b"} {
		cfg := config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		node, err := OpenNode(cfg, filepath.Join(t.TempDir(), name), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		nodes[name] = node
		servers[name].TLS = node.PeerTLSConfig()
	}
	for _, server := range servers {
		server.StartTLS()
	}
	defer func() {
		for _, server := range servers {
			server.Close()
		}
	}()
	contexts := map[string]context.CancelFunc{}
	done := map[string]chan error{}
	for name, node := range nodes {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[name] = cancel
		result := make(chan error, 1)
		done[name] = result
		go func() { result <- node.Run(ctx) }()
	}
	defer func() {
		for name, cancel := range contexts {
			if cancel == nil {
				continue
			}
			cancel()
			if err := <-done[name]; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
			if err := nodes[name].Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	var leaderName string
	deadline := time.Now().Add(8 * time.Second)
	for leaderName == "" && time.Now().Before(deadline) {
		for name, node := range nodes {
			if node.consensus.Status().RaftState == raft.StateLeader {
				leaderName = name
				break
			}
		}
		if leaderName == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if leaderName == "" {
		t.Fatal("no leader")
	}
	var id [16]byte
	id[0] = 17
	command := MetadataCommand{Namespace: MetadataVersions, Key: "photos/k/v1", Value: []byte(hex.EncodeToString(id[:]))}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := nodes[leaderName].AdmitMetadataWrite(ctx, id, "photos", "k", []byte("complete object"), command); err != nil {
		t.Fatal(err)
	}
	for name, node := range nodes {
		if err := node.ReadBarrier(ctx); err != nil {
			t.Fatalf("%s read barrier: %v", name, err)
		}
		value, found, err := node.Metadata().Get(MetadataVersions, command.Key)
		if err != nil || !found || string(value) != hex.EncodeToString(id[:]) {
			t.Fatalf("%s metadata: %q, %v, %v", name, value, found, err)
		}
		body, err := node.stages.Read(ctx, hex.EncodeToString(id[:]))
		if err != nil || string(body) != "complete object" {
			t.Fatalf("%s payload: %q, %v", name, body, err)
		}
		records, err := node.ListMetadata(ctx, MetadataVersions, "photos/k/")
		if err != nil || len(records) != 1 || records[0].Key != command.Key {
			t.Fatalf("%s linearizable version list: %+v, %v", name, records, err)
		}
	}
	// A selected replica can recover a corrupt durable copy from its peer.
	// The read barrier still requires a live quorum before any bytes are served.
	repairNode := "a"
	if leaderName == repairNode {
		repairNode = "b"
	}
	opID := hex.EncodeToString(id[:])
	if err := nodes[repairNode].stages.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(stageBucket).Put([]byte(opID), []byte("corrupt"))
	}); err != nil {
		t.Fatal(err)
	}
	committedDigest := sha256.Sum256([]byte("complete object"))
	if body, err := nodes[repairNode].ReadVerifiedPayload(ctx, id, "photos", "k", committedDigest, int64(len("complete object"))); err != nil || string(body) != "complete object" {
		t.Fatalf("repair read: %q, %v", body, err)
	}
	waitForRepairedPayload(t, nodes[repairNode], opID, "complete object")
	// A record with a valid local checksum can still differ from the
	// Raft-committed payload. Never return it or use it to repair peers.
	selected, err := SelectReplicas(clusterID, "photos", "k", []string{"a", "b"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	wrong := []byte("different object")
	wrongDigest := sha256.Sum256(wrong)
	wrongRecord := append(wrongDigest[:], wrong...)
	if err := nodes[selected[0]].stages.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(stageBucket).Put([]byte(opID), wrongRecord)
	}); err != nil {
		t.Fatal(err)
	}
	if body, err := nodes[selected[0]].ReadVerifiedPayload(ctx, id, "photos", "k", committedDigest, int64(len("complete object"))); err != nil || string(body) != "complete object" {
		t.Fatalf("committed-digest read: %q, %v", body, err)
	}
	waitForRepairedPayload(t, nodes[selected[0]], opID, "complete object")
	// A healthy quorum must not turn missing object bytes into a successful
	// read. Restore the records after checking the retryable read failure.
	for _, name := range selected {
		if err := nodes[name].stages.db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(stageBucket).Delete([]byte(opID))
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nodes[selected[0]].ReadVerifiedPayload(ctx, id, "photos", "k", committedDigest, int64(len("complete object"))); !errors.Is(err, ErrVerifiedReplicaUnavailable) {
		t.Fatalf("read with no verified replica: %v", err)
	}
	committedRecord := append(committedDigest[:], []byte("complete object")...)
	for _, name := range selected {
		if err := nodes[name].stages.db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(stageBucket).Put([]byte(opID), committedRecord)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var transactionID [16]byte
	transactionID[0] = 19
	transaction := MetadataTransaction{OperationID: hex.EncodeToString(transactionID[:]),
		Conditions: []MetadataCondition{{Namespace: MetadataVersions, Key: command.Key, Exists: true, Value: command.Value}},
		Changes: []MetadataCommand{
			{Namespace: MetadataBuckets, Key: "photos", Value: []byte("versioned")},
			{Namespace: MetadataSettings, Key: "region", Value: []byte("us-east-1")},
		}}
	if err := nodes[leaderName].AdmitTransactionWrite(ctx, transactionID, "photos", "k", []byte("bucket state"), transaction); err != nil {
		t.Fatalf("committed multi-record transaction: %v", err)
	}
	for name, node := range nodes {
		if err := node.ReadBarrier(ctx); err != nil {
			t.Fatalf("%s transaction read barrier: %v", name, err)
		}
		for _, item := range transaction.Changes {
			value, found, err := node.Metadata().Get(item.Namespace, item.Key)
			if err != nil || !found || string(value) != string(item.Value) {
				t.Fatalf("%s transaction record %s/%s: %q, %v, %v", name, item.Namespace, item.Key, value, found, err)
			}
		}
	}
	transactionID[0] = 20
	conflict := MetadataTransaction{OperationID: hex.EncodeToString(transactionID[:]),
		Conditions: []MetadataCondition{{Namespace: MetadataBuckets, Key: "photos", Exists: true, Value: []byte("unversioned")}},
		Changes:    []MetadataCommand{{Namespace: MetadataRoles, Key: "unsafe", Value: []byte("grant")}},
	}
	if err := nodes[leaderName].AdmitTransactionWrite(ctx, transactionID, "photos", "k", []byte("conflict"), conflict); !errors.Is(err, ErrMetadataCondition) {
		t.Fatalf("failed condition returned %v", err)
	}
	if _, found, err := nodes[leaderName].Metadata().Get(MetadataRoles, "unsafe"); err != nil || found {
		t.Fatalf("rejected transaction changed roles: %v, %v", found, err)
	}
	failed := "a"
	if leaderName == failed {
		failed = "b"
	}
	contexts[failed]()
	if err := <-done[failed]; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := nodes[failed].Close(); err != nil {
		t.Fatal(err)
	}
	contexts[failed] = nil
	servers[failed].Close()
	id[0] = 18
	ctxUnavailable, stopUnavailable := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer stopUnavailable()
	err = nodes[leaderName].AdmitMetadataWrite(ctxUnavailable, id, "photos", "k2", []byte("unavailable"), MetadataCommand{Namespace: MetadataVersions, Key: "photos/k2/v1", Value: []byte("pending")})
	if err == nil {
		t.Fatal("two-node write succeeded after a node failed")
	}
	if _, found, err := nodes[leaderName].Metadata().Get(MetadataVersions, "photos/k2/v1"); err != nil || found {
		t.Fatalf("unavailable write published: %v, %v", found, err)
	}
	bucketIntent, err := NewBucketCreateIntent("unavailable", storage.CreateBucketOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bucketCtx, stopBucket := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stopBucket()
	if _, err := NewBucketCatalog(nodes[leaderName]).CreateBucket(bucketCtx, bucketIntent); err == nil {
		t.Fatal("two-node bucket create succeeded after a voter failed")
	}
	if _, found, err := nodes[leaderName].Metadata().Get(MetadataBuckets, bucketKey("unavailable")); err != nil || found {
		t.Fatalf("unavailable bucket create published: %v, %v", found, err)
	}
	readCtx, stopRead := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stopRead()
	if _, _, err := nodes[leaderName].GetMetadata(readCtx, MetadataBuckets, "photos"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("two-node read succeeded without a quorum: %v", err)
	}
	id[0] = 17
	if err := nodes[leaderName].AdmitMetadataWrite(context.Background(), id, "photos", "k", []byte("complete object"), command); err != nil {
		t.Fatalf("retry of already committed operation should preserve its original acknowledgment result: %v", err)
	}
}

func TestThreeNodesAdmitWithTwoCopiesAfterThirdFails(t *testing.T) {
	const clusterID = "two-of-three-node"
	ca, certs := peerTestCertificates(t, clusterID, "a", "b", "c")
	names := []string{"a", "b", "c"}
	nodes := map[string]*Node{}
	slots := map[string]*atomic.Pointer[Node]{}
	for _, name := range names {
		slots[name] = new(atomic.Pointer[Node])
	}
	servers := map[string]*httptest.Server{}
	dirs := map[string]string{}
	configs := map[string]config.ClusterConfig{}
	for _, name := range names {
		name := name
		servers[name] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slots[name].Load().PeerHandler().ServeHTTP(w, r)
		}))
	}
	peers := make([]config.ClusterPeer, 0, len(names))
	for _, name := range names {
		peers = append(peers, config.ClusterPeer{ID: name, URL: "https://" + servers[name].Listener.Addr().String()})
	}
	for _, name := range names {
		cfg := config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 3, MinMemoryAcks: 2, MinDurableAcks: 2}
		configs[name] = cfg
		dirs[name] = filepath.Join(t.TempDir(), name)
		node, err := OpenNode(cfg, dirs[name], 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		nodes[name] = node
		slots[name].Store(node)
		servers[name].TLS = node.PeerTLSConfig()
	}
	for _, server := range servers {
		server.StartTLS()
		defer server.Close()
	}
	contexts := map[string]context.CancelFunc{}
	done := map[string]chan error{}
	for name, node := range nodes {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[name] = cancel
		result := make(chan error, 1)
		done[name] = result
		go func() { result <- node.Run(ctx) }()
	}
	defer func() {
		for name, cancel := range contexts {
			if cancel == nil {
				continue
			}
			cancel()
			if err := <-done[name]; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
			if err := nodes[name].Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	var leader string
	deadline := time.Now().Add(8 * time.Second)
	for leader == "" && time.Now().Before(deadline) {
		for _, name := range names {
			if nodes[name].consensus.Status().RaftState == raft.StateLeader {
				leader = name
				break
			}
		}
		if leader == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if leader == "" {
		t.Fatal("no leader")
	}
	var failed, entry string
	for _, name := range names {
		if name == leader {
			continue
		}
		if failed == "" {
			failed = name
		} else {
			entry = name
		}
	}
	contexts[failed]()
	if err := <-done[failed]; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := nodes[failed].Close(); err != nil {
		t.Fatal(err)
	}
	contexts[failed] = nil
	failedAddress := servers[failed].Listener.Addr().String()
	servers[failed].Close()
	var id [16]byte
	id[0] = 31
	command := MetadataCommand{Namespace: MetadataVersions, Key: "photos/quorum/v1", Value: []byte("two copies")}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := nodes[entry].AdmitMetadataWrite(ctx, id, "photos", "quorum", []byte("payload"), command); err != nil {
		t.Fatalf("follower admission with one failed replica: %v", err)
	}
	for _, name := range []string{leader, entry} {
		if err := nodes[name].ReadBarrier(ctx); err != nil {
			t.Fatalf("%s barrier: %v", name, err)
		}
		value, found, err := nodes[name].Metadata().Get(command.Namespace, command.Key)
		if err != nil || !found || string(value) != "two copies" {
			t.Fatalf("%s metadata: %q, %v, %v", name, value, found, err)
		}
		body, err := nodes[name].ReadVerifiedPayload(ctx, id, "photos", "quorum", sha256.Sum256([]byte("payload")), int64(len("payload")))
		if err != nil || string(body) != "payload" {
			t.Fatalf("%s payload: %q, %v", name, body, err)
		}
	}
	intent, err := NewBucketCreateIntent("shared", storage.CreateBucketOptions{Region: "us-east-1", Owner: "alice", ObjectLockEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewBucketCatalog(nodes[entry]).CreateBucket(ctx, intent)
	if err != nil {
		t.Fatalf("create bucket through follower with two durable nodes: %v", err)
	}
	if created.Versioning != storage.VersioningEnabled || !created.ObjectLockEnabled {
		t.Fatalf("bucket options lost: %+v", created)
	}
	if retried, err := NewBucketCatalog(nodes[entry]).CreateBucket(ctx, intent); err != nil || retried.InstanceID != created.InstanceID {
		t.Fatalf("idempotent bucket create retry: %+v, %v", retried, err)
	}
	for _, name := range []string{leader, entry} {
		catalog := NewBucketCatalog(nodes[name])
		got, err := catalog.GetBucket(ctx, "shared")
		if err != nil || got.InstanceID != created.InstanceID || got.Owner != "alice" {
			t.Fatalf("%s shared bucket: %+v, %v", name, got, err)
		}
		listed, err := catalog.ListBuckets(ctx)
		if err != nil || len(listed) != 1 || listed[0].Name != "shared" {
			t.Fatalf("%s bucket list: %+v, %v", name, listed, err)
		}
	}
	conflictingIntent, err := NewBucketCreateIntent("shared", storage.CreateBucketOptions{Region: "us-west-2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewBucketCatalog(nodes[leader]).CreateBucket(ctx, conflictingIntent); !errors.Is(err, storage.ErrBucketExists) {
		t.Fatalf("concurrent bucket name conflict: %v", err)
	}
	if err := NewBucketCatalog(nodes[entry]).SetBucketVersioning(ctx, "shared", storage.VersioningSuspended); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("object-locked bucket allowed versioning suspension: %v", err)
	}
	plainIntent, err := NewBucketCreateIntent("plain", storage.CreateBucketOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewBucketCatalog(nodes[entry]).CreateBucket(ctx, plainIntent); err != nil {
		t.Fatal(err)
	}
	if err := NewBucketCatalog(nodes[entry]).SetBucketVersioning(ctx, "plain", storage.VersioningSuspended); err != nil {
		t.Fatalf("suspend never-versioned bucket: %v", err)
	}
	if err := NewBucketCatalog(nodes[entry]).SetBucketVersioning(ctx, "plain", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	firstPut, err := NewObjectPutIntent("plain", "photo.jpg", []byte("first image"), storage.ObjectMeta{ETag: "etag-first", ContentType: "image/jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	firstVersion, err := NewObjectCatalog(nodes[entry]).PutVersion(ctx, firstPut)
	if err != nil {
		t.Fatalf("first object through follower: %v", err)
	}
	secondPut, err := NewObjectPutIntent("plain", "photo.jpg", []byte("second image"), storage.ObjectMeta{ETag: "etag-second", ContentType: "image/jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	secondPut.IfMatch = "etag-first"
	secondVersion, err := NewObjectCatalog(nodes[leader]).PutVersion(ctx, secondPut)
	if err != nil {
		t.Fatalf("conditional overwrite through leader: %v", err)
	}
	if _, err := NewObjectCatalog(nodes[entry]).PutVersion(ctx, firstPut); err != nil {
		t.Fatalf("idempotent retry of older committed version: %v", err)
	}
	for _, name := range []string{leader, entry} {
		catalog := NewObjectCatalog(nodes[name])
		latest, body, err := catalog.GetVersion(ctx, "plain", "photo.jpg", "")
		if err != nil || latest.VersionID != secondVersion.VersionID || !latest.IsLatest || string(body) != "second image" {
			t.Fatalf("%s latest object: %+v, %q, %v", name, latest, body, err)
		}
		older, body, err := catalog.GetVersion(ctx, "plain", "photo.jpg", firstVersion.VersionID)
		if err != nil || older.IsLatest || string(body) != "first image" {
			t.Fatalf("%s older object: %+v, %q, %v", name, older, body, err)
		}
		versions, err := catalog.ListVersions(ctx, "plain", "photo.jpg")
		if err != nil || len(versions) != 2 || versions[0].VersionID != secondVersion.VersionID || versions[1].VersionID != firstVersion.VersionID {
			t.Fatalf("%s ordered versions: %+v, %v", name, versions, err)
		}
	}
	blockedPut, err := NewObjectPutIntent("plain", "photo.jpg", []byte("should not commit"), storage.ObjectMeta{ETag: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	blockedPut.IfNoneMatch = true
	if _, err := NewObjectCatalog(nodes[entry]).PutVersion(ctx, blockedPut); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("If-None-Match accepted existing object: %v", err)
	}
	markerPut, err := NewObjectPutIntent("plain", "photo.jpg", nil, storage.ObjectMeta{})
	if err != nil {
		t.Fatal(err)
	}
	markerPut.DeleteMarker = true
	marker, err := NewObjectCatalog(nodes[entry]).PutVersion(ctx, markerPut)
	if err != nil {
		t.Fatalf("delete marker through follower: %v", err)
	}
	for _, name := range []string{leader, entry} {
		info, body, err := NewObjectCatalog(nodes[name]).GetVersion(ctx, "plain", "photo.jpg", "")
		if err != nil || info.VersionID != marker.VersionID || !info.DeleteMarker || body != nil {
			t.Fatalf("%s latest delete marker: %+v, %q, %v", name, info, body, err)
		}
	}
	afterMarker, err := NewObjectPutIntent("plain", "photo.jpg", []byte("after marker"), storage.ObjectMeta{ETag: "etag-after"})
	if err != nil {
		t.Fatal(err)
	}
	afterMarker.IfNoneMatch = true
	if _, err := NewObjectCatalog(nodes[leader]).PutVersion(ctx, afterMarker); err != nil {
		t.Fatalf("If-None-Match after delete marker: %v", err)
	}
	if err := NewBucketCatalog(nodes[leader]).SetBucketVersioning(ctx, "plain", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{leader, entry} {
		info, err := NewBucketCatalog(nodes[name]).GetBucket(ctx, "plain")
		if err != nil || info.Versioning != storage.VersioningSuspended {
			t.Fatalf("%s versioning after two cluster writes: %+v, %v", name, info, err)
		}
	}
	// The third voter missed committed versions while stopped. Rejoin it at
	// its original peer address and wait for the background placement scan.
	listener, err := net.Listen("tcp", failedAddress)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenNode(configs[failed], dirs[failed], 1<<20)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	nodes[failed] = restarted
	slots[failed].Store(restarted)
	name := failed
	peerServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slots[name].Load().PeerHandler().ServeHTTP(w, r)
	}))
	_ = peerServer.Listener.Close()
	peerServer.Listener = listener
	peerServer.TLS = restarted.PeerTLSConfig()
	peerServer.StartTLS()
	defer peerServer.Close()
	restartCtx, restartCancel := context.WithCancel(context.Background())
	contexts[failed] = restartCancel
	result := make(chan error, 1)
	done[failed] = result
	go func() { result <- restarted.Run(restartCtx) }()
	repairDeadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(repairDeadline) {
		local, readErr := restarted.stages.Read(context.Background(), firstVersion.VersionID)
		restarted.repairMu.Lock()
		repair := restarted.lastRepair
		restarted.repairMu.Unlock()
		if readErr == nil && string(local) == "first image" && repair.Repaired > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("returning voter did not repair committed version: %+v", restarted.lastRepair)
}
