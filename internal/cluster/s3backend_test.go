package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/replication"
	"github.com/seanmmitchell/gos3/internal/s3"
	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/bbolt"
	"go.etcd.io/raft/v3"
)

func TestClusterBackendMapsUnavailableConsensusAndReplicasToRetryable(t *testing.T) {
	for name, cause := range map[string]error{
		"stopped consensus": ErrConsensusUnavailable,
		"stopped raft node": raft.ErrStopped,
		"dropped proposal":  raft.ErrProposalDropped,
		"missing replica":   ErrVerifiedReplicaUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			err := clusterBackendError(fmt.Errorf("read or write: %w", cause))
			if !errors.Is(err, storage.ErrBackingUnavailable) || !errors.Is(err, cause) {
				t.Fatalf("unavailable cause did not become retryable: %v", err)
			}
		})
	}
	if err := clusterBackendError(storage.ErrBucketNotFound); !errors.Is(err, storage.ErrBucketNotFound) || errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("ordinary bucket miss became retryable: %v", err)
	}
}

func TestClusterWritersMapStageLimitToServiceUnavailable(t *testing.T) {
	backend := &S3Backend{node: &Node{stageMax: 3}}
	object := &clusterObjectWriter{backend: backend}
	part := &clusterPartWriter{b: backend}
	for name, writer := range map[string]io.Writer{"object": object, "multipart part": part} {
		t.Run(name, func(t *testing.T) {
			if _, err := writer.Write([]byte("four")); !errors.Is(err, storage.ErrBackingUnavailable) || !errors.Is(err, ErrStageFull) {
				t.Fatalf("oversized write error = %v, want retryable staging error", err)
			}
		})
	}
}

