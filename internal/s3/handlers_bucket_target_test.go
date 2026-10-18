package s3

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// targetBackend is a memory backend standing in for a backing with bucket
// kinds: it records the CreateBucketOptions it is given, reports the kind
// of the target a bucket was created with from GetBucket, fails
// CreateBucket of the buckets listed in fail, and fails object reads and
// deletes of the keys listed in objFail (any bucket).
type targetBackend struct {
	*memory.Backend

	mu      sync.Mutex
	created map[string]storage.CreateBucketOptions
	kinds   map[string]storage.BucketKind
	fail    map[string]error
	objFail map[string]error
}

// objErr returns the configured failure of key.
func (b *targetBackend) objErr(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.objFail[key]
}

func (b *targetBackend) GetObjectInfo(ctx context.Context, bucket, key, vid string) (storage.ObjectInfo, error) {
	if err := b.objErr(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	return b.Backend.GetObjectInfo(ctx, bucket, key, vid)
}

func (b *targetBackend) OpenObject(ctx context.Context, bucket, key, vid string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if err := b.objErr(key); err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	return b.Backend.OpenObject(ctx, bucket, key, vid)
}

func (b *targetBackend) DeleteObject(ctx context.Context, bucket, key, vid string) (storage.DeleteResult, error) {
	if err := b.objErr(key); err != nil {
		return storage.DeleteResult{}, err
	}
	return b.Backend.DeleteObject(ctx, bucket, key, vid)
}

func (b *targetBackend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	b.mu.Lock()
	b.created[name] = opts
	err := b.fail[name]
	if err == nil && opts.Target != nil {
		b.kinds[name] = opts.Target.Kind
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return b.Backend.CreateBucket(ctx, name, opts)
}

func (b *targetBackend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	bi, err := b.Backend.GetBucket(ctx, name)
	if err == nil {
		b.mu.Lock()
		bi.Kind = b.kinds[name]
		b.mu.Unlock()
	}
	return bi, err
}

// createdWith returns the options CreateBucket of name was called with.
func (b *targetBackend) createdWith(name string) (storage.CreateBucketOptions, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.created[name]
	return o, ok
}

// newTargetEnv is a test env whose server uses a targetBackend over
// e.backend.
func newTargetEnv(t *testing.T) (*testEnv, *targetBackend) {
	tb := &targetBackend{
		created: map[string]storage.CreateBucketOptions{},
		kinds:   map[string]storage.BucketKind{},
		fail:    map[string]error{},
		objFail: map[string]error{},
	}
	e := newTestEnv(t, withOptions(func(o *Options) {
		tb.Backend = o.Backend.(*memory.Backend)
		o.Backend = tb
	}))
	return e, tb
}

// argumentError is the part of an InvalidArgument error body the tests
// check.
type argumentError struct {
	Code          string
	Message       string
	ArgumentName  string
	ArgumentValue string
	BucketName    string
}

func parseArgumentError(t *testing.T, data []byte) argumentError {
	t.Helper()
	var doc argumentError
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("error body %q: %v", data, err)
	}
	return doc
}

// TestCreateBucketTarget covers the x-gos3-bucket-kind,
// x-gos3-remote-bucket and x-gos3-container headers of CreateBucket: the
// BucketTarget they build reaches the backend, bad values and combinations
// are InvalidArgument (and create nothing), and HeadBucket reports the kind.
func TestCreateBucketTarget(t *testing.T) {
	e, tb := newTargetEnv(t)
	native, wrapped := storage.BucketKindNative, storage.BucketKindWrapped
	tests := []struct {
		name      string
		hdr       []string // key/value pairs
		want      *storage.BucketTarget
		argName   string // InvalidArgument: the ArgumentName
		argValue  string
		wantInMsg string
	}{
		{name: "no headers"},
		{name: "native kind", hdr: []string{"X-Gos3-Bucket-Kind", "native"}, want: &storage.BucketTarget{Kind: native}},
		{name: "wrapped kind any case", hdr: []string{"X-Gos3-Bucket-Kind", " Wrapped "}, want: &storage.BucketTarget{Kind: wrapped}},
		{name: "remote implies native", hdr: []string{"X-Gos3-Remote-Bucket", "remote-photos"},
			want: &storage.BucketTarget{Kind: native, RemoteBucket: "remote-photos"}},
		{name: "native with remote", hdr: []string{"X-Gos3-Bucket-Kind", "native", "X-Gos3-Remote-Bucket", "remote.photos"},
			want: &storage.BucketTarget{Kind: native, RemoteBucket: "remote.photos"}},
		{name: "home container implies wrapped", hdr: []string{"X-Gos3-Container", "home"},
			want: &storage.BucketTarget{Kind: wrapped, Container: "home"}},
		{name: "wrapped with container", hdr: []string{"X-Gos3-Bucket-Kind", "wrapped", "X-Gos3-Container", " C-1 "},
			want: &storage.BucketTarget{Kind: wrapped, Container: "c-1"}},
		{name: "longest container", hdr: []string{"X-Gos3-Container", strings.Repeat("c", 63)},
			want: &storage.BucketTarget{Kind: wrapped, Container: strings.Repeat("c", 63)}},

		{name: "unknown kind", hdr: []string{"X-Gos3-Bucket-Kind", "Gateway"},
			argName: "x-gos3-bucket-kind", argValue: "Gateway", wantInMsg: "native"},
		{name: "remote upper case", hdr: []string{"X-Gos3-Remote-Bucket", "Photos"},
			argName: "x-gos3-remote-bucket", argValue: "Photos", wantInMsg: "bucket name"},
		{name: "remote too short", hdr: []string{"X-Gos3-Remote-Bucket", "ab"},
			argName: "x-gos3-remote-bucket", argValue: "ab"},
		{name: "remote ip", hdr: []string{"X-Gos3-Bucket-Kind", "native", "X-Gos3-Remote-Bucket", "192.168.1.1"},
			argName: "x-gos3-remote-bucket", argValue: "192.168.1.1"},
		{name: "container leading dash", hdr: []string{"X-Gos3-Container", "-c"},
			argName: "x-gos3-container", argValue: "-c"},
		{name: "container too long", hdr: []string{"X-Gos3-Container", strings.Repeat("c", 64)},
			argName: "x-gos3-container", argValue: strings.Repeat("c", 64)},
		{name: "container bad char", hdr: []string{"X-Gos3-Container", "c_1"},
			argName: "x-gos3-container", argValue: "c_1"},
		{name: "remote and container", hdr: []string{"X-Gos3-Remote-Bucket", "remote-photos", "X-Gos3-Container", "home"},
			argName: "x-gos3-container", argValue: "home", wantInMsg: "cannot be combined"},
		{name: "wrapped with remote", hdr: []string{"X-Gos3-Bucket-Kind", "wrapped", "X-Gos3-Remote-Bucket", "remote-photos"},
			argName: "x-gos3-remote-bucket", argValue: "remote-photos", wantInMsg: "native"},
		{name: "native with container", hdr: []string{"X-Gos3-Bucket-Kind", "native", "X-Gos3-Container", "home"},
			argName: "x-gos3-container", argValue: "home", wantInMsg: "wrapped"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bucket := fmt.Sprintf("target-%d", i)
			resp := e.signed(http.MethodPut, "/"+bucket, nil, objHdr(tc.hdr...))
			if tc.argName != "" {
				bktExpect(t, resp, 400, "InvalidArgument")
				doc := parseArgumentError(t, resp.Data)
				if doc.ArgumentName != tc.argName || doc.ArgumentValue != tc.argValue {
					t.Errorf("argument %q=%q, want %q=%q", doc.ArgumentName, doc.ArgumentValue, tc.argName, tc.argValue)
				}
				if !strings.Contains(doc.Message, tc.wantInMsg) {
					t.Errorf("message %q does not mention %q", doc.Message, tc.wantInMsg)
				}
				if _, called := tb.createdWith(bucket); called {
					t.Error("the backend was asked to create the bucket")
				}
				if head := e.signed(http.MethodHead, "/"+bucket, nil, nil); head.StatusCode != 404 {
					t.Errorf("HEAD after refusal: %d", head.StatusCode)
				}
				return
			}
			bktExpect(t, resp, 200, "")
			opts, called := tb.createdWith(bucket)
			if !called {
				t.Fatal("the backend was not asked to create the bucket")
			}
			switch {
			case tc.want == nil && opts.Target != nil:
				t.Errorf("target = %+v, want nil", *opts.Target)
			case tc.want != nil && (opts.Target == nil || *opts.Target != *tc.want):
				t.Errorf("target = %+v, want %+v", opts.Target, *tc.want)
			}
			if opts.Target != nil && opts.Target.Adopt {
				t.Error("CreateBucket asked to adopt a remote bucket")
			}
			head := e.signed(http.MethodHead, "/"+bucket, nil, nil)
			bktExpect(t, head, 200, "")
			wantKind := ""
			if tc.want != nil {
				wantKind = string(tc.want.Kind)
			}
			if got, ok := head.Header["X-Gos3-Bucket-Kind"]; wantKind == "" && ok {
				t.Errorf("x-gos3-bucket-kind sent for a bucket without kind: %q", got)
			} else if head.Header.Get("X-Gos3-Bucket-Kind") != wantKind {
				t.Errorf("x-gos3-bucket-kind = %q, want %q", head.Header.Get("X-Gos3-Bucket-Kind"), wantKind)
			}
		})
	}
}

