// Package restictest provides a fake restic executable for unit tests of
// code that runs restic through package restic.
//
// The fake is the test binary itself: Install copies it to a directory put
// first on PATH as "restic" and sets GOS3_FAKE_RESTIC=1, and Main (called
// from the package's TestMain) runs the fake instead of the tests when that
// variable is set. It supports the subcommands and options gos3 uses
// (others fail, as an unknown flag fails restic):
//
//   - version, cat config, init;
//   - backup --stdin [--stdin-filename name] (one file from stdin);
//   - backup [--parent id] [--ignore-inode] [--ignore-ctime] [--time t]
//     [--no-scan] <relative paths> (a directory tree; "." stores the
//     working directory's entries at the snapshot's root, like restic).
//     With a parent, a file whose size and modification time match the
//     parent's is not read when both --ignore-inode and --ignore-ctime are
//     given (its content is the parent's), and read otherwise (counted
//     unmodified when its content and metadata equal the parent's);
//   - both backups take --host and --tag, and with --json report a status
//     and a summary record (files_new, files_changed, files_unmodified,
//     dirs_*, data_added, total_files_processed, total_bytes_processed,
//     snapshot_id);
//   - snapshots --json [--host h]... [--tag a,b]... [id...]: a comma joins
//     tags that must all match, repeated options alternatives;
//   - forget <id>... (no policies), prune [--max-unused v] (a no-op);
//   - diff [--json] [--metadata] <a> <b>: change records ("+", "-", "M",
//     "T", "U") and a statistics record;
//   - dump [--archive tar] <id> <path>: a file's content, or a tar archive
//     of a directory ("/" for the whole snapshot).
//
// Snapshots may be named by unique ID prefixes. Repositories are
//
//   - a local directory: <repo>/config, <repo>/snapshots/<id> (one JSON
//     document per snapshot, nothing else) and <repo>/data/<sha256> (file
//     contents);
//   - restic's s3 backend, "s3:[http(s)://]host[:port]/bucket[/path]":
//     the same files as objects below path in the bucket, reached through
//     internal/s3client with credentials from AWS_ACCESS_KEY_ID and
//     AWS_SECRET_ACCESS_KEY, path-style addressing from
//     `-o s3.bucket-lookup=path` and the region from `-o s3.region=...`
//     or AWS_DEFAULT_REGION. init creates a missing bucket.
//
// The fake is controlled by environment variables (Install resets them):
//
//   - EnvLog (FAKERESTIC_LOG): file receiving each invocation's subcommand
//     ("backup", "cat config", ...) on a line of its own (see Calls);
//   - EnvFail (FAKERESTIC_FAIL): a subcommand that fails, with diagnostics
//     quoting the password;
//   - EnvVersion (FAKERESTIC_VERSION): the release `restic version` reports
//     (default 0.17.3);
//   - EnvDumpDelay (FAKERESTIC_DUMP_DELAY): a duration each dump waits
//     first;
//   - EnvReread (FAKERESTIC_REREAD=1): tree backups read the files they
//     would take from the parent, counting those whose content differs as
//     changed (and storing what they read), like a restic that does not
//     trust placeholders;
//   - EnvIncomplete (FAKERESTIC_INCOMPLETE=1): tree backups save their
//     snapshot but report an unreadable file and exit with code 3, like
//     restic's incomplete snapshots.
//
// The subcommand "noise" writes 8 MiB of diagnostics and fails. The real
// restic is exercised by integration tests, which skip without restic on
// PATH.
package restictest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Environment variables controlling the fake (see the package
// documentation).
const (
	EnvFake       = "GOS3_FAKE_RESTIC"
	EnvLog        = "FAKERESTIC_LOG"
	EnvFail       = "FAKERESTIC_FAIL"
	EnvVersion    = "FAKERESTIC_VERSION"
	EnvDumpDelay  = "FAKERESTIC_DUMP_DELAY"
	EnvReread     = "FAKERESTIC_REREAD"
	EnvIncomplete = "FAKERESTIC_INCOMPLETE"
)

// Main runs the fake restic when the test binary was started as one, and
// m's tests otherwise. Call it from TestMain:
//
//	func TestMain(m *testing.M) { restictest.Main(m) }
func Main(m *testing.M) {
	if os.Getenv(EnvFake) == "1" {
		os.Exit(run(os.Args[1:]))
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

var (
	once   sync.Once
	dir    string // parent of the fake executables' directories
	dirErr error
)

// Install puts a fake restic (in a directory of its own named variant, so
// that version checks, which are cached per executable path, are not
// shared between variants) first on PATH for the test and returns the path
// of its invocation log. The package's TestMain must call Main.
func Install(t testing.TB, variant string) string {
	t.Helper()
	once.Do(func() { dir, dirErr = os.MkdirTemp("", "gos3-fake-restic-") })
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	vdir := filepath.Join(dir, variant)
	name := "restic"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if _, err := os.Stat(filepath.Join(vdir, name)); err != nil {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, name), raw, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "restic.log")
	t.Setenv("PATH", vdir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvFake, "1")
	t.Setenv(EnvLog, log)
	for _, env := range []string{EnvFail, EnvVersion, EnvDumpDelay, EnvReread, EnvIncomplete} {
		t.Setenv(env, "")
	}
	return log
}

// Calls returns how often the fake logging to log ran the subcommand cmd
// (e.g. "backup", "cat config").
func Calls(t testing.TB, log, cmd string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSuffix(line, "\r") == cmd {
			n++
		}
	}
	return n
}

// fatal reports a fatal error the way restic does and returns its exit
// code 1.
func fatal(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "Fatal: "+format+"\n", args...)
	return 1
}

