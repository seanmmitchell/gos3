package s3

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// archiveBackend is the memory backend accepting restic as well (its
// versions are stored like AES256 ones): it records the providers the S3
// layer prepares and the encryption of every new version and completed
// upload.
type archiveBackend struct {
	*memory.Backend
	mu       sync.Mutex
	prepared []string // "bucket:mode"
	writes   []string // the encryption of each write ("" = none)
}

func (b *archiveBackend) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	b.mu.Lock()
	b.prepared = append(b.prepared, bucket+":"+mode)
	b.mu.Unlock()
	if mode != storage.EncryptionAES256 && mode != storage.EncryptionRestic {
		return errors.ErrUnsupported
	}
	return nil
}

func (b *archiveBackend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	return b.PrepareEncryption(ctx, bucket, cfg.Provider)
}

// record notes a write's options and returns those the memory backend
// stores.
func (b *archiveBackend) record(opts storage.WriteOptions) storage.WriteOptions {
	b.mu.Lock()
	b.writes = append(b.writes, opts.Encryption)
	b.mu.Unlock()
	if opts.Encryption == storage.EncryptionRestic {
		opts.Encryption = storage.EncryptionAES256
	}
	return opts
}

func (b *archiveBackend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	b.record(storage.WriteOptions{})
	return b.Backend.NewObjectWriter(ctx, bucket, key)
}

func (b *archiveBackend) NewObjectWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.ObjectWriter, error) {
	return b.Backend.NewObjectWriterWith(ctx, bucket, key, b.record(opts))
}

func (b *archiveBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	b.record(storage.WriteOptions{})
	return b.Backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, opts)
}

func (b *archiveBackend) CompleteMultipartUploadWith(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, opts storage.CommitOptions, wopts storage.WriteOptions) (storage.ObjectInfo, error) {
	return b.Backend.CompleteMultipartUploadWith(ctx, bucket, key, uploadID, partNumbers, meta, opts, b.record(wopts))
}

// last returns the encryption of the last write and whether restic was
// prepared for bucket since the previous call, and forgets both.
func (b *archiveBackend) last(bucket string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	enc := "<none>"
	if n := len(b.writes); n > 0 {
		enc = b.writes[n-1]
	}
	prepared := slices.Contains(b.prepared, bucket+":"+storage.EncryptionRestic)
	b.writes, b.prepared = nil, nil
	return enc, prepared
}

// newArchiveEnv starts a test server over an archiveBackend.
func newArchiveEnv(t *testing.T) (*testEnv, *archiveBackend) {
	var ab *archiveBackend
	e := newTestEnv(t, withOptions(func(o *Options) {
		ab = &archiveBackend{Backend: o.Backend.(*memory.Backend)}
		o.Backend = ab
	}))
	return e, ab
}