// TestCreateBucketTargetErrors covers how CreateBucket reports the storage
// errors of backings with bucket kinds.
func TestCreateBucketTargetErrors(t *testing.T) {
	e, tb := newTargetEnv(t)
	tests := []struct {
		bucket string
		err    error
		status int
		code   string
		msg    string // the exact message ("" = not checked)
	}{
		{"remote-exists", fmt.Errorf("native: attach: %w", storage.ErrRemoteBucketExists), 409, "BucketAlreadyExists",
			"The remote bucket already exists; an administrator can adopt it."},
		{"bad-target", fmt.Errorf("%w: unknown container %q", storage.ErrInvalidTarget, "c-9"), 400, "InvalidArgument",
			"The requested bucket kind, container or remote bucket cannot be used."},
		{"unavailable", fmt.Errorf("s3account: container c-1: %w", storage.ErrBucketUnavailable), 503, "ServiceUnavailable", ""},
		{"denied", fmt.Errorf("%w: %w", storage.ErrRemoteDenied,
			&s3client.Error{Op: "CreateBucket", StatusCode: 403, Code: "AccessDenied", Message: "Access Denied"}), 403, "AccessDenied",
			"The remote storage service refused the request."},
		{"rejected", fmt.Errorf("%w: %w", storage.ErrRemoteRejected,
			&s3client.Error{Op: "CreateBucket", StatusCode: 400, Code: "InvalidLocationConstraint", Message: "nope"}), 500, "InternalError",
			"The remote storage service rejected the request (InvalidLocationConstraint)."},
	}
	for _, tc := range tests {
		tb.fail[tc.bucket] = tc.err
	}
	for _, tc := range tests {
		t.Run(tc.bucket, func(t *testing.T) {
			resp := e.signed(http.MethodPut, "/"+tc.bucket, nil, objHdr("X-Gos3-Bucket-Kind", "native"))
			bktExpect(t, resp, tc.status, tc.code)
			doc := parseArgumentError(t, resp.Data)
			if tc.msg != "" && doc.Message != tc.msg {
				t.Errorf("message %q, want %q", doc.Message, tc.msg)
			}
			if tc.code == "BucketAlreadyExists" && doc.BucketName != tc.bucket {
				t.Errorf("BucketName = %q, want %q", doc.BucketName, tc.bucket)
			}
		})
	}
	// An existing gos3 bucket is still BucketAlreadyOwnedByYou for its owner.
	bktExpect(t, e.signed(http.MethodPut, "/mine", nil, nil), 200, "")
	bktExpect(t, e.signed(http.MethodPut, "/mine", nil, objHdr("X-Gos3-Bucket-Kind", "native")), 409, "BucketAlreadyOwnedByYou")
}

