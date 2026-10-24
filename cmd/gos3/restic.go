package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// gos3 restic: a bucket's restic repositories from the command line, for
// recovery with or without a running server (docs/encryption.md "Recovery
// without gos3"). Every subcommand first finds the repository and its
// password:
//
//   - online (--server): from the admin API's
//     POST /buckets/{name}/restic/credentials (admin:GetResticCredentials;
//     the server logs the disclosure);
//   - offline (--config, --data or --offline, the server stopped): from the
//     backings' own files, see restic_offline.go.
//
// Secrets (the password, the credentials of s3 repositories) are written
// only where they are asked for: by `env` to standard output, and by
// `exec`, `objects` and `recover` to restic's environment.

// resticUsage is the help of `gos3 restic`.
const resticUsage = `Usage:
  gos3 restic env     --bucket NAME [--snapshots] [--shell bash|powershell|json] [SOURCE]
  gos3 restic exec    --bucket NAME [--snapshots] [SOURCE] -- RESTIC-ARGUMENTS...
  gos3 restic objects --bucket NAME [--key KEY | --prefix PREFIX] [--json] [SOURCE]
  gos3 restic recover --bucket NAME --key KEY [--at TIME] [-o FILE] [SOURCE]

env      prints shell statements setting RESTIC_REPOSITORY, RESTIC_PASSWORD and the
         rest of restic's environment (eval "$(gos3 restic env --bucket NAME)")
exec     runs restic with that environment and the repository's -o options
objects  lists the bucket's per-version archive (one restic snapshot per archived
         version) with the object keys decoded
recover  writes the newest archived version of a key, at or before --at (default:
         the newest), to standard output or -o FILE

The repository is the bucket's per-version archive on its primary backing
(--backing selects another copy), or with --snapshots the repository of its
whole-bucket snapshots.

SOURCE is where the repository and its password come from:
  online, the server running:  --server URL [--access-key KEY --secret-key SECRET]
      (keys default to AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY; the key needs
      admin:GetResticCredentials, and the server logs every disclosure; a plain
      http:// URL of another machine needs --allow-http; the answer may name
      only s3 or local repositories, s3.* options and AWS_* credentials)
  offline, the server stopped: --config FILE, --data DIR or --offline
      (the server's configuration, GOS3_* variables included; read from the
      backings directly)

Run "gos3 restic <command> -h" for every flag.
`

// Purposes of a repository (the admin API's credentials "purpose").
const (
	purposeArchive   = "archive"
	purposeSnapshots = "snapshots"
)

// resticRepo is a repository the CLI found: how restic reaches it (secrets
// included) and what it is.
type resticRepo struct {
	loc     restic.Location
	bucket  string
	backing string // the backing holding it ("" when unknown)
	purpose string // purposeArchive or purposeSnapshots
}

// resticFlags are the flags shared by the restic subcommands.
type resticFlags struct {
	bucket, backing string
	snapshots       bool
	// online
	server, accessKey, secretKey, region string
	insecure, allowHTTP                  bool
	timeout                              time.Duration
	// offline
	configPath, data string
	offline          bool
	bucketID         string
	// restic's cache
	cacheDir string
	noCache  bool
}

