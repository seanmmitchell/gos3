package s3

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// mpReplacePartBackend replaces part 1 of the upload with other data just
// before CompleteMultipartUpload reaches the backend, i.e. after the S3
// layer has validated the part ETags: a concurrent UploadPart racing the
// completion.
type mpReplacePartBackend struct {
	storage.Backend
	data []byte
	once sync.Once
	err  error
}

func (b *mpReplacePartBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	b.once.Do(func() {
		w, err := b.Backend.NewPartWriter(ctx, bucket, key, uploadID, 1)
		if err != nil {
			b.err = err
			return
		}
		if _, err := w.Write(b.data); err != nil {
			b.err = err
			_ = w.Abort()
			return
		}
		sum := md5.Sum(b.data)
		_, b.err = w.Commit(ctx, storage.PartMeta{ETag: hex.EncodeToString(sum[:])})
	})
	if b.err != nil {
		return storage.ObjectInfo{}, b.err
	}
	return b.Backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, opts)
}

// TestCompleteMultipartPartReplacedConcurrently: a part replaced between
// the S3 layer's ETag validation and the backend commit gives InvalidPart,
// never an object whose data does not match its ETag (regression).
func TestCompleteMultipartPartReplacedConcurrently(t *testing.T) {
	repl := &mpReplacePartBackend{data: []byte("replaced")}
	e := newTestEnv(t, withOptions(func(o *Options) {
		repl.Backend = o.Backend
		o.Backend = repl
	}))
	e.createBucket("mp")
	id := mpRawCreate(t, e, "mp", "k", nil)
	pr := e.signed(http.MethodPut, mpPath("mp", "k", mpQuery("uploadId", id, "partNumber", "1")), []byte("original"), nil)
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("UploadPart: %d %s", pr.StatusCode, pr.Data)
	}
	resp := e.signed(http.MethodPost, mpPath("mp", "k", mpQuery("uploadId", id)), mpCompleteBody("1", pr.Header.Get("ETag")), nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(resp.Data), "<Code>InvalidPart</Code>") {
		t.Fatalf("CompleteMultipartUpload after a concurrent part replacement: %d %s", resp.StatusCode, resp.Data)
	}
	if r := e.signed(http.MethodHead, "/mp/k", nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("object published despite InvalidPart: HEAD %d", r.StatusCode)
	}
	// The client can list the parts again and complete with the new ETag.
	sum := md5.Sum(repl.data)
	resp = e.signed(http.MethodPost, mpPath("mp", "k", mpQuery("uploadId", id)), mpCompleteBody("1", `"`+hex.EncodeToString(sum[:])+`"`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry with the current ETag: %d %s", resp.StatusCode, resp.Data)
	}
	if r := e.signed(http.MethodGet, "/mp/k", nil, nil); r.StatusCode != http.StatusOK || string(r.Data) != "replaced" || r.Header.Get("ETag") != mpExpectedETag(repl.data) {
		t.Fatalf("GET: %d %q ETag %s", r.StatusCode, r.Data, r.Header.Get("ETag"))
	}
}
