package fs

import (
	"bytes"
	"strings"
	"testing"
)

func TestResticLongProgress(t *testing.T) {
	var out snapshotOutput
	progress := bytes.Repeat([]byte("{\"message_type\":\"status\",\"bytes_done\":12}\n"), 40000)
	snapshot := strings.Repeat("a", 64)
	progress = append(progress, []byte("{\"message_type\":\"summary\",\"snapshot_id\":\""+snapshot+"\"}")...)
	// Feed chunk boundaries through the middle of JSON records.
	for len(progress) > 0 {
		n := min(317, len(progress))
		if _, err := out.Write(progress[:n]); err != nil {
			t.Fatal(err)
		}
		progress = progress[n:]
	}
	if err := out.consume(); err != nil || out.snapshot != snapshot {
		t.Fatalf("lost backup summary: %q %v", out.snapshot, err)
	}
}