// addResticFlags registers the shared flags on fs (--snapshots only with
// snapshots).
func addResticFlags(fs *flag.FlagSet, snapshots bool) *resticFlags {
	f := &resticFlags{}
	fs.StringVar(&f.bucket, "bucket", "", "the bucket (required)")
	fs.StringVar(&f.backing, "backing", "", "the backing of the copy whose archive is used (default: the bucket's primary)")
	if snapshots {
		fs.BoolVar(&f.snapshots, "snapshots", false, "the repository of the bucket's whole-bucket snapshots instead of its per-version archive")
	}
	fs.StringVar(&f.server, "server", "", "online: base URL of the running gos3 server, e.g. https://s3.example.com")
	fs.StringVar(&f.accessKey, "access-key", "", "online: access key allowed admin:GetResticCredentials (default $AWS_ACCESS_KEY_ID)")
	fs.StringVar(&f.secretKey, "secret-key", "", "online: its secret key (default $AWS_SECRET_ACCESS_KEY)")
	fs.StringVar(&f.region, "region", "", "online: signing region (default: the server's, from /_gos3/info, else us-east-1)")
	fs.BoolVar(&f.insecure, "insecure", false, "online: skip TLS certificate verification")
	fs.BoolVar(&f.allowHTTP, "allow-http", false, "online: allow a plain http:// --server that is not on this machine (the secrets cross the network unencrypted)")
	fs.DurationVar(&f.timeout, "timeout", 30*time.Second, "online: timeout of the requests to the server")
	fs.StringVar(&f.configPath, "config", "", "offline: the server's JSON or YAML configuration file (env GOS3_CONFIG)")
	fs.StringVar(&f.data, "data", "", "offline: the server's data directory (storage.path)")
	fs.BoolVar(&f.offline, "offline", false, "offline: read the configuration from GOS3_CONFIG and the GOS3_* variables")
	fs.StringVar(&f.bucketID, "bucket-id", "", "offline: the bucket's ID, when several recovery records name the bucket (it was deleted and created again)")
	fs.StringVar(&f.cacheDir, "cache-dir", "", "restic's cache directory (default: restic's own)")
	fs.BoolVar(&f.noCache, "no-cache", false, "run restic without a cache")
	return f
}

// check validates the shared flags after parsing; errors are usage errors.
func (f *resticFlags) check(fs *flag.FlagSet) error {
	usage := func(format string, args ...any) error {
		fmt.Fprintf(fs.Output(), format+"\n", args...)
		return errUsage
	}
	online := f.server != ""
	offline := f.configPath != "" || f.data != "" || f.offline
	switch {
	case f.bucket == "":
		return usage("--bucket is required")
	case online && offline:
		return usage("--server (online) cannot be combined with --config, --data or --offline (offline)")
	case !online && !offline:
		return usage("say where the repository comes from: --server URL with the server running, or --config FILE (or --data DIR, or --offline) with the server stopped")
	case f.snapshots && f.backing != "":
		return usage("--backing selects a copy's archive; whole-bucket snapshots have one repository (leave it out)")
	case f.bucketID != "" && online:
		return usage("--bucket-id is for offline use (the server knows its buckets)")
	case f.bucketID != "" && !validBucketID(f.bucketID):
		return usage("--bucket-id %q is not a bucket ID", f.bucketID)
	}
	return nil
}

// validBucketID reports whether id can be a backend's bucket ID (a token
// that is safe in file and key names).
var validBucketID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString

// cache returns restic's cache directory for the repositories ("": none).
func (f *resticFlags) cache() string {
	switch {
	case f.noCache:
		return ""
	case f.cacheDir != "":
		return f.cacheDir
	}
	// restic's default: <user cache dir>/restic.
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "restic")
	}
	return ""
}

// resolve finds the repository the flags describe.
func (f *resticFlags) resolve(ctx context.Context, stderr io.Writer) (resticRepo, error) {
	purpose := purposeArchive
	if f.snapshots {
		purpose = purposeSnapshots
	}
	var (
		repo resticRepo
		err  error
	)
	if f.server != "" {
		repo, err = f.online(ctx, purpose)
	} else {
		var src *offlineSource
		if src, err = f.openOffline(stderr); err == nil {
			if f.snapshots {
				repo, err = src.snapshots(ctx, f.bucket)
			} else {
				repo, err = src.archive(ctx, f.bucket, f.backing)
			}
		}
	}
	if err != nil {
		return resticRepo{}, err
	}
	repo.bucket, repo.purpose = f.bucket, purpose
	if repo.loc.CacheDir == "" {
		repo.loc.CacheDir = f.cache()
	}
	return repo, nil
}

// cmdRestic implements `gos3 restic`.
func cmdRestic(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "env":
		return cmdResticEnv(ctx, args, stdout, stderr)
	case "exec":
		return cmdResticExec(ctx, args, stdout, stderr)
	case "objects":
		return cmdResticObjects(ctx, args, stdout, stderr)
	case "recover":
		return cmdResticRecover(ctx, args, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, resticUsage)
		return nil
	default:
		fmt.Fprint(stderr, resticUsage)
		return errUsage
	}
}