func TestS3BackendTwoNodeVersionedWriteAndRead(t *testing.T) {
	const clusterID = "s3-adapter-test"
	ca, certs := peerTestCertificates(t, clusterID, "a", "b")
	nodes := map[string]*Node{}
	peersHTTP := map[string]*httptest.Server{}
	for _, name := range []string{"a", "b"} {
		name := name
		peersHTTP[name] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nodes[name].PeerHandler().ServeHTTP(w, r)
		}))
	}
	peers := []config.ClusterPeer{
		{ID: "a", URL: "https://" + peersHTTP["a"].Listener.Addr().String()},
		{ID: "b", URL: "https://" + peersHTTP["b"].Listener.Addr().String()},
	}
	for _, name := range []string{"a", "b"} {
		cfg := config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers,
			TLSCA: ca, TLSCert: certs[name][0], TLSKey: certs[name][1],
			ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2}
		var err error
		nodes[name], err = OpenNode(cfg, filepath.Join(t.TempDir(), name), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		peersHTTP[name].TLS = nodes[name].PeerTLSConfig()
	}
	for _, server := range peersHTTP {
		server.StartTLS()
		defer server.Close()
	}
	runners := map[string]chan error{}
	cancels := map[string]context.CancelFunc{}
	for name, node := range nodes {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[name] = cancel
		runners[name] = make(chan error, 1)
		go func() { runners[name] <- node.Run(ctx) }()
	}
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
		for name, node := range nodes {
			if runners[name] == nil {
				continue
			}
			if err := <-runners[name]; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
			if err := node.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	identity, err := iam.Open(iam.Options{RootAccessKey: "ROOTTESTKEY", RootSecretKey: "root-test-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	clients := map[string]*awss3.Client{}
	backends := map[string]*S3Backend{}
	for name, node := range nodes {
		backends[name] = NewS3Backend(node)
		api := httptest.NewServer(s3.New(s3.Options{Backend: backends[name], IAM: identity,
			Verifier: auth.NewVerifier(identity, auth.Options{Region: "us-east-1"})}))
		defer api.Close()
		clients[name] = awss3.New(awss3.Options{Region: "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider("ROOTTESTKEY", "root-test-secret-key", ""),
			BaseEndpoint: aws.String(api.URL), UsePathStyle: true, HTTPClient: api.Client(), Retryer: aws.NopRetryer{}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := backends["a"].CreateBucket(ctx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if locked, err := backends["b"].GetBucket(ctx, "locked"); err != nil || !locked.ObjectLockEnabled || locked.Versioning != storage.VersioningEnabled {
		t.Fatalf("cross-node locked bucket: %+v, %v", locked, err)
	}
	if _, err := clients["a"].CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients["b"].PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{Bucket: aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients["a"].PutBucketTagging(ctx, &awss3.PutBucketTaggingInput{Bucket: aws.String("photos"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: aws.String("storage")}}}}); err != nil {
		t.Fatal(err)
	}
	tagging, err := clients["b"].GetBucketTagging(ctx, &awss3.GetBucketTaggingInput{Bucket: aws.String("photos")})
	if err != nil || tagging == nil || len(tagging.TagSet) != 1 || aws.ToString(tagging.TagSet[0].Value) != "storage" {
		t.Fatalf("cross-node bucket config: %+v, %v", tagging, err)
	}
	w, err := backends["a"].NewObjectWriter(ctx, "locked", "protected")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("protected")); err != nil {
		t.Fatal(err)
	}
	protected, err := w.Commit(ctx, storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: time.Now().Add(time.Hour)}, storage.CommitOptions{})
	if err != nil {
		t.Fatalf("retained object write: %v", err)
	}
	if _, err := backends["b"].DeleteObject(ctx, "locked", "protected", protected.VersionID); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("retained version removed: %v", err)
	}
	governedWriter, err := backends["a"].NewObjectWriter(ctx, "locked", "governed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := governedWriter.Write([]byte("governed")); err != nil {
		t.Fatal(err)
	}
	governed, err := governedWriter.Commit(ctx, storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: time.Now().Add(time.Hour)}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backends["b"].DeleteObject(ctx, "locked", "governed", governed.VersionID); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("governance version removed without approved bypass: %v", err)
	}
	if _, err := backends["b"].DeleteObject(storage.WithGovernanceBypass(ctx), "locked", "governed", governed.VersionID); err != nil {
		t.Fatalf("approved governance bypass rejected: %v", err)
	}
	retainUntil := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	lockedPut, err := clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("locked"), Key: aws.String("api-protected"), Body: strings.NewReader("locked bytes"), ObjectLockMode: types.ObjectLockModeCompliance, ObjectLockRetainUntilDate: aws.Time(retainUntil)})
	if err != nil {
		t.Fatalf("S3 object lock put: %v", err)
	}
	retention, err := clients["b"].GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{Bucket: aws.String("locked"), Key: aws.String("api-protected"), VersionId: lockedPut.VersionId})
	if err != nil || retention.Retention == nil || retention.Retention.Mode != types.ObjectLockRetentionModeCompliance {
		t.Fatalf("cross-node object retention: %+v, %v", retention, err)
	}
	if _, err := clients["b"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("locked"), Key: aws.String("api-protected"), VersionId: lockedPut.VersionId}); err == nil {
		t.Fatal("S3 delete removed compliance-retained version")
	}
	put, err := clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), Body: strings.NewReader("first")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(put.VersionId) == "" {
		t.Fatal("versioned PUT omitted version ID")
	}
	get, err := clients["b"].GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	first, err := io.ReadAll(get.Body)
	_ = get.Body.Close()
	if err != nil || string(first) != "first" || aws.ToString(get.VersionId) != aws.ToString(put.VersionId) {
		t.Fatalf("node b read %q, version %q: %v", first, aws.ToString(get.VersionId), err)
	}
	if err := nodes["b"].stages.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(stageBucket).Delete([]byte(aws.ToString(put.VersionId)))
	}); err != nil {
		t.Fatal(err)
	}
	if repaired, err := nodes["b"].RepairLocalPlacements(ctx); err != nil || repaired != 1 {
		t.Fatalf("local placement sweep repaired %d copies: %v", repaired, err)
	}
	if local, err := nodes["b"].stages.Read(ctx, aws.ToString(put.VersionId)); err != nil || string(local) != "first" {
		t.Fatalf("returning replica has %q: %v", local, err)
	}
	second, err := clients["b"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), Body: strings.NewReader("second")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(second.VersionId) == aws.ToString(put.VersionId) {
		t.Fatal("overwrite reused version ID")
	}
	get, err = clients["a"].GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), VersionId: put.VersionId})
	if err != nil {
		t.Fatal(err)
	}
	first, err = io.ReadAll(get.Body)
	_ = get.Body.Close()
	if err != nil || string(first) != "first" {
		t.Fatalf("old version read %q: %v", first, err)
	}
	if _, err := clients["b"].PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{Bucket: aws.String("photos"), Key: aws.String("k"),
		VersionId: put.VersionId, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("tier"), Value: aws.String("archive")}}}}); err != nil {
		t.Fatal(err)
	}
	tags, err := clients["a"].GetObjectTagging(ctx, &awss3.GetObjectTaggingInput{Bucket: aws.String("photos"), Key: aws.String("k"), VersionId: put.VersionId})
	if err != nil || tags == nil || len(tags.TagSet) != 1 || aws.ToString(tags.TagSet[0].Value) != "archive" {
		t.Fatalf("cross-node object metadata update: %+v, %v", tags, err)
	}
	stale := errors.New("authorized metadata changed")
	_, err = backends["a"].UpdateObjectMetaIf(ctx, "photos", "k", aws.ToString(put.VersionId), func(info storage.ObjectInfo) error {
		if info.Tags["tier"] != "" {
			return stale
		}
		return nil
	}, func(meta *storage.ObjectMeta) error {
		meta.ACL = "public-read"
		return nil
	})
	if !errors.Is(err, stale) {
		t.Fatalf("stale metadata authorization changed version: %v", err)
	}
	unchanged, err := backends["b"].GetObjectInfo(ctx, "photos", "k", aws.ToString(put.VersionId))
	if err != nil || unchanged.ACL == "public-read" {
		t.Fatalf("rejected metadata change was published: %+v, %v", unchanged, err)
	}
	listed, err := clients["a"].ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("photos")})
	if err != nil || listed == nil || len(listed.Contents) != 1 || aws.ToString(listed.Contents[0].Key) != "k" {
		t.Fatalf("latest objects: %+v, %v", listed, err)
	}
	versions, err := clients["b"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("photos")})
	if err != nil || versions == nil || len(versions.Versions) != 2 || aws.ToString(versions.Versions[0].VersionId) != aws.ToString(second.VersionId) {
		t.Fatalf("cross-node version list: %+v, %v", versions, err)
	}
	var concurrent sync.WaitGroup
	writeErrors := make(chan error, 6)
	for i := 0; i < 6; i++ {
		concurrent.Add(1)
		go func(i int) {
			defer concurrent.Done()
			name := "a"
			if i%2 == 1 {
				name = "b"
			}
			_, err := clients[name].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"),
				Body: strings.NewReader(fmt.Sprintf("concurrent-%d", i))})
			writeErrors <- err
		}(i)
	}
	concurrent.Wait()
	close(writeErrors)
	for err := range writeErrors {
		if err != nil {
			t.Fatalf("concurrent unconditional write: %v", err)
		}
	}
	versions, err = clients["a"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("photos")})
	if err != nil || versions == nil || len(versions.Versions) != 8 {
		t.Fatalf("concurrent version list: %+v, %v", versions, err)
	}
	if _, err := clients["b"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), VersionId: put.VersionId}); err != nil {
		t.Fatalf("delete oldest version: %v", err)
	}
	if _, err := clients["b"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), VersionId: versions.Versions[3].VersionId}); err != nil {
		t.Fatalf("delete middle version: %v", err)
	}
	latestID := versions.Versions[0].VersionId
	nextID := versions.Versions[1].VersionId
	if _, err := clients["a"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), VersionId: latestID}); err != nil {
		t.Fatalf("delete latest version: %v", err)
	}
	current, err := clients["b"].HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("photos"), Key: aws.String("k")})
	if err != nil || current == nil || aws.ToString(current.VersionId) != aws.ToString(nextID) {
		t.Fatalf("latest after version delete: %+v, %v", current, err)
	}
	_, err = clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"),
		Body: strings.NewReader("must not publish"), IfNoneMatch: aws.String("*")})
	var responseErr *smithyhttp.ResponseError
	if !errors.As(err, &responseErr) || responseErr.HTTPStatusCode() != http.StatusPreconditionFailed {
		t.Fatalf("conditional write: %v", err)
	}
	deleted, err := clients["a"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("k")})
	if err != nil || deleted == nil || !aws.ToBool(deleted.DeleteMarker) {
		t.Fatalf("delete marker: %+v, %v", deleted, err)
	}
	versions, err = clients["b"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("photos")})
	if err != nil || versions == nil || len(versions.DeleteMarkers) != 1 || len(versions.Versions) != 5 || aws.ToString(versions.DeleteMarkers[0].VersionId) != aws.ToString(deleted.VersionId) {
		t.Fatalf("cross-node delete marker list: %+v, %v", versions, err)
	}
	_, err = clients["a"].DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String("photos")})
	responseErr = nil
	if !errors.As(err, &responseErr) || responseErr.HTTPStatusCode() != http.StatusConflict {
		t.Fatalf("nonempty bucket deletion: %v", err)
	}
	for _, version := range versions.Versions {
		if _, err := clients["a"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), VersionId: version.VersionId}); err != nil {
			t.Fatalf("remove version %s: %v", aws.ToString(version.VersionId), err)
		}
	}
	if _, err := clients["b"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("k"), VersionId: deleted.VersionId}); err != nil {
		t.Fatalf("remove delete marker: %v", err)
	}
	if _, err := clients["b"].DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatalf("delete empty bucket: %v", err)
	}
	if _, err := clients["a"].CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatalf("recreate bucket: %v", err)
	}
	if _, err := backends["a"].GetBucketConfig(ctx, "photos", storage.ConfigTagging); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("recreated bucket retained old config: %v", err)
	}
	for _, body := range []string{"plain-one", "plain-two"} {
		if _, err := clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain"), Body: strings.NewReader(body)}); err != nil {
			t.Fatalf("unversioned PUT: %v", err)
		}
	}
	plain, reader, err := backends["b"].OpenObject(ctx, "photos", "plain", storage.NullVersionID)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || plain.VersionID != storage.NullVersionID || string(content) != "plain-two" {
		t.Fatalf("replaced null version: %+v, %q, %v", plain, content, err)
	}
	versions, err = clients["b"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("photos")})
	if err != nil || versions == nil || len(versions.Versions) != 1 || aws.ToString(versions.Versions[0].VersionId) != storage.NullVersionID {
		t.Fatalf("unversioned version list: %+v, %v", versions, err)
	}
	if _, err := clients["a"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain")}); err != nil {
		t.Fatalf("unversioned delete: %v", err)
	}
	if _, err := backends["b"].GetObjectInfo(ctx, "photos", "plain", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("unversioned delete left a version: %v", err)
	}
	if _, err := clients["a"].PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{Bucket: aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	numbered, err := clients["b"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain"), Body: strings.NewReader("numbered")})
	if err != nil || numbered == nil || aws.ToString(numbered.VersionId) == "" || aws.ToString(numbered.VersionId) == storage.NullVersionID {
		t.Fatalf("versioned write before suspension: %+v, %v", numbered, err)
	}
	if _, err := clients["b"].PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{Bucket: aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"suspended-one", "suspended-two"} {
		if _, err := clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain"), Body: strings.NewReader(body)}); err != nil {
			t.Fatalf("suspended PUT: %v", err)
		}
	}
	versions, err = clients["b"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("photos")})
	if err != nil || versions == nil || len(versions.Versions) != 2 || aws.ToString(versions.Versions[0].VersionId) != storage.NullVersionID || aws.ToString(versions.Versions[1].VersionId) != aws.ToString(numbered.VersionId) {
		t.Fatalf("suspended null replacement: %+v, %v", versions, err)
	}
	if _, err := clients["b"].PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{Bucket: aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	newer, err := clients["b"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain"), Body: strings.NewReader("newer")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients["a"].PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{Bucket: aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain"), Body: strings.NewReader("new null")}); err != nil {
		t.Fatal(err)
	}
	versions, err = clients["b"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("photos")})
	if err != nil || versions == nil || len(versions.Versions) != 3 || aws.ToString(versions.Versions[0].VersionId) != storage.NullVersionID || aws.ToString(versions.Versions[1].VersionId) != aws.ToString(newer.VersionId) || aws.ToString(versions.Versions[2].VersionId) != aws.ToString(numbered.VersionId) {
		t.Fatalf("middle null version replacement: %+v, %v", versions, err)
	}
	marker, err := clients["b"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain")})
	if err != nil || marker == nil || !aws.ToBool(marker.DeleteMarker) || aws.ToString(marker.VersionId) != storage.NullVersionID {
		t.Fatalf("suspended delete marker: %+v, %v", marker, err)
	}
	if _, err := clients["a"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain"), VersionId: aws.String(storage.NullVersionID)}); err != nil {
		t.Fatalf("delete null marker: %v", err)
	}
	revealed, err := clients["b"].HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("photos"), Key: aws.String("plain")})
	if err != nil || revealed == nil || aws.ToString(revealed.VersionId) != aws.ToString(newer.VersionId) {
		t.Fatalf("numbered version after null marker delete: %+v, %v", revealed, err)
	}
	firstIntent, err := NewObjectPutIntent("photos", "retry", []byte("first"), storage.ObjectMeta{ETag: "first"})
	if err != nil {
		t.Fatal(err)
	}
	firstNull, err := backends["a"].objects.PutVersion(ctx, firstIntent)
	if err != nil {
		t.Fatal(err)
	}
	secondIntent, err := NewObjectPutIntent("photos", "retry", []byte("second"), storage.ObjectMeta{ETag: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backends["b"].objects.PutVersion(ctx, secondIntent); err != nil {
		t.Fatal(err)
	}
	if retried, err := backends["a"].objects.PutVersion(ctx, firstIntent); err != nil || retried.ETag != firstNull.ETag {
		t.Fatalf("retry of replaced null operation: %+v, %v", retried, err)
	}
	if latest, err := backends["b"].GetObjectInfo(ctx, "photos", "retry", ""); err != nil || latest.ETag != "second" {
		t.Fatalf("retry restored old null version: %+v, %v", latest, err)
	}
	reusedID := firstIntent
	reusedID.Payload = []byte("different")
	if _, err := backends["a"].objects.PutVersion(ctx, reusedID); err == nil {
		t.Fatal("operation ID reused for different bytes")
	}
	if _, err := clients["a"].CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("replica")}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients["b"].PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{Bucket: aws.String("replica"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	importedAt := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	for _, body := range []string{"imported-one", "imported-replacement"} {
		writer, err := backends["a"].NewVersionWriter(ctx, "replica", "copy")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		stored, err := writer.Commit(ctx, storage.ObjectInfo{ObjectMeta: storage.ObjectMeta{ETag: body}, VersionID: "source-v1", LastModified: importedAt})
		if err != nil || stored.VersionID != "source-v1" || !stored.LastModified.Equal(importedAt) {
			t.Fatalf("import version: %+v, %v", stored, err)
		}
	}
	imported, reader, err := backends["b"].OpenObject(ctx, "replica", "copy", "source-v1")
	if err != nil {
		t.Fatal(err)
	}
	content, err = io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || imported.VersionID != "source-v1" || string(content) != "imported-replacement" {
		t.Fatalf("imported replacement: %+v, %q, %v", imported, content, err)
	}
	importedVersions, err := clients["b"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("replica")})
	if err != nil || importedVersions == nil || len(importedVersions.Versions) != 1 {
		t.Fatalf("replaced imported version listed twice: %+v, %v", importedVersions, err)
	}
	markerWriter, err := backends["b"].NewVersionWriter(ctx, "replica", "copy")
	if err != nil {
		t.Fatal(err)
	}
	importedMarker, err := markerWriter.Commit(ctx, storage.ObjectInfo{VersionID: "source-marker", LastModified: importedAt.Add(time.Hour), DeleteMarker: true})
	if err != nil || !importedMarker.DeleteMarker || importedMarker.VersionID != "source-marker" {
		t.Fatalf("imported delete marker: %+v, %v", importedMarker, err)
	}
	upload, err := clients["a"].CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("photos"), Key: aws.String("multipart")})
	if err != nil {
		t.Fatal(err)
	}
	part, err := clients["b"].UploadPart(ctx, &awss3.UploadPartInput{Bucket: aws.String("photos"), Key: aws.String("multipart"), UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("cluster multipart")})
	if err != nil {
		t.Fatal(err)
	}
	listedParts, err := clients["a"].ListParts(ctx, &awss3.ListPartsInput{Bucket: aws.String("photos"), Key: aws.String("multipart"), UploadId: upload.UploadId})
	if err != nil || len(listedParts.Parts) != 1 {
		t.Fatalf("cross-node multipart part listing: %+v, %v", listedParts, err)
	}
	replacedPart, err := clients["a"].UploadPart(ctx, &awss3.UploadPartInput{Bucket: aws.String("photos"), Key: aws.String("multipart"), UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("replaced multipart")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = clients["b"].CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{Bucket: aws.String("photos"), Key: aws.String("multipart"), UploadId: upload.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}}})
	if err == nil {
		t.Fatal("stale part ETag completed upload")
	}
	if _, err := backends["a"].GetMultipartUpload(ctx, "photos", "multipart", aws.ToString(upload.UploadId)); err != nil {
		t.Fatalf("failed completion removed upload: %v", err)
	}
	completed, err := clients["a"].CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{Bucket: aws.String("photos"), Key: aws.String("multipart"), UploadId: upload.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: replacedPart.ETag}}}})
	if err != nil || completed == nil || aws.ToString(completed.VersionId) == "" {
		t.Fatalf("multipart completion: %+v, %v", completed, err)
	}
	mpGet, err := clients["b"].GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("multipart")})
	if err != nil {
		t.Fatal(err)
	}
	mpBody, err := io.ReadAll(mpGet.Body)
	_ = mpGet.Body.Close()
	if err != nil || string(mpBody) != "replaced multipart" {
		t.Fatalf("cross-node multipart body: %q, %v", mpBody, err)
	}
	if _, err := backends["a"].GetMultipartUpload(ctx, "photos", "multipart", aws.ToString(upload.UploadId)); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("completed upload still exists: %v", err)
	}
	abort, err := clients["b"].CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("photos"), Key: aws.String("abort")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients["a"].AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{Bucket: aws.String("photos"), Key: aws.String("abort"), UploadId: abort.UploadId}); err != nil {
		t.Fatal(err)
	}
	if _, err := backends["b"].GetMultipartUpload(ctx, "photos", "abort", aws.ToString(abort.UploadId)); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("aborted upload still exists: %v", err)
	}
	if _, err := clients["a"].CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("pending-only")}); err != nil {
		t.Fatal(err)
	}
	pending, err := clients["b"].CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("pending-only"), Key: aws.String("unfinished")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients["a"].DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String("pending-only")}); err != nil {
		t.Fatalf("pending upload blocked empty bucket delete: %v", err)
	}
	if _, found, err := nodes["b"].Metadata().Get(MetadataJobs, uploadKey("pending-only", aws.ToString(pending.UploadId))); err != nil || found {
		t.Fatalf("bucket delete retained upload: %v, %v", found, err)
	}
	for _, bucket := range []string{"source", "target"} {
		if _, err := clients["a"].CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatal(err)
		}
		if _, err := clients["b"].PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{Bucket: aws.String(bucket),
			VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
			t.Fatal(err)
		}
	}
	preexisting, err := clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("source"), Key: aws.String("old"), Body: strings.NewReader("backfill me")})
	if err != nil {
		t.Fatal(err)
	}
	const roleARN = "arn:aws:iam::000000000000:role/cluster-replication"
	if err := identity.PutReplicationRole(ctx, iam.ReplicationRole{ARN: roleARN, Grants: []iam.ReplicationGrant{{
		SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction, iam.ReplicateDeleteAction},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients["a"].PutBucketReplication(ctx, &awss3.PutBucketReplicationInput{Bucket: aws.String("source"),
		ReplicationConfiguration: &types.ReplicationConfiguration{Role: aws.String(roleARN), Rules: []types.ReplicationRule{{
			Status: types.ReplicationRuleStatusEnabled, Prefix: aws.String(""), Destination: &types.Destination{Bucket: aws.String("arn:aws:s3:::target")},
			DeleteMarkerReplication:   &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusEnabled},
			ExistingObjectReplication: &types.ExistingObjectReplication{Status: types.ExistingObjectReplicationStatusEnabled},
		}}}}); err != nil {
		t.Fatal(err)
	}
	leaderBackend := backends["a"]
	if !leaderBackend.IsClusterLeader() {
		leaderBackend = backends["b"]
	}
	copyWorker := replication.NewWorker(leaderBackend, identity, nil)
	sourcePut, err := clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("source"), Key: aws.String("copy"), Body: strings.NewReader("replicate me")})
	if err != nil {
		t.Fatal(err)
	}
	if err := copyWorker.RunOnce(ctx); err != nil {
		t.Fatalf("replicate through cluster: %v", err)
	}
	sourceOld, err := backends["b"].GetObjectInfo(ctx, "source", "old", aws.ToString(preexisting.VersionId))
	if err != nil {
		t.Fatal(err)
	}
	backfilled, err := backends["a"].GetObjectInfo(ctx, "target", "old", aws.ToString(preexisting.VersionId))
	if err != nil || backfilled.ReplicationOrigin == "" || !backfilled.LastModified.Equal(sourceOld.LastModified) {
		t.Fatalf("cluster existing-version backfill: %+v, %v", backfilled, err)
	}
	copied, reader, err := backends["a"].OpenObject(ctx, "target", "copy", aws.ToString(sourcePut.VersionId))
	if err != nil {
		t.Fatal(err)
	}
	content, err = io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(content) != "replicate me" || copied.ReplicationOrigin == "" {
		t.Fatalf("cluster replicated body: %+v, %q, %v", copied, content, err)
	}
	sourceMarker, err := clients["b"].DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("source"), Key: aws.String("copy")})
	if err != nil {
		t.Fatal(err)
	}
	if err := copyWorker.RunOnce(ctx); err != nil {
		t.Fatalf("replicate delete marker through cluster: %v", err)
	}
	copiedMarker, err := backends["a"].GetObjectInfo(ctx, "target", "copy", aws.ToString(sourceMarker.VersionId))
	if err != nil || !copiedMarker.DeleteMarker || copiedMarker.ReplicationOrigin == "" {
		t.Fatalf("cluster replicated delete marker: %+v, %v", copiedMarker, err)
	}
	if err := replication.NewWorker(backends["a"], identity, nil).RunOnce(ctx); err != nil {
		t.Fatalf("replication retry after completed copy: %v", err)
	}
	copyVersions, err := clients["a"].ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("target")})
	if err != nil || copyVersions == nil || len(copyVersions.Versions) != 2 || len(copyVersions.DeleteMarkers) != 1 {
		t.Fatalf("duplicate replicated versions: %+v, %v", copyVersions, err)
	}
	unavailableUpload, err := clients["a"].CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("photos"), Key: aws.String("unavailable-multipart")})
	if err != nil {
		t.Fatal(err)
	}
	// Losing either node makes the default 2-of-2 policy unavailable. A
	// healthy entry node must answer with a retryable S3 error.
	backends["a"].operationTimeout = 600 * time.Millisecond
	cancels["b"]()
	if err := <-runners["b"]; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	runners["b"] = nil
	if err := nodes["b"].Close(); err != nil {
		t.Fatal(err)
	}
	peersHTTP["b"].Close()
	_, err = clients["a"].HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String("photos")})
	responseErr = nil
	if !errors.As(err, &responseErr) || responseErr.HTTPStatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("node a after quorum loss: %v", err)
	}
	_, err = clients["a"].PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("unavailable"), Body: strings.NewReader("must not publish")})
	responseErr = nil
	if !errors.As(err, &responseErr) || responseErr.HTTPStatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("write after quorum loss: %v", err)
	}
	if _, found, err := nodes["a"].Metadata().Get(MetadataVersions, latestObjectKey("photos", "unavailable")); err != nil || found {
		t.Fatalf("unavailable write published: %v, %v", found, err)
	}
	_, err = clients["a"].UploadPart(ctx, &awss3.UploadPartInput{Bucket: aws.String("photos"), Key: aws.String("unavailable-multipart"), UploadId: unavailableUpload.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("must not publish")})
	responseErr = nil
	if !errors.As(err, &responseErr) || responseErr.HTTPStatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("multipart part after quorum loss: %v", err)
	}
	if _, found, err := nodes["a"].Metadata().Get(MetadataJobs, partKey("photos", aws.ToString(unavailableUpload.UploadId), 1)); err != nil || found {
		t.Fatalf("unavailable multipart part published: %v, %v", found, err)
	}
}
