package multi

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	storagefs "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func TestResticBackingUsesLiveConfiguration(t *testing.T) {
	ctx := context.Background()
	oldDir, newDir := t.TempDir(), t.TempDir()
	r := openRouter(t, Options{Backings: []Backing{{Name: "default", Kind: "memory", Backend: memory.New()}, fsBacking(t, "extra", oldDir)}})
	r.EnableBackingManagement(config.StorageConfig{Backend: config.BackendMemory, Backings: []config.BackingConfig{{Name: "extra", Backend: config.BackendFS, Path: oldDir}}}, "", func(d config.BackingDefinition) (Backing, error) {
		b, err := storagefs.Open(d.Path, storagefs.Options{})
		return Backing{Name: d.Name, Kind: d.Backend, Path: d.Path, Backend: b}, err
	})
	handle, err := r.ResticBacking("extra")
	if err != nil {
		t.Fatal(err)
	}
	in, err := r.BackingConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	in.Backings[0].Path = newDir
	if _, err := r.ConfigureBackings(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := handle.ConfigureResticBacking(ctx, nil); err != nil {
		t.Fatalf("restic operation retained a replaced backend: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, ".gos3", "restic-backing.json")); err != nil {
		t.Fatal("settings did not reach the current backing")
	}
	if _, err := os.Stat(filepath.Join(oldDir, ".gos3", "restic-backing.json")); !os.IsNotExist(err) {
		t.Fatal("settings were written to a removed backing")
	}
}

func TestReplicasKeepEncryption(t *testing.T) {
	bk := []Backing{fsBacking(t, "a", t.TempDir()), fsBacking(t, "b", t.TempDir()), fsBacking(t, "c", t.TempDir())}
	r := openRouter(t, Options{Backings: bk, NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"}})
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "encrypted", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "encrypted", StateInSync)
	data := strings.Repeat("replicate encrypted content", 5000)
	w, err := r.NewObjectWriterWith(ctx, "encrypted", "k", storage.WriteOptions{Encryption: storage.EncryptionAES256})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(data))
	_, err = w.Commit(ctx, storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionAES256}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.SetReplication(ctx, "encrypted", ModeActivePassive, []string{"b", "c"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "encrypted", StateInSync)
	for _, backing := range bk {
		info, err := backing.Backend.GetObjectInfo(ctx, "encrypted", "k", "")
		if err != nil {
			t.Fatal(err)
		}
		if info.Encryption != storage.EncryptionAES256 {
			t.Fatalf("%s lost encryption: %+v", backing.Name, info)
		}
		// PhysicalSize is derived from the metadata: check the bytes the
		// replica actually stores.
		assertCiphertextOnDisk(t, backing, []byte(data))
		if got := read(t, backing.Backend, "encrypted", "k", ""); got != data {
			t.Fatalf("%s content mismatch", backing.Name)
		}
	}
}

func TestReplicationEncryptionChangedBeforeRead(t *testing.T) {
	bk := memBackings(2)
	r := openRouter(t, Options{Backings: bk})
	ctx := context.Background()
	for _, b := range bk {
		if err := b.Backend.CreateBucket(ctx, "source", storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	w, err := bk[0].Backend.NewObjectWriter(ctx, "source", "k")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("public"))
	old, err := w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w, err = bk[0].Backend.(storage.CompressingBackend).NewObjectWriterWith(ctx, "source", "k", storage.WriteOptions{Encryption: storage.EncryptionAES256})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("private"))
	if _, err = w.Commit(ctx, storage.ObjectMeta{SSE: "AES256"}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// A listing saw plaintext, but opening that null version now sees a new
	// encrypted object. Abort before sending any bytes to the plaintext writer.
	topology := r.topology.Load()
	if _, err = r.importVersion(ctx, topology.backings["a"], topology.backings["b"], "source", "k", old); !errors.Is(err, errSourceChanged) {
		t.Fatalf("expected retry after encryption changed: %v", err)
	}
	if _, err = bk[1].Backend.GetObjectInfo(ctx, "source", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("published inconsistent copy: %v", err)
	}
}

// assertCiphertextOnDisk checks that the fs backing holds exactly one data
// file, of the encrypted size of data and without its plaintext.
func assertCiphertextOnDisk(t *testing.T, backing Backing, data []byte) {
	t.Helper()
	var files []string
	root := filepath.Join(backing.Path, ".gos3", "data")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil || len(files) != 1 {
		t.Fatalf("%s: data files %v (%v)", backing.Name, files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != storage.EncryptedSize(int64(len(data))) || bytes.Contains(raw, data[:32]) {
		t.Fatalf("%s stores %d bytes that are not the ciphertext of %d bytes", backing.Name, len(raw), len(data))
	}
}