// valueFlags are the subcommand options taking a value.
var valueFlags = map[string]bool{
	"--host": true, "--tag": true, "--parent": true, "--time": true, "--archive": true,
	"--stdin-filename": true, "--max-unused": true,
}

// boolFlags are the subcommand options without a value.
var boolFlags = map[string]bool{
	"--stdin": true, "--ignore-inode": true, "--ignore-ctime": true, "--no-scan": true, "--metadata": true,
}

// flagSet holds a subcommand's options and arguments.
type flagSet struct {
	values map[string][]string
	bools  map[string]bool
	args   []string
	json   bool // the global --json
}

// value returns the last value of the option name ("" when not given).
func (f flagSet) value(name string) string {
	v := f.values[name]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

// parseFlags parses a subcommand's options ("--name value" or
// "--name=value") and arguments.
func parseFlags(args []string) (flagSet, error) {
	f := flagSet{values: map[string][]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			f.args = append(f.args, a)
			continue
		}
		name, value, inline := strings.Cut(a, "=")
		switch {
		case valueFlags[name]:
			if !inline {
				if i+1 >= len(args) {
					return f, fmt.Errorf("flag needs an argument: %s", name)
				}
				i++
				value = args[i]
			}
			f.values[name] = append(f.values[name], value)
		case boolFlags[name] && !inline:
			f.bools[name] = true
		default:
			return f, fmt.Errorf("unknown flag: %s", a)
		}
	}
	return f, nil
}