// --- env ---

// Shells of `gos3 restic env`.
const (
	shellBash       = "bash"
	shellPowerShell = "powershell"
	shellJSON       = "json"
)

func cmdResticEnv(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("restic env", "gos3 restic env --bucket NAME [--snapshots] [--shell bash|powershell|json] [SOURCE]", stderr)
	f := addResticFlags(fs, true)
	shell := fs.String("shell", shellBash, "output format: bash (also sh, zsh), powershell or json")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	switch *shell {
	case shellBash, "sh", "zsh":
		*shell = shellBash
	case shellPowerShell, "pwsh":
		*shell = shellPowerShell
	case shellJSON:
	default:
		fmt.Fprintf(stderr, "--shell %q: want bash, powershell or json\n", *shell)
		return errUsage
	}
	if err := f.check(fs); err != nil {
		return err
	}
	repo, err := f.resolve(ctx, stderr)
	if err != nil {
		return err
	}
	return writeResticEnv(stdout, *shell, repo)
}

// envVar is one environment variable of restic.
type envVar struct{ name, value string }

// validEnvName matches the environment variable names the shells accept.
var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString

// resticEnvVars returns restic's environment for loc: RESTIC_REPOSITORY,
// RESTIC_PASSWORD, then loc.Env by name. loc must pass Location.Check (so
// that `eval` never sets a variable that runs a program).
func resticEnvVars(loc restic.Location) ([]envVar, error) {
	if err := loc.Check(); err != nil {
		return nil, err
	}
	vars := []envVar{{"RESTIC_REPOSITORY", loc.Repository}, {"RESTIC_PASSWORD", loc.Password}}
	var extra []envVar
	for _, e := range loc.Env {
		name, value, ok := strings.Cut(e, "=")
		if !ok || !validEnvName(name) {
			return nil, fmt.Errorf("the repository's environment has an invalid entry %q", name)
		}
		if name == "RESTIC_REPOSITORY" || name == "RESTIC_PASSWORD" {
			continue
		}
		extra = append(extra, envVar{name, value})
	}
	slices.SortStableFunc(extra, func(a, b envVar) int { return strings.Compare(a.name, b.name) })
	return append(vars, extra...), nil
}

// bashQuote quotes s for a POSIX shell: inside single quotes, where only
// the single quote itself needs care ('\”).
func bashQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powerShellQuote quotes s for PowerShell as ASCII. Printable ASCII goes
// inside single quotes, where only the single quote itself is doubled;
// anything else is sent as the base64 of its UTF-8 and decoded by
// PowerShell. The output must be ASCII: PowerShell decodes a program's
// output with the console's code page (`gos3 restic env | Out-String |
// Invoke-Expression`), not UTF-8, and in some code pages (932, 936) the
// UTF-8 of innocent characters reads as a typographic quote (U+2018-U+201B,
// which PowerShell takes as single quotes), so a value from a malicious
// server would end its quotes and run as code.
func powerShellQuote(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return "[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" +
				base64.StdEncoding.EncodeToString([]byte(s)) + "'))"
		}
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// commentSafe makes s safe in a shell comment (no line breaks or other
// control characters).
func commentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// asciiCommentSafe is commentSafe that also replaces every character
// beyond ASCII with '?', for PowerShell's comments (see powerShellQuote).
func asciiCommentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r >= 0x7f {
			return '?'
		}
		return r
	}, s)
}

// describeRepo names repo for a comment or a message.
func describeRepo(repo resticRepo) string {
	what := "per-version archive"
	if repo.purpose == purposeSnapshots {
		what = "whole-bucket snapshot repository"
	}
	s := fmt.Sprintf("the %s of bucket %q", what, repo.bucket)
	if repo.backing != "" {
		s += fmt.Sprintf(" on backing %q", repo.backing)
	}
	return s
}

