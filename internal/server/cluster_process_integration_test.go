package server

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
)

const clusterChildConfigEnv = "GOS3_TEST_CLUSTER_CHILD_CONFIG"

func TestClusterProcessChild(t *testing.T) {
	path := os.Getenv(clusterChildConfigEnv)
	if path == "" {
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func freeClusterProcessAddresses(t *testing.T, count int) []string {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	for range count {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, prior := range listeners {
				_ = prior.Close()
			}
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
	}
	addresses := make([]string, 0, count)
	for _, ln := range listeners {
		addresses = append(addresses, ln.Addr().String())
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return addresses
}

type clusterChildProcess struct {
	cmd *exec.Cmd
	log bytes.Buffer
}

func startClusterChild(t *testing.T, configPath string) *clusterChildProcess {
	t.Helper()
	p := &clusterChildProcess{cmd: exec.Command(os.Args[0], "-test.run=^TestClusterProcessChild$")}
	p.cmd.Env = append(os.Environ(), clusterChildConfigEnv+"="+configPath)
	p.cmd.Stdout = &p.log
	p.cmd.Stderr = &p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *clusterChildProcess) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	if err := p.cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Errorf("waiting for cluster child: %v; log:\n%s", err, p.log.String())
		}
	}
	p.cmd = nil
}

func clusterProcessRequest(method, target string, body []byte) (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil
	}
	if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", "", time.Now()); err != nil {
		return 0, nil
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil
	}
	return resp.StatusCode, data
}