// run is the fake's main function.
func run(args []string) int {
	var repo, sub string
	var rest []string
	var jsonOut bool
	opts := map[string]string{}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--repo", "--retry-lock", "--http-user-agent", "--cache-dir", "-o", "--option":
			if i+1 >= len(args) {
				return fatal("flag %s needs a value", a)
			}
			switch a {
			case "--repo":
				repo = args[i+1]
			case "--retry-lock":
				if _, err := time.ParseDuration(args[i+1]); err != nil {
					return fatal("invalid argument %q for --retry-lock", args[i+1])
				}
			case "-o", "--option":
				k, v, _ := strings.Cut(args[i+1], "=")
				opts[k] = v
			}
			i++
		case "--json":
			jsonOut = true
		case "--no-cache", "--no-lock":
		default:
			if sub == "" {
				sub = a
			} else {
				rest = append(rest, a)
			}
		}
	}
	if sub == "cat" && len(rest) > 0 {
		sub, rest = "cat "+rest[0], rest[1:]
	}
	if log := os.Getenv(EnvLog); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, sub)
			_ = f.Close()
		}
	}
	switch sub {
	case "version":
		v := os.Getenv(EnvVersion)
		if v == "" {
			v = "0.17.3"
		}
		fmt.Printf("restic %s compiled with go1.23 on %s/%s\n", v, runtime.GOOS, runtime.GOARCH)
		return 0
	case "noise":
		// Plenty of diagnostics, to check that they are bounded.
		line := append(bytes.Repeat([]byte("x"), 1023), '\n')
		for range 8 << 10 {
			_, _ = os.Stderr.Write(line)
		}
		return 1
	}
	if os.Getenv(EnvFail) == sub {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Fprintf(os.Stderr, "progress...\nFatal: unable to save snapshot: password %s rejected\n", os.Getenv("RESTIC_PASSWORD"))
		return 1
	}
	fl, err := parseFlags(rest)
	if err != nil {
		return fatal("%v", err)
	}
	fl.json = jsonOut
	if os.Getenv("RESTIC_PASSWORD") == "" {
		return fatal("no password")
	}
	if repo == "" {
		return fatal("Please specify repository location (-r or --repository-file)")
	}
	var st store = localStore{root: repo}
	if spec, ok := strings.CutPrefix(repo, "s3:"); ok {
		s, err := newS3Store(spec, opts)
		if err != nil {
			return fatal("%v", err)
		}
		st = s
	}
	if sub != "init" {
		ok, err := st.exists("config")
		if err != nil {
			return fatal("unable to open config file: %v", err)
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "Fatal: repository does not exist: unable to open config file")
			return 10
		}
	}
	switch sub {
	case "cat config":
		if err := st.get("config", os.Stdout); err != nil {
			return fatal("%v", err)
		}
	case "init":
		if ok, err := st.exists("config"); err != nil || ok {
			return fatal("create repository: config file already exists (%v)", err)
		}
		if err := st.create(); err != nil {
			return fatal("create repository: %v", err)
		}
		cfg := []byte(`{"version":2}`)
		if err := st.put("config", bytes.NewReader(cfg), int64(len(cfg))); err != nil {
			return fatal("create repository: %v", err)
		}
	case "backup":
		if fl.bools["--stdin"] {
			return backupStdin(st, fl)
		}
		return backupTree(st, fl)
	case "dump":
		return dump(st, fl)
	case "snapshots":
		return snapshots(st, fl)
	case "forget":
		return forget(st, fl)
	case "prune":
		fmt.Println("fake restic: nothing to prune")
	case "diff":
		return diff(st, fl)
	default:
		return fatal("unknown command %q", sub)
	}
	return 0
}

