package jobs

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
)

// fileName is the jobs file in Options.Dir.
const fileName = "jobs.json"

// fileVersion is the format version of the jobs file.
const fileVersion = 1

// jobsFile is the persisted form of the jobs (jobs.json).
type jobsFile struct {
	Version int      `json:"version"`
	Jobs    []record `json:"jobs"`
}

// record is one persisted job: the Job plus the Manager's bookkeeping.
type record struct {
	Job
	// Seq orders queued jobs (FIFO); it grows each time a job is queued.
	Seq uint64 `json:"seq,omitempty"`
}

// loadFile reads the jobs file. A missing file holds no jobs; path "" (in
// memory) too. Unknown fields are ignored; another format version, an
// invalid or duplicate ID, an empty kind and an unknown state are refused.
func loadFile(path string) ([]record, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: reading %s: %w", path, err)
	}
	var f jobsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("jobs: %s is corrupt: %v (fix or delete it)", path, err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("jobs: %s: unsupported format version %d (this gos3 reads version %d)", path, f.Version, fileVersion)
	}
	seen := make(map[string]bool, len(f.Jobs))
	for i := range f.Jobs {
		j := &f.Jobs[i].Job
		switch {
		case !validID(j.ID):
			return nil, fmt.Errorf("jobs: %s: job %d has an invalid ID %q", path, i, j.ID)
		case seen[j.ID]:
			return nil, fmt.Errorf("jobs: %s: duplicate job ID %q", path, j.ID)
		case j.Kind == "":
			return nil, fmt.Errorf("jobs: %s: job %q has no kind", path, j.ID)
		case !j.State.Valid():
			return nil, fmt.Errorf("jobs: %s: job %q has an unknown state %q", path, j.ID, j.State)
		}
		seen[j.ID] = true
		// The file is indented: give the runner documents back their
		// compact form.
		j.Spec = compactJSON(j.Spec)
		if j.Status = compactJSON(j.Status); isNullDoc(j.Status) {
			j.Status = nil
		}
		if j.Checkpoint = compactJSON(j.Checkpoint); isNullDoc(j.Checkpoint) {
			j.Checkpoint = nil
		}
	}
	return f.Jobs, nil
}

// compactJSON returns d without insignificant white space.
func compactJSON(d json.RawMessage) json.RawMessage {
	if len(d) == 0 {
		return d
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, d); err != nil {
		return d // cannot happen: the file was valid JSON
	}
	return buf.Bytes()
}

// writeFile writes recs to path atomically (mode 0600).
func writeFile(path string, recs []record) error {
	data, err := json.MarshalIndent(jobsFile{Version: fileVersion, Jobs: recs}, "", "  ")
	if err != nil {
		return fmt.Errorf("jobs: encoding %s: %w", path, err)
	}
	if err := config.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("jobs: writing %s: %w", path, err)
	}
	return nil
}

// newID returns a random job ID: 26 lower-case base32 characters (130
// random bits).
func newID() string { return strings.ToLower(rand.Text()) }

// validID reports whether id can name a job (and its state directory): 1
// to 64 lower-case letters, digits, '-' and '_', starting with a letter or
// digit. IDs from newID always are.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// clone returns a deep copy of j.
func (j Job) clone() Job {
	j.Labels = maps.Clone(j.Labels)
	j.Spec = bytes.Clone(j.Spec)
	j.Status = bytes.Clone(j.Status)
	j.Checkpoint = bytes.Clone(j.Checkpoint)
	return j
}

// marshalDoc encodes a runner's status or checkpoint document; JSON null
// becomes nil (no document).
func marshalDoc(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if string(data) == "null" {
		return nil, nil
	}
	return data, nil
}

// isNullDoc reports whether a stored document is absent.
func isNullDoc(d json.RawMessage) bool {
	d = bytes.TrimSpace(d)
	return len(d) == 0 || string(d) == "null"
}

// sortRecords orders records for the jobs file: by creation time, then ID.
func sortRecords(recs []record) {
	slices.SortFunc(recs, func(a, b record) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// stateDir returns the state directory of job id under dir.
func stateDir(dir, id string) string { return filepath.Join(dir, id) }
