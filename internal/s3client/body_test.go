package s3client_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// watchedReaderAt fails the test when it is read after done is set.
type watchedReaderAt struct {
	t    *testing.T
	data []byte
	done atomic.Bool
}

func (w *watchedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if w.done.Load() {
		w.t.Error("request body read after the call returned")
	}
	return bytes.NewReader(w.data).ReadAt(p, off)
}

// TestBodyNotReadAfterReturn checks that a call does not return while the
// transport may still read the request body: a server answering before
// reading the body (as S3 does for NoSuchUpload) leaves net/http's writer
// sending in the background, and callers free the body's source (a spool
// file) as soon as the call returns.
func TestBodyNotReadAfterReturn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchUpload</Code><Message>gone</Message></Error>`))
	}))
	defer srv.Close()
	c, err := s3client.New(s3client.Config{Endpoint: srv.URL, AccessKey: "a", SecretKey: "s", PathStyle: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		body := &watchedReaderAt{t: t, data: bytes.Repeat([]byte("x"), 2<<20)}
		_, err := c.UploadPart(t.Context(), "bkt", "k", "upload", 1, body, int64(len(body.data)))
		body.done.Store(true)
		if !s3client.IsCode(err, "NoSuchUpload") {
			t.Fatalf("UploadPart: %v", err)
		}
	}
}