// hostOf returns the backup's host name.
func hostOf(fl flagSet) string {
	if h := fl.value("--host"); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

// tagsOf returns the backup's tags (comma-separated within an option).
func tagsOf(fl flagSet) []string {
	var tags []string
	for _, v := range fl.values["--tag"] {
		for _, t := range strings.Split(v, ",") {
			if t != "" && !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// report writes a backup's status and summary records.
func report(fl flagSet, id string, sum summary) {
	if !fl.json {
		fmt.Printf("snapshot %s saved\n", id[:8])
		return
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{
		"message_type": "status", "percent_done": 1,
		"total_files": sum.TotalFilesProcessed, "files_done": sum.TotalFilesProcessed,
		"total_bytes": sum.TotalBytesProcessed, "bytes_done": sum.TotalBytesProcessed,
	})
	sum.MessageType, sum.SnapshotID = "summary", id
	_ = enc.Encode(sum)
}

// backupStdin archives stdin as a new snapshot. The snapshot exists only
// once the input ended, as with restic.
func backupStdin(st store, fl flagSet) int {
	start := time.Now()
	blob, size, added, err := saveBlob(st, os.Stdin)
	if err != nil {
		return fatal("%v", err)
	}
	name := fl.value("--stdin-filename")
	if name == "" {
		name = "stdin"
	}
	sum := summary{
		FilesNew: 1, DataAdded: added, DataAddedPacked: added,
		TotalFilesProcessed: 1, TotalBytesProcessed: size, BackupStart: start, BackupEnd: time.Now(),
	}
	doc := snapshotDoc{
		Time: start, Paths: []string{"/" + name}, Hostname: hostOf(fl), Tags: tagsOf(fl), Summary: sum,
		Nodes: []node{{Path: "/" + name, Type: "file", Size: size, ModTime: start, Blob: blob}},
	}
	id := newID()
	if err := saveDoc(st, id, doc); err != nil {
		return fatal("unable to save snapshot: %v", err)
	}
	report(fl, id, sum)
	return 0
}

// backupTree archives the directory trees named by the (relative)
// arguments as a new snapshot.
func backupTree(st store, fl flagSet) int {
	start := time.Now()
	if len(fl.args) == 0 {
		return fatal("nothing to backup, please specify source files/dirs")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fatal("%v", err)
	}
	var nodes []node
	local := map[string]string{} // file path by node path
	seen := map[string]bool{}
	var walk func(file, p string) error
	walk = func(file, p string) error {
		fi, err := os.Lstat(file)
		if err != nil {
			return err
		}
		if !seen[p] {
			seen[p] = true
			switch {
			case fi.IsDir():
				nodes = append(nodes, node{Path: p, Type: "dir", ModTime: fi.ModTime()})
			case fi.Mode().IsRegular():
				nodes = append(nodes, node{Path: p, Type: "file", Size: fi.Size(), ModTime: fi.ModTime()})
				local[p] = file
			default:
				return fmt.Errorf("%s: the fake restic does not back up %v", file, fi.Mode().Type())
			}
		}
		if !fi.IsDir() {
			return nil
		}
		entries, err := os.ReadDir(file)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := walk(filepath.Join(file, e.Name()), p+"/"+e.Name()); err != nil {
				return err
			}
		}
		return nil
	}
	var paths []string
	for _, target := range fl.args {
		clean := filepath.Clean(target)
		if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fatal("the fake restic backs up relative paths only, not %q", target)
		}
		abs := filepath.Join(cwd, clean)
		paths = append(paths, abs)
		if clean == "." {
			// restic replaces "." by the directory's entries.
			entries, err := os.ReadDir(cwd)
			if err != nil {
				return fatal("%v", err)
			}
			for _, e := range entries {
				if err := walk(filepath.Join(cwd, e.Name()), "/"+e.Name()); err != nil {
					return fatal("%v", err)
				}
			}
			continue
		}
		comps := strings.Split(filepath.ToSlash(clean), "/")
		for i := 1; i < len(comps); i++ {
			p := "/" + strings.Join(comps[:i], "/")
			fi, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(p)))
			if err != nil {
				return fatal("%v", err)
			}
			if !seen[p] {
				seen[p] = true
				nodes = append(nodes, node{Path: p, Type: "dir", ModTime: fi.ModTime()})
			}
		}
		if err := walk(abs, "/"+filepath.ToSlash(clean)); err != nil {
			return fatal("%v", err)
		}
	}
	slices.SortFunc(nodes, func(a, b node) int { return comparePaths(a.Path, b.Path) })

	var parentID string
	var parentNodes []node
	if ref := fl.value("--parent"); ref != "" {
		if parentID, err = resolve(st, ref); err != nil {
			return fatal("invalid id %q: %v", ref, err)
		}
		doc, err := loadDoc(st, parentID)
		if err != nil {
			return fatal("%v", err)
		}
		parentNodes = doc.Nodes
	}
	parent := index(parentNodes)
	trust := fl.bools["--ignore-inode"] && fl.bools["--ignore-ctime"] && os.Getenv(EnvReread) != "1"
	sum := summary{BackupStart: start}
	unmodified := map[string]bool{}
	for i := range nodes {
		n := &nodes[i]
		if n.Type != "file" {
			continue
		}
		sum.TotalFilesProcessed++
		prev, ok := parent[n.Path]
		ok = ok && prev.Type == "file"
		if ok && trust && prev.Size == n.Size && prev.ModTime.Equal(n.ModTime) {
			// Not read: the parent's content.
			n.Blob = prev.Blob
			sum.FilesUnmodified++
			sum.TotalBytesProcessed += n.Size
			unmodified[n.Path] = true
			continue
		}
		f, err := os.Open(local[n.Path])
		if err != nil {
			return fatal("%v", err)
		}
		blob, size, added, err := saveBlob(st, f)
		_ = f.Close()
		if err != nil {
			return fatal("%v", err)
		}
		n.Blob, n.Size = blob, size
		sum.DataAdded += added
		sum.TotalBytesProcessed += size
		switch {
		case !ok:
			sum.FilesNew++
		case prev.Blob == n.Blob && prev.Size == n.Size && prev.ModTime.Equal(n.ModTime):
			sum.FilesUnmodified++
			unmodified[n.Path] = true
		default:
			sum.FilesChanged++
		}
	}
	// Directories, children first: unmodified when their modification
	// time and items are the parent's, and every item is unmodified.
	items, parentItems := children(nodes), children(parentNodes)
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		if n.Type != "dir" {
			continue
		}
		prev, ok := parent[n.Path]
		if !ok || prev.Type != "dir" {
			sum.DirsNew++
			continue
		}
		same := prev.ModTime.Equal(n.ModTime) && slices.Equal(items[n.Path], parentItems[n.Path])
		for _, c := range items[n.Path] {
			same = same && unmodified[c]
		}
		if same {
			sum.DirsUnmodified++
			unmodified[n.Path] = true
		} else {
			sum.DirsChanged++
		}
	}
	sum.DataAddedPacked = sum.DataAdded
	sum.BackupEnd = time.Now()

	snapTime := start
	if v := fl.value("--time"); v != "" {
		if snapTime, err = time.ParseInLocation("2006-01-02 15:04:05", v, time.Local); err != nil {
			return fatal("invalid --time %q: %v", v, err)
		}
	}
	doc := snapshotDoc{
		Time: snapTime, Parent: parentID, Paths: paths, Hostname: hostOf(fl), Tags: tagsOf(fl),
		Summary: sum, Nodes: nodes,
	}
	if doc.Nodes == nil {
		doc.Nodes = []node{}
	}
	id := newID()
	if err := saveDoc(st, id, doc); err != nil {
		return fatal("unable to save snapshot: %v", err)
	}
	report(fl, id, sum)
	if os.Getenv(EnvIncomplete) == "1" {
		fmt.Fprintln(os.Stderr, `{"message_type":"error","error":{"message":"read: access denied"},"during":"archival","item":"unreadable"}`)
		fmt.Fprintln(os.Stderr, "Warning: at least one source file could not be read")
		return 3
	}
	return 0
}

