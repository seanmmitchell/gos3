package copier

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func TestMapVersionID(t *testing.T) {
	for _, id := range []string{"null", "abc", "A-b_c.9", strings.Repeat("a", 64)} {
		if got := MapVersionID(id); got != id {
			t.Errorf("MapVersionID(%q) = %q", id, got)
		}
	}
	seen := map[string]bool{}
	for _, id := range []string{"a/b", "a:b", strings.Repeat("a", 65), "", "ümlaut"} {
		got := MapVersionID(id)
		if len(got) != 44 || got[0] != 'x' || !storage.ValidImportedVersionID(got) || seen[got] {
			t.Errorf("MapVersionID(%q) = %q", id, got)
		}
		if MapVersionID(id) != got {
			t.Errorf("MapVersionID(%q) is not deterministic", id)
		}
		seen[got] = true
	}
}

func TestWriteVersionImports(t *testing.T) {
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	lm := time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC)
	v := storage.ObjectInfo{VersionID: "remote/1", LastModified: lm, ObjectMeta: storage.ObjectMeta{ETag: etagOf("hi"), ContentType: "text/x"}}
	w, err := WriteVersion(t.Context(), dst.Backend, dst.Bucket, "k", v, strings.NewReader("hi"), Policy{}, WriteImport)
	if err != nil || !w.Imported || w.Bytes != 2 || w.Info.VersionID != MapVersionID("remote/1") || !w.Info.LastModified.Equal(lm) {
		t.Fatalf("import: %+v, %v", w, err)
	}
	dm := storage.ObjectInfo{VersionID: "m1", DeleteMarker: true, LastModified: lm.Add(time.Second)}
	w, err = WriteVersion(t.Context(), dst.Backend, dst.Bucket, "k", dm, nil, Policy{}, WriteImport)
	if err != nil || !w.Imported || !w.Info.DeleteMarker || w.Info.VersionID != "m1" {
		t.Fatalf("marker import: %+v, %v", w, err)
	}
	h := history(t, dst, "k")
	if len(h) != 2 || !h[0].DeleteMarker || h[1].ContentType != "text/x" {
		t.Fatalf("target %+v", h)
	}
}

func TestWriteVersionPlain(t *testing.T) {
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	lm := time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC)
	v := storage.ObjectInfo{VersionID: "v1", LastModified: lm, ObjectMeta: storage.ObjectMeta{ETag: etagOf("hi")}}
	w, err := WriteVersion(t.Context(), dst.Backend, dst.Bucket, "k", v, strings.NewReader("hi"), Policy{}, WritePlain)
	if err != nil || w.Imported || w.Info.VersionID == "v1" || w.Info.LastModified.Equal(lm) || w.Info.ETag != etagOf("hi") {
		t.Fatalf("plain write: %+v, %v", w, err)
	}
	w, err = WriteVersion(t.Context(), dst.Backend, dst.Bucket, "k", storage.ObjectInfo{VersionID: "m", DeleteMarker: true}, nil, Policy{}, WritePlain)
	if err != nil || w.Imported || !w.Info.DeleteMarker || w.Info.VersionID == "m" {
		t.Fatalf("plain marker: %+v, %v", w, err)
	}
}

func TestWriteVersionFallsBackToPlainWrites(t *testing.T) {
	v := storage.ObjectInfo{VersionID: "v1", LastModified: time.Now(), ObjectMeta: storage.ObjectMeta{ETag: etagOf("hi")}}
	m := memory.New()
	newBucket(t, m, "dst", storage.VersioningEnabled, false)
	for name, b := range map[string]storage.Backend{
		"refusing importer": nativeTarget{m},
		"no importer":       bareTarget{Backend: m, ConditionalDeleter: m},
	} {
		_, err := WriteVersion(t.Context(), b, "dst", "k", v, strings.NewReader("hi"), Policy{}, WriteImport)
		if !errors.Is(err, ErrNoImport) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s, import only: %v", name, err)
		}
		w, err := WriteVersion(t.Context(), b, "dst", "k", v, strings.NewReader("hi"), Policy{}, WriteImportOrPlain)
		if err != nil || w.Imported || w.Bytes != 2 {
			t.Errorf("%s, import or plain: %+v, %v", name, w, err)
		}
		// Compression is optional; encryption is not.
		cv := v
		cv.Compression = storage.CompressionDeflate
		if _, err := WriteVersion(t.Context(), b, "dst", "k", cv, strings.NewReader("hi"), Policy{}, WriteImportOrPlain); err != nil {
			t.Errorf("%s, compressed: %v", name, err)
		}
		ev := v
		ev.Encryption, ev.SSE = storage.EncryptionAES256, "AES256"
		if _, err := WriteVersion(t.Context(), b, "dst", "k", ev, strings.NewReader("hi"), Policy{}, WriteImportOrPlain); !errors.Is(err, ErrEncryptionUnsupported) {
			t.Errorf("%s, encrypted: %v", name, err)
		}
	}
}

func TestWriteVersionEncryptsWithTheTargetsChoice(t *testing.T) {
	dst := memEndpoint(t, "dst", "", storage.VersioningEnabled)
	v := storage.ObjectInfo{VersionID: "v1", LastModified: time.Now(), ObjectMeta: storage.ObjectMeta{ETag: etagOf("hi")}}
	pol := Policy{Encryption: EncryptionTarget, TargetEncryption: func(storage.ObjectInfo) string { return storage.EncryptionAES256 }}
	w, err := WriteVersion(t.Context(), dst.Backend, dst.Bucket, "k", v, strings.NewReader("hi"), pol, WriteImport)
	if err != nil || w.Info.Encryption != storage.EncryptionAES256 {
		t.Fatalf("target encryption: %+v, %v", w, err)
	}
	pol.TargetEncryption = func(storage.ObjectInfo) string { return storage.EncryptionRestic }
	if _, err := WriteVersion(t.Context(), dst.Backend, dst.Bucket, "k", v, strings.NewReader("hi"), pol, WriteImport); !errors.Is(err, ErrEncryptionUnsupported) {
		t.Fatalf("restic on memory: %v", err)
	}
	pol.AllowPlaintext = true
	if w, err := WriteVersion(t.Context(), dst.Backend, dst.Bucket, "k", v, strings.NewReader("hi"), pol, WriteImport); err != nil || w.Info.Encryption != "" {
		t.Fatalf("restic on memory with plaintext allowed: %+v, %v", w, err)
	}
}
