package s3client_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

func TestGetObjectValidatesRemoteRange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		cr, data string
		opts     s3client.GetOptions
		valid    bool
	}{
		{"ignored", 200, "", "abcdef", s3client.GetOptions{Offset: 2, Length: 2}, false},
		{"missing header", 206, "", "cd", s3client.GetOptions{Offset: 2, Length: 2}, false},
		{"wrong offset", 206, "bytes 0-1/6", "ab", s3client.GetOptions{Offset: 2, Length: 2}, false},
		{"short span", 206, "bytes 2-2/6", "c", s3client.GetOptions{Offset: 2, Length: 2}, false},
		{"bad total", 206, "bytes 2-3/3", "cd", s3client.GetOptions{Offset: 2, Length: 2}, false},
		{"wrong length", 206, "bytes 2-3/6", "cde", s3client.GetOptions{Offset: 2, Length: 2}, false},
		{"unsolicited partial", 206, "bytes 0-1/6", "ab", s3client.GetOptions{}, false},
		{"valid", 206, "bytes 2-3/6", "cd", s3client.GetOptions{Offset: 2, Length: 2}, true},
		{"to end", 206, "bytes 2-5/6", "cdef", s3client.GetOptions{Offset: 2}, true},
		{"clipped end", 206, "bytes 2-5/6", "cdef", s3client.GetOptions{Offset: 2, Length: 20}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(tc.data)))
				if tc.cr != "" {
					w.Header().Set("Content-Range", tc.cr)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.data)
			}))
			defer srv.Close()
			c, err := s3client.New(s3client.Config{Endpoint: srv.URL, AccessKey: "a", SecretKey: "b", PathStyle: true})
			if err != nil {
				t.Fatal(err)
			}
			r, _, err := c.GetObject(t.Context(), "bucket", "key", tc.opts)
			if r != nil {
				defer r.Close()
			}
			if tc.valid && err != nil {
				t.Fatalf("valid range refused: %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}