// TestRemoteErrorMapping checks mapStorageError for the storage errors of
// backings with bucket kinds: their wrapping order, and fixed messages
// that carry nothing of the storage error but a plain remote error code.
func TestRemoteErrorMapping(t *testing.T) {
	remote := func(code string) error {
		return &s3client.Error{Op: "PutObject", StatusCode: 400, Code: code,
			Message: "User: arn:aws:iam::123456789012:user/backing is not authorized"}
	}
	const (
		denied   = "The remote storage service refused the request."
		rejected = "The remote storage service rejected the request."
		target   = "The requested bucket kind, container or remote bucket cannot be used."
	)
	for _, tc := range []struct {
		err    error
		code   string
		status int
		msg    string // the exact message, when set
	}{
		{err: storage.ErrRemoteBucketExists, code: "BucketAlreadyExists", status: 409,
			msg: "The remote bucket already exists; an administrator can adopt it."},
		{err: fmt.Errorf("wrapped: %w", storage.ErrRemoteBucketExists), code: "BucketAlreadyExists", status: 409},
		{err: storage.ErrBucketExists, code: "BucketAlreadyOwnedByYou", status: 409},
		{err: storage.ErrInvalidTarget, code: "InvalidArgument", status: 400, msg: target},
		{err: fmt.Errorf("%w: remote bucket %q is used by backing %q", storage.ErrInvalidTarget, "secret", "cold"),
			code: "InvalidArgument", status: 400, msg: target},
		{err: storage.ErrBucketUnavailable, code: "ServiceUnavailable", status: 503},
		{err: fmt.Errorf("native: %w", storage.ErrBucketUnavailable), code: "ServiceUnavailable", status: 503},
		{err: storage.ErrRemoteDenied, code: "AccessDenied", status: 403, msg: denied},
		{err: fmt.Errorf("native: delete %q: %w: %w", "k", storage.ErrRemoteDenied, remote("AccessDenied")),
			code: "AccessDenied", status: 403, msg: denied},
		{err: storage.ErrRemoteRejected, code: "InternalError", status: 500, msg: rejected},
		{err: fmt.Errorf("%w: %w", storage.ErrRemoteRejected, remote("InvalidRequest")), code: "InternalError", status: 500,
			msg: "The remote storage service rejected the request (InvalidRequest)."},
		{err: fmt.Errorf("native: put: %w: %w", storage.ErrRemoteRejected, fmt.Errorf("retry 2: %w", remote("SlowDown"))),
			code: "InternalError", status: 500, msg: "The remote storage service rejected the request (SlowDown)."},
		// Codes that are not plain letter tokens are left out.
		{err: fmt.Errorf("%w: %w", storage.ErrRemoteRejected, remote("Invalid Request <x>")), code: "InternalError", status: 500, msg: rejected},
		{err: fmt.Errorf("%w: %w", storage.ErrRemoteRejected, remote("Code123")), code: "InternalError", status: 500, msg: rejected},
		{err: fmt.Errorf("%w: %w", storage.ErrRemoteRejected, remote(strings.Repeat("A", 65))), code: "InternalError", status: 500, msg: rejected},
		{err: fmt.Errorf("%w: HTTP 400 InvalidRequest: arn:aws:iam::123456789012:root", storage.ErrRemoteRejected),
			code: "InternalError", status: 500, msg: rejected},
	} {
		mapped := mapStorageError(tc.err)
		var se *s3err.Error
		if !errors.As(mapped, &se) || se.Code != tc.code || se.HTTPStatus != tc.status {
			t.Errorf("%v → %v, want %d %s", tc.err, mapped, tc.status, tc.code)
			continue
		}
		if tc.msg != "" && se.Message != tc.msg {
			t.Errorf("%v: message %q, want %q", tc.err, se.Message, tc.msg)
		}
		if strings.Contains(se.Message, "123456789012") || strings.Contains(se.Message, "secret") {
			t.Errorf("%v: message %q discloses the storage error", tc.err, se.Message)
		}
		// The storage error stays in the chain for the server log, and
		// mapping again keeps it.
		for _, m := range []error{mapped, mapStorageError(mapped), bucketError(mapped, "b")} {
			if errors.Is(tc.err, storage.ErrRemoteDenied) && (!errors.Is(m, storage.ErrRemoteDenied) || !strings.Contains(m.Error(), tc.err.Error())) {
				t.Errorf("%v: mapped %v lost the storage error", tc.err, m)
			}
		}
	}
}