// The restic archive toggle (scope "all") archives every write, also those
// without SSE, which then report no SSE; scope "sse" (or none) archives
// only SSE writes.
func TestEncryptionScopeAllArchivesEveryWrite(t *testing.T) {
	e, ab := newArchiveEnv(t)
	e.createBucket("all")
	e.createBucket("sse")
	e.createBucket("plain")
	e.putBucketConfig("all", storage.ConfigEncryptionProvider, `{"provider":"restic","scope":"all"}`)
	e.putBucketConfig("sse", storage.ConfigEncryptionProvider, `{"provider":"restic","scope":"sse"}`)
	e.putBucketConfig("plain", storage.ConfigEncryptionProvider, `{"provider":"restic"}`)

	r := e.signed(http.MethodPut, "/all/k", []byte("archived"), nil)
	objExpect(t, r, 200, "")
	if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "" {
		t.Fatalf("PutObject reports SSE %q", got)
	}
	if enc, prepared := ab.last("all"); enc != storage.EncryptionRestic || !prepared {
		t.Fatalf("scope all: write encryption %q, prepared %v", enc, prepared)
	}
	if info := objGetInfo(t, e, "all", "k", ""); info.SSE != "" {
		t.Fatalf("stored SSE %q", info.SSE)
	}
	if got := e.signed(http.MethodHead, "/all/k", nil, nil).Header.Get("X-Amz-Server-Side-Encryption"); got != "" {
		t.Fatalf("HeadObject reports SSE %q", got)
	}
	// SSE writes use restic too, and report it.
	r = e.signed(http.MethodPut, "/all/s", []byte("x"), objHdr("X-Amz-Server-Side-Encryption", "AES256"))
	objExpect(t, r, 200, "")
	if enc, _ := ab.last("all"); enc != storage.EncryptionRestic || r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Fatalf("SSE write in scope all: %q, header %q", enc, r.Header.Get("X-Amz-Server-Side-Encryption"))
	}
	// Restic's own writes are refused like SSE restic writes.
	objExpect(t, e.signed(http.MethodPut, "/all/config", []byte("repository"), http.Header{"User-Agent": {storage.ResticUserAgent}}), 400, "InvalidRequest")
	ab.last("all")

	// A multipart upload without SSE is archived at completion.
	r = e.signed(http.MethodPost, "/all/mp?uploads", nil, nil)
	objExpect(t, r, 200, "")
	if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "" {
		t.Fatalf("CreateMultipartUpload reports SSE %q", got)
	}
	uploadID := between(string(r.Data), "<UploadId>", "</UploadId>")
	r = e.signed(http.MethodPut, mpPath("all", "mp", mpQuery("partNumber", "1", "uploadId", uploadID)), []byte("part"), nil)
	objExpect(t, r, 200, "")
	ab.last("all")
	r = e.signed(http.MethodPost, mpPath("all", "mp", mpQuery("uploadId", uploadID)), mpCompleteBody("1", r.Header.Get("ETag")), nil)
	objExpect(t, r, 200, "")
	if strings.Contains(string(r.Data), "<Error>") {
		t.Fatalf("completion failed: %s", r.Data)
	}
	if enc, prepared := ab.last("all"); enc != storage.EncryptionRestic || !prepared || r.Header.Get("X-Amz-Server-Side-Encryption") != "" {
		t.Fatalf("completion: write encryption %q, prepared %v, SSE header %q", enc, prepared, r.Header.Get("X-Amz-Server-Side-Encryption"))
	}

	// Scope sse, or none: plain writes stay plain, SSE writes use restic.
	for _, bucket := range []string{"sse", "plain"} {
		objExpect(t, e.signed(http.MethodPut, "/"+bucket+"/k", []byte("plain"), nil), 200, "")
		if enc, prepared := ab.last(bucket); enc != "" || prepared {
			t.Fatalf("%s: plain write encryption %q, prepared %v", bucket, enc, prepared)
		}
		// Restic's own plain writes are not refused there.
		objExpect(t, e.signed(http.MethodPut, "/"+bucket+"/config", []byte("repository"), http.Header{"User-Agent": {storage.ResticUserAgent}}), 200, "")
		objExpect(t, e.signed(http.MethodPut, "/"+bucket+"/s", []byte("x"), objHdr("X-Amz-Server-Side-Encryption", "AES256")), 200, "")
		if enc, prepared := ab.last(bucket); enc != storage.EncryptionRestic || !prepared {
			t.Fatalf("%s: SSE write encryption %q, prepared %v", bucket, enc, prepared)
		}
	}

	// Turning the toggle off applies to the next write.
	e.putBucketConfig("all", storage.ConfigEncryptionProvider, `{"provider":"restic","scope":"sse"}`)
	objExpect(t, e.signed(http.MethodPut, "/all/after", []byte("plain"), nil), 200, "")
	if enc, _ := ab.last("all"); enc != "" {
		t.Fatalf("after the toggle: write encryption %q", enc)
	}
}

// A backend that cannot archive fails every write of a bucket whose
// settings say to archive them (never storing them unarchived), and an
// invalid settings document fails writes rather than guessing.
func TestEncryptionScopeAllFailsClosed(t *testing.T) {
	e := newTestEnv(t) // the memory backend: no restic
	e.createBucket("all")
	e.putBucketConfig("all", storage.ConfigEncryptionProvider, `{"provider":"restic","scope":"all"}`)
	objExpect(t, e.signed(http.MethodPut, "/all/k", []byte("x"), nil), 501, "NotImplemented")
	if _, err := e.backend.GetObjectInfo(context.Background(), "all", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("refused write stored: %v", err)
	}

	e.createBucket("bad")
	e.putBucketConfig("bad", storage.ConfigEncryptionProvider, `{"provider":"AES256","scope":"all"}`)
	r := e.signed(http.MethodPut, "/bad/k", []byte("x"), nil)
	if r.StatusCode < 500 {
		t.Fatalf("write with invalid settings: %d %s", r.StatusCode, r.Data)
	}
}

// between returns the text of s between the first open and the next close.
func between(s, open, close string) string {
	_, rest, _ := strings.Cut(s, open)
	v, _, _ := strings.Cut(rest, close)
	return v
}
