package restictest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// errNotFound is returned by a store for a missing file.
var errNotFound = errors.New("not found")

// store holds the files of one fake repository.
type store interface {
	// create prepares an empty repository (directory or bucket).
	create() error
	// get copies the file name to w.
	get(name string, w io.Writer) error
	// exists reports whether the file name exists.
	exists(name string) (bool, error)
	// put stores the size bytes of r as the file name.
	put(name string, r io.ReaderAt, size int64) error
	// list returns the names of the files in the directory name.
	list(name string) ([]string, error)
	// remove deletes the file name.
	remove(name string) error
}

// localStore is a repository in a local directory.
type localStore struct{ root string }

func (s localStore) create() error {
	return os.MkdirAll(filepath.Join(s.root, "snapshots"), 0o700)
}

func (s localStore) get(name string, w io.Writer) error {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (s localStore) exists(name string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s localStore) put(name string, r io.ReaderAt, size int64) error {
	dst := filepath.Join(s.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "incoming-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.NewSectionReader(r, 0, size)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (s localStore) list(name string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, err
}

func (s localStore) remove(name string) error {
	err := os.Remove(filepath.Join(s.root, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return errNotFound
	}
	return err
}

// s3Store is a repository in restic's s3 backend.
type s3Store struct {
	ctx    context.Context
	c      *s3client.Client
	bucket string
	prefix string // "" or ending in "/"
}

// newS3Store returns the store of the repository spec (the part after
// "s3:") with restic's extended options opts.
func newS3Store(spec string, opts map[string]string) (*s3Store, error) {
	scheme := "https://"
	for _, p := range []string{"http://", "https://"} {
		if strings.HasPrefix(spec, p) {
			scheme, spec = p, strings.TrimPrefix(spec, p)
		}
	}
	host, rest, _ := strings.Cut(spec, "/")
	bucket, prefix, _ := strings.Cut(rest, "/")
	if host == "" || bucket == "" {
		return nil, fmt.Errorf("invalid s3 repository %q", spec)
	}
	if prefix = strings.Trim(prefix, "/"); prefix != "" {
		prefix += "/"
	}
	region := opts["s3.region"]
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	var pathStyle bool
	switch lookup := opts["s3.bucket-lookup"]; lookup {
	case "path":
		pathStyle = true
	case "dns":
	case "", "auto":
		pathStyle = !strings.HasSuffix(strings.Split(host, ":")[0], "amazonaws.com")
	default:
		return nil, fmt.Errorf("bad bucket-lookup style %q", lookup)
	}
	c, err := s3client.New(s3client.Config{
		Endpoint:    scheme + host,
		Region:      region,
		AccessKey:   os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:   os.Getenv("AWS_SECRET_ACCESS_KEY"),
		PathStyle:   pathStyle,
		MaxAttempts: 2,
	})
	if err != nil {
		return nil, err
	}
	return &s3Store{ctx: context.Background(), c: c, bucket: bucket, prefix: prefix}, nil
}

func (s *s3Store) create() error {
	err := s.c.HeadBucket(s.ctx, s.bucket)
	if s3client.IsNotFound(err) {
		return s.c.CreateBucket(s.ctx, s.bucket)
	}
	return err
}

func (s *s3Store) get(name string, w io.Writer) error {
	body, _, err := s.c.GetObject(s.ctx, s.bucket, s.prefix+name, s3client.GetOptions{})
	if s3client.IsNotFound(err) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = io.Copy(w, body)
	return err
}

func (s *s3Store) exists(name string) (bool, error) {
	_, err := s.c.HeadObject(s.ctx, s.bucket, s.prefix+name)
	if s3client.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *s3Store) put(name string, r io.ReaderAt, size int64) error {
	_, err := s.c.PutObject(s.ctx, s.bucket, s.prefix+name, r, size, s3client.PutOptions{})
	return err
}

func (s *s3Store) list(name string) ([]string, error) {
	var names []string
	dir := s.prefix + name + "/"
	err := s.c.Walk(s.ctx, s.bucket, dir, "", func(o s3client.ObjectAttrs) error {
		names = append(names, path.Base(o.Key))
		return nil
	})
	return names, err
}

func (s *s3Store) remove(name string) error {
	if ok, err := s.exists(name); err != nil {
		return err
	} else if !ok {
		return errNotFound
	}
	return s.c.DeleteObject(s.ctx, s.bucket, s.prefix+name)
}