// TestRemoteErrorDetailsNotDisclosed: what a bucket's backing or its remote
// service says about a failure (backing names, remote buckets, the remote
// message with account IDs and ARNs) never reaches S3 clients, anonymous
// ones included; the server log has it, with the request ID.
func TestRemoteErrorDetailsNotDisclosed(t *testing.T) {
	e, tb := newTargetEnv(t)
	const (
		account = "123456789012"
		arn     = "arn:aws:iam::" + account + ":user/gos3-backing-key"
	)
	secrets := []string{account, "gos3-backing-key", "secret-remote", "backing-cold", "other-gos3-bucket", "not authorized"}
	remote := func(code string) error {
		return &s3client.Error{Op: "PutObject", StatusCode: 403, Code: code, RequestID: "REMOTEREQ",
			Message: "User: " + arn + " is not authorized to perform: s3:PutObject on resource: arn:aws:s3:::secret-remote/k"}
	}
	tb.fail["denied-create"] = fmt.Errorf("native: create %q on backing %q: %w: %w", "secret-remote", "backing-cold", storage.ErrRemoteDenied, remote("AccessDenied"))
	tb.fail["claimed"] = fmt.Errorf("%w: remote bucket %q is used by bucket %q of backing %q", storage.ErrInvalidTarget, "secret-remote", "other-gos3-bucket", "backing-cold")
	tb.fail["rejected-create"] = fmt.Errorf("native: %q: %w: %w", "secret-remote", storage.ErrRemoteRejected, remote("InvalidRequest"))
	e.createBucket("pub")
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::pub/*"]}]}`)
	tb.objFail["denied"] = fmt.Errorf("native: get %q from %q: %w: %w", "denied", "secret-remote", storage.ErrRemoteDenied, remote("AccessDenied"))
	tb.objFail["rejected"] = fmt.Errorf("native: get from %q: %w: %w", "secret-remote", storage.ErrRemoteRejected, remote("Odd Code: "+arn))

	body := `<Delete><Object><Key>denied</Key></Object></Delete>`
	sum := md5.Sum([]byte(body))
	for _, tc := range []struct {
		name   string
		resp   *testResponse
		status int
		code   string
	}{
		{"create denied", e.signed(http.MethodPut, "/denied-create", nil, objHdr("X-Gos3-Remote-Bucket", "secret-remote")), 403, "AccessDenied"},
		{"create claimed", e.signed(http.MethodPut, "/claimed", nil, objHdr("X-Gos3-Bucket-Kind", "native")), 400, "InvalidArgument"},
		{"create rejected", e.signed(http.MethodPut, "/rejected-create", nil, objHdr("X-Gos3-Bucket-Kind", "native")), 500, "InternalError"},
		{"anonymous get denied", e.anon(http.MethodGet, "/pub/denied", nil, nil), 403, "AccessDenied"},
		{"anonymous get rejected", e.anon(http.MethodGet, "/pub/rejected", nil, nil), 500, "InternalError"},
		{"delete denied", e.signed(http.MethodDelete, "/pub/denied", nil, nil), 403, "AccessDenied"},
		{"delete objects denied", e.signed(http.MethodPost, "/pub?delete", []byte(body),
			objHdr("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))), 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.resp.StatusCode != tc.status || tc.resp.code() != tc.code {
				t.Fatalf("got %d %q, want %d %q: %s", tc.resp.StatusCode, tc.resp.code(), tc.status, tc.code, tc.resp.Data)
			}
			if tc.code == "" && !strings.Contains(string(tc.resp.Data), "<Code>AccessDenied</Code>") {
				t.Errorf("DeleteObjects result: %s", tc.resp.Data)
			}
			for _, s := range secrets {
				if strings.Contains(string(tc.resp.Data), s) {
					t.Errorf("response discloses %q: %s", s, tc.resp.Data)
				}
				for k, vs := range tc.resp.Header {
					if strings.Contains(strings.Join(vs, " "), s) {
						t.Errorf("response header %s discloses %q", k, s)
					}
				}
			}
			// The operator can find the details by request ID.
			id := tc.resp.Header.Get("X-Amz-Request-Id")
			found := false
			for _, line := range strings.Split(e.logs.String(), "\n") {
				if strings.Contains(line, `"request_id":"`+id+`"`) && strings.Contains(line, "secret-remote") {
					found = true
					break
				}
			}
			if id == "" || !found {
				t.Errorf("no log line of request %q has the error details", id)
			}
		})
	}
}
