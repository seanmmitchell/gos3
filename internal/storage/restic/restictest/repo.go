package restictest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// A fake repository holds
//
//	config               the repository's configuration;
//	snapshots/<id>       one JSON snapshotDoc per snapshot (and nothing else,
//	                     so that tests can count snapshots);
//	data/<sha256>        file contents, shared by the snapshots holding them.

// snapshotDoc is a snapshot of a fake repository.
type snapshotDoc struct {
	Time     time.Time `json:"time"`
	Parent   string    `json:"parent,omitempty"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags,omitempty"`
	Summary  summary   `json:"summary"`
	// Nodes are the snapshot's items in depth-first order (see
	// comparePaths).
	Nodes []node `json:"nodes"`
}

// node is an item of a snapshot.
type node struct {
	// Path is the item's absolute path in the snapshot ("/a/b").
	Path    string    `json:"path"`
	Type    string    `json:"type"` // "file" or "dir"
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mtime"`
	// Blob names the file's content under data/ ("" for an empty file).
	Blob string `json:"blob,omitempty"`
}

// summary is restic's backup summary record (and a snapshot's summary).
type summary struct {
	MessageType         string    `json:"message_type,omitempty"`
	FilesNew            int64     `json:"files_new"`
	FilesChanged        int64     `json:"files_changed"`
	FilesUnmodified     int64     `json:"files_unmodified"`
	DirsNew             int64     `json:"dirs_new"`
	DirsChanged         int64     `json:"dirs_changed"`
	DirsUnmodified      int64     `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           int64     `json:"data_added"`
	DataAddedPacked     int64     `json:"data_added_packed"`
	TotalFilesProcessed int64     `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	SnapshotID          string    `json:"snapshot_id,omitempty"`
}

// listedSnapshot is a snapshot as `restic snapshots --json` lists it.
type listedSnapshot struct {
	Time     time.Time `json:"time"`
	Parent   string    `json:"parent,omitempty"`
	Tree     string    `json:"tree"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags,omitempty"`
	Summary  *summary  `json:"summary,omitempty"`
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
}

// newID returns a new random snapshot ID.
func newID() string {
	var id [32]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// isID reports whether name is a full snapshot ID.
func isID(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil && strings.ToLower(name) == name
}

// snapshotIDs returns the IDs of the repository's snapshots.
func snapshotIDs(st store) ([]string, error) {
	names, err := st.list("snapshots")
	if err != nil {
		return nil, err
	}
	ids := names[:0]
	for _, n := range names {
		if isID(n) { // not a concurrent put's temporary file
			ids = append(ids, n)
		}
	}
	return ids, nil
}

// resolve returns the ID of the snapshot ref names: a full ID or a unique
// prefix of one.
func resolve(st store, ref string) (string, error) {
	if isID(ref) {
		if ok, err := st.exists("snapshots/" + ref); err != nil || ok {
			return ref, err
		}
		return "", fmt.Errorf("no matching ID found for prefix %q", ref)
	}
	if ref == "" {
		return "", errors.New("empty snapshot ID")
	}
	ids, err := snapshotIDs(st)
	if err != nil {
		return "", err
	}
	var match []string
	for _, id := range ids {
		if strings.HasPrefix(id, ref) {
			match = append(match, id)
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("no matching ID found for prefix %q", ref)
	case 1:
		return match[0], nil
	default:
		return "", fmt.Errorf("multiple IDs with prefix %q found", ref)
	}
}

// loadDoc returns the snapshot id.
func loadDoc(st store, id string) (snapshotDoc, error) {
	var buf bytes.Buffer
	if err := st.get("snapshots/"+id, &buf); err != nil {
		return snapshotDoc{}, err
	}
	var doc snapshotDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		return snapshotDoc{}, fmt.Errorf("snapshot %s is damaged: %v", id, err)
	}
	return doc, nil
}

// saveDoc stores the snapshot doc as id; the snapshot exists from then on.
func saveDoc(st store, id string, doc snapshotDoc) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return st.put("snapshots/"+id, bytes.NewReader(raw), int64(len(raw)))
}

// listed returns doc as restic lists it.
func (doc snapshotDoc) listed(id string) listedSnapshot {
	tree := sha256.Sum256([]byte(id))
	sum := doc.Summary
	return listedSnapshot{
		Time: doc.Time, Parent: doc.Parent, Tree: hex.EncodeToString(tree[:]), Paths: doc.Paths,
		Hostname: doc.Hostname, Tags: doc.Tags, Summary: &sum, ID: id, ShortID: id[:8],
	}
}

// saveBlob stores the content of r and returns its name ("" when empty),
// its size and how many bytes the repository did not hold yet.
func saveBlob(st store, r io.Reader) (blob string, size, added int64, err error) {
	tmp, err := os.CreateTemp("", "fakerestic-")
	if err != nil {
		return "", 0, 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	if size, err = io.Copy(io.MultiWriter(tmp, h), r); err != nil || size == 0 {
		return "", 0, 0, err
	}
	blob = hex.EncodeToString(h.Sum(nil))
	name := "data/" + blob
	if ok, err := st.exists(name); err != nil {
		return "", 0, 0, err
	} else if ok {
		return blob, size, 0, nil
	}
	if err := st.put(name, tmp, size); err != nil {
		// A concurrent backup may have stored (and be reading) the same
		// content.
		if ok, _ := st.exists(name); !ok {
			return "", 0, 0, err
		}
		return blob, size, 0, nil
	}
	return blob, size, size, nil
}

// getBlob copies the content blob to w.
func getBlob(st store, blob string, w io.Writer) error {
	if blob == "" {
		return nil
	}
	return st.get("data/"+blob, w)
}

// comparePaths orders snapshot paths depth first, by name (restic's
// order): "/a" < "/a/z" < "/a-b".
func comparePaths(a, b string) int {
	return compareStrings(strings.Split(a, "/"), strings.Split(b, "/"))
}

// compareStrings compares a and b element by element.
func compareStrings(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := strings.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

// dirOf returns the path of the directory holding the item at p ("" for
// the snapshot's root).
func dirOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// index returns nodes by path.
func index(nodes []node) map[string]node {
	m := make(map[string]node, len(nodes))
	for _, n := range nodes {
		m[n.Path] = n
	}
	return m
}

// children returns the paths of the items of each directory of nodes (in
// the nodes' order), by directory path ("" for the root).
func children(nodes []node) map[string][]string {
	m := make(map[string][]string)
	for _, n := range nodes {
		d := dirOf(n.Path)
		m[d] = append(m[d], n.Path)
	}
	return m
}