// writeResticEnv writes restic's environment for repo in the shell's
// syntax.
func writeResticEnv(w io.Writer, shell string, repo resticRepo) error {
	vars, err := resticEnvVars(repo.loc)
	if err != nil {
		return err
	}
	if shell == shellJSON {
		env := make(map[string]string, len(vars))
		for _, v := range vars {
			env[v.name] = v.value
		}
		opts := repo.loc.Options
		if opts == nil {
			opts = []string{}
		}
		return writeJSONTo(w, struct {
			Bucket  string            `json:"bucket"`
			Backing string            `json:"backing,omitempty"`
			Purpose string            `json:"purpose"`
			Env     map[string]string `json:"env"`
			Options []string          `json:"options"`
		}{repo.bucket, repo.backing, repo.purpose, env, opts})
	}
	comment := commentSafe
	if shell == shellPowerShell {
		comment = asciiCommentSafe // the whole output is ASCII
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# gos3 restic: %s\n", comment(describeRepo(repo)))
	if len(repo.loc.Options) > 0 {
		var o []string
		for _, opt := range repo.loc.Options {
			o = append(o, "-o "+opt)
		}
		fmt.Fprintf(&b, "# restic reads no options from the environment: add %s to every restic command\n", comment(strings.Join(o, " ")))
	}
	for _, v := range vars {
		if shell == shellPowerShell {
			fmt.Fprintf(&b, "$env:%s = %s\n", v.name, powerShellQuote(v.value))
		} else {
			fmt.Fprintf(&b, "export %s=%s\n", v.name, bashQuote(v.value))
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// --- exec ---

// exitCodeError ends the command with an exit code and no message: the
// program it ran (restic) reported the failure already.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func cmdResticExec(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("restic exec", "gos3 restic exec --bucket NAME [--snapshots] [SOURCE] -- RESTIC-ARGUMENTS...", stderr)
	f := addResticFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	rargs := fs.Args()
	if len(rargs) == 0 {
		fmt.Fprintln(stderr, "give restic's arguments after --, e.g. gos3 restic exec --bucket photos --server URL -- snapshots")
		return errUsage
	}
	if err := f.check(fs); err != nil {
		return err
	}
	repo, err := f.resolve(ctx, stderr)
	if err != nil {
		return err
	}
	cmd, err := (&restic.Runtime{}).Command(ctx, repo.loc, rargs...)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return exitCodeError{ee.ExitCode()}
	}
	if err != nil {
		return fmt.Errorf("restic: %w", err)
	}
	return nil
}

// --- objects ---

// archivedVersion is one per-version archive snapshot with its key.
type archivedVersion struct {
	Key      string    `json:"key"`
	Archived time.Time `json:"archived"`
	Snapshot string    `json:"snapshot"`
	ShortID  string    `json:"shortId"`
	// Size is the content's size (-1 when restic recorded no summary).
	Size int64 `json:"size"`
	// KeyTag is the raw tag when it does not decode (Key is then "").
	KeyTag string `json:"keyTag,omitempty"`
}

// archivedVersions lists the per-version archive snapshots of bucket in
// repo, of key when not "", oldest first.
func archivedVersions(ctx context.Context, repo resticRepo, key string) ([]archivedVersion, error) {
	filter := restic.SnapshotFilter{Tags: []string{restic.TagObject, restic.BucketTag(repo.bucket)}}
	if key != "" {
		filter.Tags = append(filter.Tags, restic.KeyTag(key))
	}
	snaps, err := (&restic.Runtime{}).Snapshots(ctx, repo.loc, filter)
	if err != nil {
		return nil, err
	}
	out := make([]archivedVersion, 0, len(snaps))
	for _, s := range snaps {
		v := archivedVersion{Archived: s.Time.UTC(), Snapshot: s.ID, ShortID: s.ShortID, Size: -1}
		if v.ShortID == "" && len(s.ID) >= 8 {
			v.ShortID = s.ID[:8]
		}
		if s.Summary != nil {
			v.Size = s.Summary.TotalBytesProcessed
		}
		for _, t := range s.Tags {
			enc, ok := strings.CutPrefix(t, restic.TagKeyPrefix)
			if !ok {
				continue
			}
			if k, err := base64.RawURLEncoding.DecodeString(enc); err == nil {
				v.Key = string(k)
			} else {
				v.KeyTag = t
			}
			break
		}
		out = append(out, v)
	}
	return out, nil
}

func cmdResticObjects(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("restic objects", "gos3 restic objects --bucket NAME [--key KEY | --prefix PREFIX] [--json] [SOURCE]", stderr)
	f := addResticFlags(fs, false)
	key := fs.String("key", "", "only the versions of this key")
	prefix := fs.String("prefix", "", "only the versions of keys with this prefix")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *key != "" && *prefix != "" {
		fmt.Fprintln(stderr, "--key and --prefix exclude each other")
		return errUsage
	}
	if err := f.check(fs); err != nil {
		return err
	}
	repo, err := f.resolve(ctx, stderr)
	if err != nil {
		return err
	}
	versions, err := archivedVersions(ctx, repo, *key)
	if err != nil {
		return err
	}
	list := versions[:0]
	for _, v := range versions {
		if strings.HasPrefix(v.Key, *prefix) {
			list = append(list, v)
		}
	}
	// By key, then oldest first.
	slices.SortStableFunc(list, func(a, b archivedVersion) int {
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return a.Archived.Compare(b.Archived)
	})
	if *asJSON {
		return writeJSONTo(stdout, list)
	}
	if len(list) == 0 {
		fmt.Fprintf(stderr, "no archived versions in %s\n", describeRepo(repo))
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ARCHIVED\tSNAPSHOT\tSIZE\tKEY")
	for _, v := range list {
		size := "-"
		if v.Size >= 0 {
			size = fmt.Sprint(v.Size)
		}
		k := v.Key
		if v.KeyTag != "" {
			k = "(undecodable tag " + v.KeyTag + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.Archived.Format(time.RFC3339), v.ShortID, size, commentSafe(k))
	}
	return tw.Flush()
}

// --- recover ---

func cmdResticRecover(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("restic recover", "gos3 restic recover --bucket NAME --key KEY [--at TIME] [-o FILE] [SOURCE]", stderr)
	f := addResticFlags(fs, false)
	key := fs.String("key", "", "the object key (required)")
	atFlag := fs.String("at", "", "the newest version archived at or before this time (RFC 3339, e.g. 2026-10-01T12:00:00Z; default: the newest)")
	out := fs.String("o", "", "write to FILE instead of standard output (replaced only once the content is complete)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *key == "" {
		fmt.Fprintln(stderr, "--key is required")
		return errUsage
	}
	at := time.Time{}
	if *atFlag != "" {
		t, err := time.Parse(time.RFC3339Nano, *atFlag)
		if err != nil {
			fmt.Fprintf(stderr, "--at %q: want an RFC 3339 time such as 2026-10-01T12:00:00Z\n", *atFlag)
			return errUsage
		}
		at = t
	}
	if err := f.check(fs); err != nil {
		return err
	}
	repo, err := f.resolve(ctx, stderr)
	if err != nil {
		return err
	}
	versions, err := archivedVersions(ctx, repo, *key)
	if err != nil {
		return err
	}
	var pick *archivedVersion
	for i := range versions { // oldest first
		if v := &versions[i]; v.Key == *key && (at.IsZero() || !v.Archived.After(at)) {
			pick = v
		}
	}
	if pick == nil {
		when := ""
		if !at.IsZero() {
			when = " at or before " + at.UTC().Format(time.RFC3339)
		}
		return fmt.Errorf("no version of key %q archived%s in %s (empty objects are never archived)", *key, when, describeRepo(repo))
	}
	rt := &restic.Runtime{}
	if *out == "" {
		if err := rt.DumpFile(ctx, repo.loc, pick.Snapshot, "/object", stdout); err != nil {
			return err
		}
	} else if err := dumpToFile(ctx, rt, repo.loc, pick.Snapshot, *out); err != nil {
		return err
	}
	dest := *out
	if dest == "" {
		dest = "standard output"
	}
	fmt.Fprintf(stderr, "recovered %q archived %s (snapshot %s) to %s\n", *key, pick.Archived.Format(time.RFC3339), pick.ShortID, dest)
	return nil
}

// dumpToFile writes /object of snapshot to path through a temporary file
// in the same directory, renamed into place once complete.
func dumpToFile(ctx context.Context, rt *restic.Runtime, loc restic.Location, snapshot, path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".gos3-*")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := rt.DumpFile(ctx, loc, snapshot, "/object", tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	return nil
}

// --- online ---

// resticCredentials is the admin API's ResticCredentials document.
type resticCredentials struct {
	Backing    string            `json:"backing"`
	Purpose    string            `json:"purpose"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Env        map[string]string `json:"env"`
	Options    []string          `json:"options"`
}

// adminPrefix is the admin API's path prefix.
const adminPrefix = "/_gos3/admin/v1"

// online asks the server for the repository's credentials.
func (f *resticFlags) online(ctx context.Context, purpose string) (resticRepo, error) {
	base := strings.TrimRight(f.server, "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		return resticRepo{}, fmt.Errorf("--server %q: want the server's base URL, e.g. https://s3.example.com:9000", f.server)
	}
	if u.Scheme == "http" && !f.allowHTTP && !loopbackHost(u.Hostname()) {
		return resticRepo{}, fmt.Errorf("--server %q uses plain HTTP: the repository's password and the backing's S3 keys would cross the network unencrypted, "+
			"and anyone on the way could change the answer; use https://, or --allow-http to accept that", f.server)
	}
	creds := auth.Credentials{AccessKey: f.accessKey, SecretKey: f.secretKey}
	if creds.AccessKey == "" {
		creds.AccessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if creds.SecretKey == "" {
		creds.SecretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return resticRepo{}, errors.New("online use needs an access key allowed admin:GetResticCredentials: --access-key and --secret-key, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	client := httpClient(f.insecure)
	region := f.region
	if region == "" {
		region = serverRegion(ctx, client, base)
	}
	body, _ := json.Marshal(struct {
		Backing string `json:"backing,omitempty"`
		Purpose string `json:"purpose"`
	}{f.backing, purpose})
	endpoint := base + adminPrefix + "/buckets/" + url.PathEscape(f.bucket) + "/restic/credentials"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return resticRepo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := auth.SignRequest(req, creds, region, "s3", "", time.Now()); err != nil {
		return resticRepo{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return resticRepo{}, fmt.Errorf("asking %s for the credentials: %w", base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resticRepo{}, fmt.Errorf("reading the server's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resticRepo{}, apiError(resp.StatusCode, data)
	}
	var c resticCredentials
	if err := json.Unmarshal(data, &c); err != nil || c.Repository == "" {
		return resticRepo{}, fmt.Errorf("the server's answer is not a ResticCredentials document (is %s a gos3 server?)", base)
	}
	loc := restic.Location{Repository: c.Repository, Password: c.Password, Options: c.Options}
	for name, value := range c.Env {
		loc.Env = append(loc.Env, name+"="+value)
	}
	slices.Sort(loc.Env)
	// The answer is run by restic (and eval'ed after `env`): accept only
	// what gos3's own repositories use, never a backend, option or
	// variable that makes restic or a shell run a program.
	if err := loc.Check(); err != nil {
		return resticRepo{}, fmt.Errorf("refusing the server's answer: %w", err)
	}
	return resticRepo{loc: loc, backing: c.Backing}, nil
}

// loopbackHost reports whether host (a URL's hostname) is this machine:
// "localhost" or a loopback address.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// apiError describes an error answer of the admin API.
func apiError(status int, body []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("server: %s (%s, HTTP %d)", commentSafe(e.Error.Message), commentSafe(e.Error.Code), status)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return fmt.Errorf("server: HTTP %d %s", status, commentSafe(msg))
}

// httpClient returns the client of the online requests.
func httpClient(insecure bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in
	}
	return &http.Client{Transport: tr}
}

// serverRegion returns the region the server at base reports in
// /_gos3/info (signatures must use it with auth.strict_region), or
// us-east-1 when it does not tell.
func serverRegion(ctx context.Context, client *http.Client, base string) string {
	const fallback = "us-east-1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/_gos3/info", nil)
	if err != nil {
		return fallback
	}
	resp, err := client.Do(req)
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	var info struct {
		Region string `json:"region"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil || info.Region == "" {
		return fallback
	}
	return info.Region
}
