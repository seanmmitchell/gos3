package e2e

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// TestBackingsReplicationAndMove runs a server with three fs backings:
// it replicates a versioned bucket active-active, moves its primary to
// another backing while a client keeps writing, and checks that version
// IDs, data and the placement survive the move and a restart.
func TestBackingsReplicationAndMove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := baseConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = filepath.Join(dir, "data")
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "disk2", Backend: config.BackendFS, Path: filepath.Join(dir, "disk2")},
		{Name: "disk3", Backend: config.BackendFS, Path: filepath.Join(dir, "disk3")},
	}
	cfg.Storage.Replication.HealthInterval = time.Second
	e := startPrivate(t, cfg)
	c := e.root()
	b := bucketName(t)
	_, err := c.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(b)})
	must(t, err, "CreateBucket")
	e.setVersioning(t, b, "Enabled")
	var ids []string
	for i := range 5 {
		out := e.put(t, b, "doc", []byte(fmt.Sprintf("version %d", i)))
		ids = append(ids, aws.ToString(out.VersionId))
	}

	placement := func() multi.PlacementStatus {
		var st multi.PlacementStatus
		if r := e.admin(t, rootCreds, "GET", "/buckets/"+b+"/placement", nil, &st); r.status != 200 {
			t.Fatalf("GET placement: %d %s", r.status, r.body)
		}
		return st
	}
	waitInSync := func(what string, ok func(multi.PlacementStatus) bool) multi.PlacementStatus {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			st := placement()
			done := st.Move == nil && ok(st)
			for _, m := range st.Members {
				done = done && m.State == multi.StateInSync
			}
			if done {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, st)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Active-active replica on disk2.
	if r := e.admin(t, rootCreds, "PUT", "/buckets/"+b+"/placement",
		map[string]any{"mode": "active-active", "replicas": []string{"disk2"}}, nil); r.status != 200 {
		t.Fatalf("PUT placement: %d %s", r.status, r.body)
	}
	waitInSync("the replica", func(st multi.PlacementStatus) bool { return len(st.Members) == 2 })

	// Move the primary to disk3 while a client keeps writing.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var written []string
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("live/%04d", i)
			_, err := c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String(key), Body: bytes.NewReader([]byte(key))})
			if err != nil {
				t.Errorf("PutObject during the move: %v", err)
				return
			}
			written = append(written, key)
		}
	}()
	if r := e.admin(t, rootCreds, "POST", "/buckets/"+b+"/placement/move", map[string]any{"to": "disk3"}, nil); r.status != 202 {
		t.Fatalf("POST move: %d %s", r.status, r.body)
	}
	st := waitInSync("the move", func(st multi.PlacementStatus) bool { return st.Primary == "disk3" })
	close(stop)
	wg.Wait()
	if len(st.Members) != 2 || st.Members[1].Backing != "disk2" {
		t.Fatalf("placement after the move: %+v", st)
	}

	check := func(c *s3.Client) {
		t.Helper()
		out, err := c.ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{Bucket: aws.String(b), Prefix: aws.String("doc")})
		must(t, err, "ListObjectVersions")
		var got []string
		for _, v := range out.Versions {
			got = append(got, aws.ToString(v.VersionId))
		}
		slices.Reverse(got)
		if !slices.Equal(got, ids) {
			t.Fatalf("version IDs after the move = %v, want %v", got, ids)
		}
		for i, id := range ids {
			o, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("doc"), VersionId: aws.String(id)})
			must(t, err, "GetObject")
			if data := readAll(t, o.Body); string(data) != fmt.Sprintf("version %d", i) {
				t.Fatalf("version %s = %q", id, data)
			}
		}
		for _, k := range written {
			if got := e.get(t, b, k); string(got) != k {
				t.Fatalf("%s = %q", k, got)
			}
		}
	}
	check(c)
	t.Logf("%d objects written during the move", len(written))

	// Restart: placements persist.
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
	e = startPrivate(t, cfg)
	c = e.root()
	st = waitInSync("the replica after a restart", func(st multi.PlacementStatus) bool { return st.Primary == "disk3" })
	if st.Mode != multi.ModeActiveActive {
		t.Fatalf("placement after restart: %+v", st)
	}
	check(c)
	var bl struct {
		Backings []multi.BackingStatus `json:"backings"`
	}
	if r := e.admin(t, rootCreds, "GET", "/backings", nil, &bl); r.status != 200 || len(bl.Backings) != 3 {
		t.Fatalf("GET /backings: %d %s", r.status, r.body)
	}
	_, err = c.HeadBucket(ctx(t), &s3.HeadBucketInput{Bucket: aws.String(b)})
	must(t, err, "HeadBucket")
}
