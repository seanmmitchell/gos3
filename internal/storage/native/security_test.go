package native_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

// gos3MetaHeader returns a gos3-meta header holding the JSON doc.
func gos3MetaHeader(doc string) http.Header {
	return http.Header{"X-Amz-Meta-Gos3-Meta": {base64.RawURLEncoding.EncodeToString([]byte(doc))}}
}

// TestForgedMetaIgnored: whoever can write the remote bucket cannot
// publish an object through a gos3-meta ACL or redirect.
func TestForgedMetaIgnored(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	h.mk("b", storage.CreateBucketOptions{})
	for key, doc := range map[string]string{
		"plain":      `{"acl":"public-read","wr":"https://evil.example/","sc":"GLACIER"}`,
		"with-a-mac": `{"acl":"public-read","wr":"https://evil.example/","sc":"GLACIER","s":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`,
	} {
		h.remotePut("nb-b", key, []byte("secret"), gos3MetaHeader(doc))
		info := h.info("b", key, "")
		if info.ACL != "" || info.WebsiteRedirect != "" {
			t.Errorf("%s: forged header honoured: ACL %q, redirect %q", key, info.ACL, info.WebsiteRedirect)
		}
		if info.StorageClass != "GLACIER" {
			t.Errorf("%s: storage class %q, want the header's (other fields are used)", key, info.StorageClass)
		}
		if got, _ := h.read("b", key, ""); got.ACL != "" {
			t.Errorf("%s: GET reports ACL %q", key, got.ACL)
		}
	}
}

// remoteExt decodes the gos3-meta header of a remote object.
func remoteExt(t *testing.T, a s3client.ObjectAttrs) string {
	t.Helper()
	v := a.Header.Get("X-Amz-Meta-Gos3-Meta")
	if v == "" {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestACLInOverlayOnly: gos3's own ACLs and redirects round-trip through
// overlay records, for every write path, and never travel with the object.
func TestACLInOverlayOnly(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, func(o *native.Options) { o.PartSize = 5 << 20 })
	ctx := t.Context()
	h.mkVersioned("b")
	meta := storage.ObjectMeta{ACL: "public-read", WebsiteRedirect: "/elsewhere", ContentType: "text/plain"}
	h.mustPut("b", "single", []byte("public"), meta)
	big := randData(6<<20, 1)
	h.mustPut("b", "upload", big, meta)
	u, err := h.CreateMultipartUpload(ctx, "b", "mp", meta, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, h, "b", "mp", u.UploadID, 1, randData(5<<20, 2))
	uploadPart(t, h, "b", "mp", u.UploadID, 2, []byte("tail"))
	if _, err := h.CompleteMultipartUpload(ctx, "b", "mp", u.UploadID, []int{1, 2}, meta, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"single", "upload", "mp"} {
		info := h.info("b", key, "")
		if info.ACL != "public-read" || info.WebsiteRedirect != "/elsewhere" {
			t.Errorf("%s: ACL %q, redirect %q", key, info.ACL, info.WebsiteRedirect)
		}
		if got, _ := h.read("b", key, ""); got.ACL != "public-read" {
			t.Errorf("%s: GET ACL %q", key, got.ACL)
		}
		if ext := remoteExt(t, h.remoteHead("b", key, "")); strings.Contains(ext, `"acl"`) || strings.Contains(ext, `"wr"`) {
			t.Errorf("%s: the ACL or redirect travels with the object: %s", key, ext)
		}
	}
	var listed string
	if err := h.WalkObjects(ctx, "b", "single", "", func(o storage.ObjectInfo) error { listed = o.ACL; return nil }); err != nil {
		t.Fatal(err)
	}
	if listed != "public-read" {
		t.Errorf("listed ACL %q", listed)
	}
}

// TestACLNotCarriedByCopies: a version written by another tool with the
// content and headers of a gos3 version (a restore of an old version, a
// CopyObject) does not get that version's ACL; an ACL revoked through gos3
// stays revoked.
func TestACLNotCarriedByCopies(t *testing.T) {
	h := newHarness(t, s3clienttest.Options{}, nil)
	ctx := t.Context()
	h.mkVersioned("b")
	v1 := h.mustPut("b", "k", []byte("secret"), storage.ObjectMeta{ACL: "public-read", ContentType: "text/x-é"})
	// A CopyObject to another key.
	if _, err := h.c.CopyObject(ctx, "nb-b", "copy", "k", s3client.CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	if info := h.info("b", "copy", ""); info.ACL != "" {
		t.Errorf("copy: ACL %q", info.ACL)
	}
	// Revoked through gos3, then restored by another tool (a new version
	// with v1's content and headers).
	if _, err := h.UpdateObjectMeta(ctx, "b", "k", v1.VersionID, func(m *storage.ObjectMeta) error { m.ACL = "private"; return nil }); err != nil {
		t.Fatal(err)
	}
	a := h.remoteHead("b", "k", v1.VersionID)
	restored := h.remotePut("nb-b", "k", []byte("secret"), http.Header{
		"X-Amz-Meta-Gos3-Meta": {a.Header.Get("X-Amz-Meta-Gos3-Meta")},
		"Content-Type":         {a.Header.Get("Content-Type")},
	})
	if info := h.info("b", "k", ""); info.VersionID != restored.VersionID || info.ACL == "public-read" {
		t.Errorf("restored version %s: ACL %q", info.VersionID, info.ACL)
	}
	if info := h.info("b", "k", v1.VersionID); info.ACL != "private" {
		t.Errorf("revoked version: ACL %q", info.ACL)
	}
	// New content under a copied header (an unversioned overwrite).
	h.mk("u", storage.CreateBucketOptions{})
	h.mustPut("u", "k", []byte("public announcement"), storage.ObjectMeta{ACL: "public-read"})
	h.remotePut("nb-u", "k", []byte("entirely different content"), nil)
	if info := h.info("u", "k", ""); info.ACL != "" {
		t.Errorf("foreign content: ACL %q", info.ACL)
	}
}

// TestACLOverlayWriteFails: when the overlay of a new object's ACL cannot
// be stored, the write fails and the object is private.
func TestACLOverlayWriteFails(t *testing.T) {
	h, state := newFaulty(t)
	state.set(&state.failPutRecord, true)
	_, err := h.put("bkt", "k", []byte("x"), storage.ObjectMeta{ACL: "public-read"}, storage.CommitOptions{})
	state.set(&state.failPutRecord, false)
	if !errors.Is(err, errInjected) {
		t.Fatalf("write with a failing overlay: %v", err)
	}
	if info := h.info("bkt", "k", ""); info.ACL != "" {
		t.Errorf("object after the failed write: ACL %q, want private", info.ACL)
	}
}