// snapshots lists the snapshots as JSON.
func snapshots(st store, fl flagSet) int {
	if !fl.json {
		return fatal("the fake restic lists snapshots as JSON only")
	}
	var ids []string
	var err error
	if len(fl.args) > 0 {
		for _, ref := range fl.args {
			id, err := resolve(st, ref)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Ignoring %q: %v\n", ref, err)
				continue
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	} else if ids, err = snapshotIDs(st); err != nil {
		return fatal("%v", err)
	}
	hosts := fl.values["--host"]
	var groups [][]string // alternatives of tags that must all match
	for _, v := range fl.values["--tag"] {
		groups = append(groups, strings.Split(v, ","))
	}
	out := []listedSnapshot{}
	for _, id := range ids {
		doc, err := loadDoc(st, id)
		if err != nil {
			return fatal("%v", err)
		}
		if len(hosts) > 0 && !slices.Contains(hosts, doc.Hostname) {
			continue
		}
		if len(groups) > 0 && !slices.ContainsFunc(groups, func(g []string) bool {
			for _, t := range g {
				if t != "" && !slices.Contains(doc.Tags, t) {
					return false
				}
			}
			return true
		}) {
			continue
		}
		out = append(out, doc.listed(id))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return fatal("%v", err)
	}
	return 0
}

// forget removes the snapshots named by the arguments.
func forget(st store, fl flagSet) int {
	if len(fl.args) == 0 {
		fmt.Println("no policy was specified, no snapshots will be removed")
		return 0
	}
	code := 0
	for _, ref := range fl.args {
		id, err := resolve(st, ref)
		if err == nil {
			err = st.remove("snapshots/" + id)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ignoring %q: %v\n", ref, err)
			code = 1
			continue
		}
		fmt.Printf("removed snapshot %s\n", id[:8])
	}
	return code
}

// diffStat counts the items added or removed by a diff.
type diffStat struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Bytes int64 `json:"bytes"`
}

