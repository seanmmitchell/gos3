package fs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// The fake restic of package restictest runs restic-archived versions in
// unit tests; the real restic is exercised by the integration tests
// (skipped without restic on PATH).

func TestMain(m *testing.M) { restictest.Main(m) }

// installFakeRestic puts a fake restic first on PATH for the test (see
// restictest.Install) and returns the path of its invocation log.
func installFakeRestic(t *testing.T, variant string) string {
	t.Helper()
	return restictest.Install(t, variant)
}

// resticCalls returns how often the fake ran the subcommand cmd (e.g.
// "backup", "cat config").
func resticCalls(t *testing.T, log, cmd string) int {
	t.Helper()
	return restictest.Calls(t, log, cmd)
}

// snapshotCount returns how many snapshots the fake repository of the
// bucket bucket on b holds.
func snapshotCount(t *testing.T, b *Backend, bucket string) int {
	t.Helper()
	id, err := b.encryptionBucketID(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(b.resticRepository(id), "snapshots"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(entries)
}