func TestClusterTwoIndependentProcessesRestartAndFailClosed(t *testing.T) {
	if os.Getenv(clusterChildConfigEnv) != "" {
		return
	}
	const clusterID = "process-integration"
	ca, certs := clusterServerCerts(t, clusterID, "a", "b")
	addresses := freeClusterProcessAddresses(t, 4)
	peerAddresses := map[string]string{"a": addresses[0], "b": addresses[1]}
	publicAddresses := map[string]string{"a": addresses[2], "b": addresses[3]}
	peers := []config.ClusterPeer{{ID: "a", URL: "https://" + peerAddresses["a"]}, {ID: "b", URL: "https://" + peerAddresses["b"]}}
	paths := map[string]string{}
	for _, name := range []string{"a", "b"} {
		cfg := testConfig()
		cfg.Server.Address = publicAddresses[name]
		cfg.Storage.Backend = config.BackendFS
		cfg.Storage.Path = t.TempDir()
		cfg.Cluster = config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca,
			TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 2, MinMemoryAcks: 2, MinDurableAcks: 2,
			StageMemoryBytes: config.MinClusterStageMemoryBytes}
		cfg.Normalize()
		encoded, err := cfg.YAML()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), name+".yaml")
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	a, b := startClusterChild(t, paths["a"]), startClusterChild(t, paths["b"])
	t.Cleanup(func() {
		a.stop(t)
		b.stop(t)
		if t.Failed() {
			t.Logf("cluster process a:\n%s", a.log.String())
			t.Logf("cluster process b:\n%s", b.log.String())
		}
	})
	baseA, baseB := "http://"+publicAddresses["a"], "http://"+publicAddresses["b"]
	waitFor(t, 20*time.Second, "two process quorum", func() bool {
		statusA, _ := clusterProcessRequest("GET", baseA+"/", nil)
		statusB, _ := clusterProcessRequest("GET", baseB+"/", nil)
		return statusA == 200 && statusB == 200
	})
	if status, body := clusterProcessRequest("PUT", baseA+"/process-bucket", nil); status != 200 {
		t.Fatalf("bucket creation: %d %s", status, body)
	}
	if status, body := clusterProcessRequest("PUT", baseB+"/process-bucket/key", []byte("first")); status != 200 {
		t.Fatalf("write through b: %d %s", status, body)
	}
	for _, base := range []string{baseA, baseB} {
		if status, body := clusterProcessRequest("GET", base+"/process-bucket/key", nil); status != 200 || string(body) != "first" {
			t.Fatalf("read through %s: %d %q", base, status, body)
		}
	}
	versioning := []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`)
	if r := do(t, "PUT", baseB+"/process-bucket?versioning", versioning, &rootCreds, nil); r.status != 200 {
		t.Fatalf("enable versioning through b: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", baseA+"/process-bucket?versioning", nil, &rootCreds, nil); r.status != 200 || !bytes.Contains(r.body, []byte("Enabled")) {
		t.Fatalf("versioning through a: %d %s", r.status, r.body)
	}
	firstVersion := do(t, "PUT", baseA+"/process-bucket/versioned", []byte("old"), &rootCreds, nil)
	if firstVersion.status != 200 || firstVersion.header.Get("X-Amz-Version-Id") == "" {
		t.Fatalf("first version through a: %d %s", firstVersion.status, firstVersion.body)
	}
	conditional := do(t, "PUT", baseB+"/process-bucket/versioned", []byte("new"), &rootCreds, map[string]string{"If-Match": firstVersion.header.Get("ETag")})
	if conditional.status != 200 {
		t.Fatalf("conditional version through b: %d %s", conditional.status, conditional.body)
	}
	if r := do(t, "PUT", baseA+"/process-bucket/versioned", []byte("rejected"), &rootCreds, map[string]string{"If-Match": `"incorrect"`}); r.status != http.StatusPreconditionFailed {
		t.Fatalf("stale conditional write: %d %s", r.status, r.body)
	}
	if r := do(t, "DELETE", baseB+"/process-bucket/versioned", nil, &rootCreds, nil); r.status != http.StatusNoContent || r.header.Get("X-Amz-Delete-Marker") != "true" {
		t.Fatalf("delete marker through b: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", baseA+"/process-bucket/versioned", nil, &rootCreds, nil); r.status != http.StatusNotFound {
		t.Fatalf("latest version after delete marker: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", baseB+"/process-bucket/versioned?versionId="+url.QueryEscape(firstVersion.header.Get("X-Amz-Version-Id")), nil, &rootCreds, nil); r.status != 200 || string(r.body) != "old" {
		t.Fatalf("historical version through b: %d %s", r.status, r.body)
	}
	createUpload := do(t, "POST", baseA+"/process-bucket/multipart?uploads", nil, &rootCreds, nil)
	var upload struct {
		UploadID string `xml:"UploadId"`
	}
	if createUpload.status != 200 || xml.Unmarshal(createUpload.body, &upload) != nil || upload.UploadID == "" {
		t.Fatalf("create multipart through a: %d %s", createUpload.status, createUpload.body)
	}
	uploadQuery := "uploadId=" + url.QueryEscape(upload.UploadID)
	part := do(t, "PUT", baseB+"/process-bucket/multipart?partNumber=1&"+uploadQuery, []byte("part-body"), &rootCreds, nil)
	if part.status != 200 || part.header.Get("ETag") == "" {
		t.Fatalf("upload part through b: %d %s", part.status, part.body)
	}
	completeBody := []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + part.header.Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`)
	if r := do(t, "POST", baseA+"/process-bucket/multipart?"+uploadQuery, completeBody, &rootCreds, nil); r.status != 200 {
		t.Fatalf("complete multipart through a: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", baseB+"/process-bucket/multipart", nil, &rootCreds, nil); r.status != 200 || string(r.body) != "part-body" {
		t.Fatalf("multipart result through b: %d %s", r.status, r.body)
	}
	userDoc := []byte(`{"name":"process-user","policies":["readwrite"]}`)
	if r := do(t, "POST", baseB+"/_gos3/admin/v1/users", userDoc, &rootCreds, map[string]string{"Content-Type": "application/json"}); r.status != http.StatusCreated {
		t.Fatalf("IAM create through b: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", baseA+"/_gos3/admin/v1/users/process-user", nil, &rootCreds, nil); r.status != 200 || !bytes.Contains(r.body, []byte("process-user")) {
		t.Fatalf("IAM read through a: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", baseA+"/locked-bucket", nil, &rootCreds, map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"}); r.status != 200 {
		t.Fatalf("object-lock bucket through a: %d %s", r.status, r.body)
	}
	locked := do(t, "PUT", baseB+"/locked-bucket/protected", []byte("keep"), &rootCreds, map[string]string{
		"X-Amz-Object-Lock-Mode":              "COMPLIANCE",
		"X-Amz-Object-Lock-Retain-Until-Date": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	if locked.status != 200 || locked.header.Get("X-Amz-Version-Id") == "" {
		t.Fatalf("locked write through b: %d %s", locked.status, locked.body)
	}
	if r := do(t, "DELETE", baseA+"/locked-bucket/protected?versionId="+url.QueryEscape(locked.header.Get("X-Amz-Version-Id")), nil, &rootCreds, nil); r.status != http.StatusForbidden {
		t.Fatalf("compliance-retained delete through a: %d %s", r.status, r.body)
	}
	if status, body := clusterProcessRequest("PUT", baseA+"/process-bucket/too-large", bytes.Repeat([]byte("x"), int(config.MinClusterStageMemoryBytes)+1)); status != http.StatusServiceUnavailable {
		t.Fatalf("staging limit returned %d %s, want 503", status, body)
	}
	if status, _ := clusterProcessRequest("GET", baseB+"/process-bucket/too-large", nil); status != http.StatusNotFound {
		t.Fatalf("oversized write published a version: %d", status)
	}
	b.stop(t)
	if status, body := clusterProcessRequest("PUT", baseA+"/process-bucket/blocked", []byte("denied")); status != http.StatusServiceUnavailable {
		t.Fatalf("write without second voter: %d %s", status, body)
	}
	b = startClusterChild(t, paths["b"])
	waitFor(t, 20*time.Second, "restarted voter read", func() bool {
		status, body := clusterProcessRequest("GET", baseB+"/process-bucket/key", nil)
		return status == 200 && string(body) == "first"
	})
	if r := do(t, "GET", baseB+"/process-bucket/versioned?versionId="+url.QueryEscape(firstVersion.header.Get("X-Amz-Version-Id")), nil, &rootCreds, nil); r.status != 200 || string(r.body) != "old" {
		t.Fatalf("historical version after restart: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", baseB+"/process-bucket/multipart", nil, &rootCreds, nil); r.status != 200 || string(r.body) != "part-body" {
		t.Fatalf("multipart object after restart: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", baseB+"/_gos3/admin/v1/users/process-user", nil, &rootCreds, nil); r.status != 200 || !bytes.Contains(r.body, []byte("process-user")) {
		t.Fatalf("IAM user after restart: %d %s", r.status, r.body)
	}
	if r := do(t, "DELETE", baseB+"/locked-bucket/protected?versionId="+url.QueryEscape(locked.header.Get("X-Amz-Version-Id")), nil, &rootCreds, nil); r.status != http.StatusForbidden {
		t.Fatalf("object lock after restart: %d %s", r.status, r.body)
	}
	waitFor(t, 20*time.Second, "write after restart", func() bool {
		status, _ := clusterProcessRequest("PUT", baseA+"/process-bucket/recovered", []byte("second"))
		return status == 200
	})
	if status, body := clusterProcessRequest("GET", baseB+"/process-bucket/recovered", nil); status != 200 || string(body) != "second" {
		t.Fatalf("recovered read through b: %d %q", status, body)
	}
}

func TestClusterThreeIndependentProcessesContinueWithTwoVoters(t *testing.T) {
	if os.Getenv(clusterChildConfigEnv) != "" {
		return
	}
	const clusterID = "process-three-voters"
	ca, certs := clusterServerCerts(t, clusterID, "a", "b", "c")
	addresses := freeClusterProcessAddresses(t, 6)
	var peers []config.ClusterPeer
	publicAddresses := map[string]string{}
	for i, name := range []string{"a", "b", "c"} {
		peers = append(peers, config.ClusterPeer{ID: name, URL: "https://" + addresses[i]})
		publicAddresses[name] = addresses[i+3]
	}
	paths := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		cfg := testConfig()
		cfg.Server.Address = publicAddresses[name]
		cfg.Storage.Backend = config.BackendFS
		cfg.Storage.Path = t.TempDir()
		cfg.Cluster = config.ClusterConfig{Enabled: true, ID: clusterID, NodeID: name, Peers: peers, TLSCA: ca,
			TLSCert: certs[name][0], TLSKey: certs[name][1], ReplicaCount: 3, MinMemoryAcks: 2, MinDurableAcks: 2}
		cfg.Normalize()
		encoded, err := cfg.YAML()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), name+".yaml")
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	processes := map[string]*clusterChildProcess{}
	for _, name := range []string{"a", "b", "c"} {
		processes[name] = startClusterChild(t, paths[name])
	}
	t.Cleanup(func() {
		for _, name := range []string{"a", "b", "c"} {
			p := processes[name]
			p.stop(t)
			if t.Failed() {
				t.Logf("cluster process %s:\n%s", name, p.log.String())
			}
		}
	})
	base := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		base[name] = "http://" + publicAddresses[name]
	}
	waitFor(t, 25*time.Second, "three process quorum", func() bool {
		for _, name := range []string{"a", "b", "c"} {
			status, _ := clusterProcessRequest("GET", base[name]+"/", nil)
			if status != 200 {
				return false
			}
		}
		return true
	})
	if status, body := clusterProcessRequest("PUT", base["a"]+"/three-bucket", nil); status != 200 {
		t.Fatalf("bucket creation: %d %s", status, body)
	}
	if status, body := clusterProcessRequest("PUT", base["c"]+"/three-bucket/key", []byte("first")); status != 200 {
		t.Fatalf("write through c: %d %s", status, body)
	}
	for _, name := range []string{"a", "b", "c"} {
		if status, body := clusterProcessRequest("GET", base[name]+"/three-bucket/key", nil); status != 200 || string(body) != "first" {
			t.Fatalf("read through %s: %d %q", name, status, body)
		}
	}
	status, body := clusterProcessRequest("GET", base["a"]+"/_gos3/admin/v1/cluster", nil)
	if status != 200 {
		t.Fatalf("cluster status before failover: %d %s", status, body)
	}
	var leadership struct {
		LeaderID string `json:"leaderId"`
	}
	if err := json.Unmarshal(body, &leadership); err != nil {
		t.Fatal(err)
	}
	if processes[leadership.LeaderID] == nil {
		t.Fatalf("unknown elected leader %q", leadership.LeaderID)
	}
	processes[leadership.LeaderID].stop(t)
	var survivors []string
	for _, name := range []string{"a", "b", "c"} {
		if name != leadership.LeaderID {
			survivors = append(survivors, name)
		}
	}
	waitFor(t, 25*time.Second, "two surviving voters admit a write", func() bool {
		status, _ := clusterProcessRequest("PUT", base[survivors[0]]+"/three-bucket/after-failure", []byte("second"))
		return status == 200
	})
	status, body = clusterProcessRequest("GET", base[survivors[0]]+"/_gos3/admin/v1/cluster", nil)
	if status != 200 {
		t.Fatalf("cluster status after leader loss: %d %s", status, body)
	}
	var recovered struct {
		LeaderID  string `json:"leaderId"`
		Available bool   `json:"available"`
		LastWrite struct {
			MemoryAcks         int  `json:"memoryAcks"`
			DurableAcks        int  `json:"durableAcks"`
			ConsensusCommitted bool `json:"consensusCommitted"`
		} `json:"lastWrite"`
	}
	if err := json.Unmarshal(body, &recovered); err != nil {
		t.Fatal(err)
	}
	if !recovered.Available || recovered.LeaderID == leadership.LeaderID || recovered.LeaderID == "" ||
		recovered.LastWrite.MemoryAcks < 2 || recovered.LastWrite.DurableAcks < 2 || !recovered.LastWrite.ConsensusCommitted {
		t.Fatalf("write acknowledged without two surviving copies: %+v", recovered)
	}
	for _, name := range survivors {
		if status, body := clusterProcessRequest("GET", base[name]+"/three-bucket/after-failure", nil); status != 200 || string(body) != "second" {
			t.Fatalf("survivor %s read: %d %q", name, status, body)
		}
		if status, body := clusterProcessRequest("GET", base[name]+"/three-bucket/key", nil); status != 200 || string(body) != "first" {
			t.Fatalf("survivor %s retained prior write: %d %q", name, status, body)
		}
	}
}