// count adds n to the stat.
func (s *diffStat) count(n node) {
	if n.Type == "dir" {
		s.Dirs++
	} else {
		s.Files++
		s.Bytes += n.Size
	}
}

// diff reports the differences between two snapshots.
func diff(st store, fl flagSet) int {
	if len(fl.args) != 2 {
		return fatal("specify two snapshot IDs")
	}
	var docs [2]snapshotDoc
	var ids [2]string
	for i, ref := range fl.args {
		id, err := resolve(st, ref)
		if err != nil {
			return fatal("%v", err)
		}
		if docs[i], err = loadDoc(st, id); err != nil {
			return fatal("%v", err)
		}
		ids[i] = id
	}
	a, b := index(docs[0].Nodes), index(docs[1].Nodes)
	var paths []string
	for p := range a {
		paths = append(paths, p)
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.SortFunc(paths, comparePaths)
	enc := json.NewEncoder(os.Stdout)
	var added, removed diffStat
	changed := 0
	for _, p := range paths {
		na, inA := a[p]
		nb, inB := b[p]
		var mod string
		name := p
		switch {
		case !inA:
			mod = "+"
			added.count(nb)
		case !inB:
			mod = "-"
			removed.count(na)
		case na.Type != nb.Type:
			mod = "T"
		case na.Type == "file" && na.Blob != nb.Blob:
			mod = "M"
			changed++
		case fl.bools["--metadata"] && (na.Size != nb.Size || !na.ModTime.Equal(nb.ModTime)):
			mod = "U"
		default:
			continue
		}
		if (inB && nb.Type == "dir") || (!inB && na.Type == "dir") {
			name += "/"
		}
		if fl.json {
			_ = enc.Encode(map[string]string{"message_type": "change", "path": name, "modifier": mod})
		} else {
			fmt.Printf("%-5s%s\n", mod, name)
		}
	}
	if fl.json {
		_ = enc.Encode(map[string]any{
			"message_type": "statistics", "source_snapshot": ids[0], "target_snapshot": ids[1],
			"changed_files": changed, "added": added, "removed": removed,
		})
	}
	return 0
}

// dump writes a file of a snapshot, or a tar archive of a directory.
func dump(st store, fl flagSet) int {
	if d, err := time.ParseDuration(os.Getenv(EnvDumpDelay)); err == nil {
		time.Sleep(d)
	}
	if len(fl.args) != 2 {
		return fatal("bad dump arguments")
	}
	if a := fl.value("--archive"); a != "" && a != "tar" {
		return fatal("the fake restic writes tar archives only, not %q", a)
	}
	id, err := resolve(st, fl.args[0])
	if err != nil {
		return fatal("no matching ID found for prefix %q: %v", fl.args[0], err)
	}
	doc, err := loadDoc(st, id)
	if err != nil {
		return fatal("%v", err)
	}
	p := path.Clean("/" + fl.args[1])
	if p != "/" {
		n, ok := index(doc.Nodes)[p]
		if !ok {
			return fatal("cannot dump file: path %q not found in snapshot", p)
		}
		if n.Type == "file" {
			if err := getBlob(st, n.Blob, os.Stdout); err != nil {
				return fatal("%v", err)
			}
			return 0
		}
	}
	if err := writeTar(st, doc.Nodes, p, os.Stdout); err != nil {
		return fatal("%v", err)
	}
	return 0
}

// writeTar writes a tar archive of the items below the directory dir of
// nodes to w, named like restic does: their paths without the leading
// slash, directories ending in "/".
func writeTar(st store, nodes []node, dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for _, n := range nodes {
		if !strings.HasPrefix(n.Path, prefix) {
			continue
		}
		hdr := &tar.Header{Name: strings.TrimPrefix(n.Path, "/"), ModTime: n.ModTime, Mode: 0o644, Typeflag: tar.TypeReg, Size: n.Size}
		if n.Type == "dir" {
			hdr.Name += "/"
			hdr.Mode, hdr.Typeflag, hdr.Size = 0o755, tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if n.Type == "file" {
			if err := getBlob(st, n.Blob, tw); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}
