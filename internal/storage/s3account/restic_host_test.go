package s3account

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

func TestMain(m *testing.M) { restictest.Main(m) }

// TestResticHostDispatch: a wrapped bucket's repository is its
// container's, a native bucket keeps none, and aux repositories live in
// the home.
func TestResticHostDispatch(t *testing.T) {
	restictest.Install(t, "s3account-host")
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	o := homeOptions(t, r, NewClaims(), "home", "")
	o.Conn.KEK = bytes.Repeat([]byte{7}, 32)
	b := openReady(t, o)
	ctx := context.Background()
	noErr(t, "create wrapped", b.CreateBucket(ctx, "wr", storage.CreateBucketOptions{}))
	noErr(t, "create native", b.CreateBucket(ctx, "nat", storage.CreateBucketOptions{Target: nativeTarget("", false)}))

	info, loc, err := b.ResticRepository(ctx, "wr", true)
	noErr(t, "ResticRepository(wrapped)", err)
	cinfo, cloc, err := b.home.ResticRepository(ctx, "wr", false)
	noErr(t, "container's ResticRepository", err)
	if info.BucketID == "" || info.BucketID != cinfo.BucketID || info.Repository != "s3:"+r.URL+"/home/restic/"+info.BucketID ||
		!info.Initialized || loc.Password != cloc.Password || loc.Password == "" {
		t.Fatalf("wrapped bucket's repository = %+v, container's %+v", info, cinfo)
	}
	_, _, err = b.ResticRepository(ctx, "nat", true)
	wantErr(t, "ResticRepository(native)", err, errors.ErrUnsupported)
	_, _, err = b.ResticRepository(ctx, "nope", false)
	wantErr(t, "ResticRepository(unknown)", err, storage.ErrBucketNotFound)

	ainfo, aloc, err := b.ResticAuxRepository(ctx, "snap", true)
	noErr(t, "ResticAuxRepository", err)
	hinfo, hloc, err := b.home.ResticAuxRepository(ctx, "snap", false)
	noErr(t, "home's ResticAuxRepository", err)
	if ainfo.Repository != "s3:"+r.URL+"/home/restic-aux/snap" || ainfo.Repository != hinfo.Repository ||
		!hinfo.Initialized || aloc.Password != hloc.Password {
		t.Fatalf("aux repository = %+v, home's %+v", ainfo, hinfo)
	}
	_, _, err = b.ResticAuxRepository(ctx, "Bad", true)
	wantErr(t, "invalid aux name", err, storage.ErrInvalidName)
}

// TestResticHostUnsupported: without a home (records kept locally) there
// is no aux repository, and without an encryption key neither aux nor
// bucket repositories.
func TestResticHostUnsupported(t *testing.T) {
	restictest.Install(t, "s3account-host")
	ctx := context.Background()
	r := s3clienttest.Start(t)
	local := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	_, _, err := local.ResticAuxRepository(ctx, "snap", true)
	wantErr(t, "aux repository without a home", err, errors.ErrUnsupported)

	r2 := s3clienttest.Start(t)
	r2.CreateBucket(t, "home")
	noKEK := openReady(t, homeOptions(t, r2, NewClaims(), "home", ""))
	noErr(t, "create wrapped", noKEK.CreateBucket(ctx, "wr", storage.CreateBucketOptions{}))
	_, _, err = noKEK.ResticAuxRepository(ctx, "snap", true)
	wantErr(t, "aux repository without a KEK", err, errors.ErrUnsupported)
	_, _, err = noKEK.ResticRepository(ctx, "wr", true)
	wantErr(t, "bucket repository without a KEK", err, errors.ErrUnsupported)
}
