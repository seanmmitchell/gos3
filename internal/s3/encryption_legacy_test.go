package s3

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// legacyKMSDefault is a bucket default that builds before real encryption
// accepted and stored.
const legacyKMSDefault = `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault>` +
	`<SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>arn:aws:kms:us-east-1:000000000000:key/abc</KMSMasterKeyID>` +
	`</ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`

// A stored legacy KMS default must not stop writes after an upgrade: new
// objects get SSE-S3 instead, and the stored document stays visible so an
// administrator can replace it.
func TestLegacyKMSDefaultAppliesSSES3(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("legacy")
	ctx := context.Background()
	if err := e.backend.PutBucketConfig(ctx, "legacy", storage.ConfigEncryption, []byte(legacyKMSDefault)); err != nil {
		t.Fatal(err)
	}
	r := e.signed(http.MethodPut, "/legacy/obj", []byte("data"), nil)
	objExpect(t, r, 200, "")
	if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "AES256" {
		t.Fatalf("PutObject SSE header %q", got)
	}
	if got := r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"); got != "" {
		t.Fatalf("PutObject reported KMS key %q", got)
	}
	info, err := e.backend.GetObjectInfo(ctx, "legacy", "obj", "")
	if err != nil || info.SSE != "AES256" || info.Encryption == "" || info.SSEKMSKeyID != "" {
		t.Fatalf("stored %+v %v", info.ObjectMeta, err)
	}
	r = e.signed(http.MethodHead, "/legacy/obj", nil, nil)
	if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "AES256" {
		t.Fatalf("HEAD SSE header %q", got)
	}
	r = e.signed(http.MethodPost, "/legacy/mp?uploads", nil, nil)
	objExpect(t, r, 200, "")
	if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "AES256" {
		t.Fatalf("CreateMultipartUpload SSE header %q", got)
	}
	r = e.signed(http.MethodGet, "/legacy?encryption", nil, nil)
	objExpect(t, r, 200, "")
	if !bytes.Contains(r.Data, []byte("aws:kms")) {
		t.Fatalf("GetBucketEncryption no longer shows the stored legacy default: %s", r.Data)
	}
}

// Multipart uploads created before the upgrade recorded SSE without
// Encryption (and possibly AWS KMS). Completion encrypts them with SSE-S3,
// and every response of the upload says so.
func TestLegacyMultipartUploadsReportSSE(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta storage.ObjectMeta
	}{
		{"AES256", storage.ObjectMeta{SSE: "AES256"}},
		{"aws:kms", storage.ObjectMeta{SSE: "aws:kms", SSEKMSKeyID: "arn:aws:kms:us-east-1:000000000000:key/abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.createBucket("b")
			ctx := context.Background()
			up, err := e.backend.CreateMultipartUpload(ctx, "b", "k", tc.meta, "")
			if err != nil {
				t.Fatal(err)
			}
			r := e.signed(http.MethodPut, mpPath("b", "k", mpQuery("partNumber", "1", "uploadId", up.UploadID)), []byte("part data"), nil)
			objExpect(t, r, 200, "")
			if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "AES256" {
				t.Fatalf("UploadPart SSE header %q", got)
			}
			etag := r.Header.Get("ETag")
			r = e.signed(http.MethodPost, mpPath("b", "k", mpQuery("uploadId", up.UploadID)), mpCompleteBody("1", etag), nil)
			objExpect(t, r, 200, "")
			if strings.Contains(string(r.Data), "<Error>") {
				t.Fatalf("completion failed: %s", r.Data)
			}
			if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "AES256" {
				t.Fatalf("CompleteMultipartUpload SSE header %q", got)
			}
			info, err := e.backend.GetObjectInfo(ctx, "b", "k", "")
			if err != nil || info.SSE != "AES256" || info.Encryption == "" || info.SSEKMSKeyID != "" {
				t.Fatalf("stored %+v %v", info.ObjectMeta, err)
			}
		})
	}
}

// A KMS key ID without KMS encryption is InvalidArgument, and its message
// no longer suggests AWS KMS (which is NotImplemented).
func TestKMSKeyIDHeaderRefused(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	r := e.signed(http.MethodPut, "/b/k", []byte("x"), objHdr("X-Amz-Server-Side-Encryption", "AES256",
		"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", "key"))
	objExpect(t, r, 400, "InvalidArgument")
	if bytes.Contains(r.Data, []byte("requires HTTP header")) {
		t.Fatalf("message still asks for aws:kms: %s", r.Data)
	}
	objExpect(t, e.signed(http.MethodPut, "/b/k", []byte("x"), objHdr("X-Amz-Server-Side-Encryption", "aws:kms:dsse")), 501, "NotImplemented")
}
