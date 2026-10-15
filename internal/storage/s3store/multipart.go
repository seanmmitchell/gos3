package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/index"
)

// CreateMultipartUpload starts a multipart upload of bucket/key.
func (b *Backend) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	if b.opts.Gateway {
		return storage.UploadInfo{}, errGateway
	}
	if err := index.ValidKey(key); err != nil {
		return storage.UploadInfo{}, err
	}
	bk, err := b.lockBucket(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	u := bk.ix.AddUpload(storage.UploadInfo{
		UploadID:  bk.ix.NewUploadID(),
		Bucket:    bk.ix.Info.Name,
		Key:       key,
		Initiated: b.now().UTC(),
		Meta:      meta,
		Initiator: initiator,
	})
	info := index.CloneUpload(u.Info)
	done := b.appendLocked(bk, &record{Op: opMkUpload, B: bucket, U: &info}, nil)
	bk.mu.Unlock()
	if err := await(ctx, done); err != nil {
		return storage.UploadInfo{}, err
	}
	return index.CloneUpload(info), nil
}

// GetMultipartUpload returns an in-progress upload of bucket/key.
func (b *Backend) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	if b.opts.Gateway {
		return storage.UploadInfo{}, errGateway
	}
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	u, err := bk.ix.Upload(key, uploadID)
	var info storage.UploadInfo
	if err == nil {
		info = index.CloneUpload(u.Info)
	}
	lsn := bk.lsn
	bk.mu.RUnlock()
	if werr := b.waitRead(ctx, lsn); werr != nil {
		return storage.UploadInfo{}, werr
	}
	return info, err
}

// partWriter streams one part into a spool and uploads it as a part blob.
type partWriter struct {
	*dataWriter
	bk       *bucket
	key      string
	uploadID string
	number   int
}

// NewPartWriter starts writing part partNumber (1..MaxPartNumber) of an
// upload. The upload's existence is checked again at Commit.
func (b *Backend) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	if b.opts.Gateway {
		return nil, errGateway
	}
	if partNumber < 1 || partNumber > MaxPartNumber {
		return nil, fmt.Errorf("s3store: invalid part number %d", partNumber)
	}
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	u, err := bk.ix.Upload(key, uploadID)
	var mode string
	if err == nil {
		mode = u.Info.Meta.Encryption
		// Uploads that only recorded SSE may be encrypted at completion:
		// stage their parts encrypted too when the backing can.
		if mode == "" && u.Info.Meta.SSE != "" && len(b.conn.Load().kek) > 0 {
			mode = storage.EncryptionAES256
		}
	}
	bk.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	var master []byte
	if mode != "" {
		// Parts of encrypted uploads (restic ones too) are stored AES256
		// encrypted, each under its own data ID.
		if master, err = b.bucketKey(ctx, bk, true); err != nil {
			return nil, err
		}
	}
	dw, err := b.newDataWriter(storage.Compression{}, master)
	if err != nil {
		return nil, err
	}
	return &partWriter{dataWriter: dw, bk: bk, key: key, uploadID: uploadID, number: partNumber}, nil
}

func (w *partWriter) Commit(ctx context.Context, meta storage.PartMeta) (storage.PartInfo, error) {
	size, err := w.close()
	if err != nil {
		return storage.PartInfo{}, err
	}
	b, bk := w.b, w.bk
	var ref blobRef
	var key string
	if size > 0 {
		ref = w.blob()
		key = b.partKey(w.uploadID, w.number, ref.ID)
		b.pending.add(key)
		defer b.pending.done(key)
		err = b.putBlob(ctx, key, w.sp, ref.Size)
	}
	_ = w.sp.Close()
	if err != nil {
		return storage.PartInfo{}, err
	}
	if err := b.relock(ctx, bk); err != nil {
		b.deleteLater(key)
		return storage.PartInfo{}, err
	}
	u, err := bk.ix.Upload(w.key, w.uploadID)
	if err != nil {
		bk.mu.Unlock()
		b.deleteLater(key)
		return storage.PartInfo{}, err
	}
	p := &index.Part[blobRef]{
		Info: storage.PartInfo{
			PartMeta:     index.ClonePartMeta(meta),
			PartNumber:   w.number,
			Size:         size,
			LastModified: b.now().UTC(),
		},
		Data: ref,
	}
	old := u.PutPart(p)
	var onDurable func()
	if old != nil && old.Data.ID != "" {
		oldKey := b.partKey(w.uploadID, w.number, old.Data.ID)
		onDurable = b.dropOnDurable([]string{oldKey})
	}
	rec := &record{Op: opPart, B: bk.ix.Info.Name, K: w.key, P: &partRec{Upload: w.uploadID, Info: p.Info, Data: ref}}
	done := b.appendLocked(bk, rec, onDurable)
	bk.mu.Unlock()
	if err := await(ctx, done); err != nil {
		return storage.PartInfo{}, err
	}
	out := p.Info
	out.PartMeta = index.ClonePartMeta(out.PartMeta)
	return out, nil
}

func (w *partWriter) Abort() error {
	w.abort()
	return nil
}

// ListParts returns all committed parts sorted by part number.
func (b *Backend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	if b.opts.Gateway {
		return nil, errGateway
	}
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	u, err := bk.ix.Upload(key, uploadID)
	var out []storage.PartInfo
	if err == nil {
		parts := u.Parts()
		out = make([]storage.PartInfo, 0, len(parts))
		for _, p := range parts {
			pi := p.Info
			pi.PartMeta = index.ClonePartMeta(pi.PartMeta)
			out = append(out, pi)
		}
	}
	lsn := bk.lsn
	bk.mu.RUnlock()
	if werr := b.waitRead(ctx, lsn); werr != nil {
		return nil, werr
	}
	return out, err
}

// ListMultipartUploads returns all in-progress uploads in listing order.
func (b *Backend) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	if b.opts.Gateway {
		return nil, errGateway
	}
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	defer bk.mu.RUnlock()
	uploads := bk.ix.Uploads()
	out := make([]storage.UploadInfo, 0, len(uploads))
	for _, u := range uploads {
		out = append(out, index.CloneUpload(u.Info))
	}
	return out, nil
}

// WalkMultipartUploads calls fn for the uploads with the given key prefix
// after position after, in listing order (see storage.UploadWalker).
func (b *Backend) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	if b.opts.Gateway {
		return errGateway
	}
	if fn == nil {
		return errors.New("s3store: nil walk function")
	}
	return index.WalkUploads(ctx, b.walkAcquire(ctx, bucket), prefix, after, fn)
}

// CompleteMultipartUpload concatenates the listed parts into a new object
// version and removes the upload with all its parts.
func (b *Backend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	return b.CompleteMultipartUploadWith(ctx, bucket, key, uploadID, partNumbers, meta, opts, storage.WriteOptions{})
}

// partSrc is a part being assembled.
type partSrc struct {
	key    string // "" for an empty part
	id     string
	size   int64 // content size
	stored int64 // blob size
	enc    bool  // stored AES256 encrypted
	number int
}

// CompleteMultipartUploadWith is CompleteMultipartUpload with write options
// (see storage.CompressingBackend). Plain parts that satisfy the service's
// multipart limits are composed server-side (UploadPartCopy) when the
// result is stored as is; otherwise the parts are streamed (decrypted when
// encrypted) through the compressor and encryption into a new blob, or
// into a restic snapshot. If the preconditions fail the upload is kept.
func (b *Backend) CompleteMultipartUploadWith(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, opts storage.CommitOptions, wopts storage.WriteOptions) (storage.ObjectInfo, error) {
	if b.opts.Gateway {
		return storage.ObjectInfo{}, errGateway
	}
	if err := b.checkWriteOptions(wopts); err != nil {
		return storage.ObjectInfo{}, err
	}
	bk, err := b.lockBucket(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	srcs, err := b.selectParts(bk, key, uploadID, partNumbers, opts)
	if err != nil {
		lsn := bk.lsn
		bk.mu.Unlock()
		if werr := b.waitRead(ctx, lsn); werr != nil {
			return storage.ObjectInfo{}, werr
		}
		return storage.ObjectInfo{}, err
	}
	for _, s := range srcs {
		if s.key != "" {
			b.blobs.acquire(s.key)
		}
	}
	bucketID := bk.id
	bk.mu.Unlock()
	defer func() {
		for _, s := range srcs {
			if s.key != "" {
				b.releaseBlob(s.key)
			}
		}
	}()

	// The key decrypting encrypted parts.
	var partMaster []byte
	for _, s := range srcs {
		if s.enc {
			if partMaster, err = b.bucketKey(ctx, bk, false); err != nil {
				return storage.ObjectInfo{}, err
			}
			break
		}
	}
	var (
		c   index.Content[blobRef]
		dst string
	)
	switch wopts.Encryption {
	case storage.EncryptionRestic:
		c, err = b.archiveParts(ctx, bk, bucket, key, uploadID, partNumbers, srcs, opts, partMaster)
	case storage.EncryptionAES256:
		var master []byte
		if master, err = b.bucketKey(ctx, bk, true); err == nil {
			c, dst, err = b.assemble(ctx, bucketID, srcs, wopts.Compression, partMaster, master)
		}
	default:
		c, dst, err = b.assemble(ctx, bucketID, srcs, wopts.Compression, partMaster, nil)
	}
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if dst != "" {
		defer b.pending.done(dst)
	}
	if err := b.relock(ctx, bk); err != nil {
		b.deleteLater(dst)
		return storage.ObjectInfo{}, err
	}
	// The upload may have changed while the parts were assembled.
	err = checkSameParts(bk, key, uploadID, partNumbers, srcs)
	var info storage.ObjectInfo
	var ch index.Change[blobRef]
	if err == nil {
		meta.Encryption = wopts.Encryption
		info, ch, err = bk.ix.Commit(key, c, meta, opts)
	}
	if err != nil {
		bk.mu.Unlock()
		b.deleteLater(dst)
		return storage.ObjectInfo{}, err
	}
	removed := bk.ix.RemoveUpload(uploadID)
	done := b.appendChange(bk, ch, uploadID, b.partBlobs(removed)...)
	bk.mu.Unlock()
	if err := await(ctx, done); err != nil {
		return storage.ObjectInfo{}, err
	}
	return info, nil
}

// checkSameParts verifies (bk locked) that the upload still has the parts
// that were assembled.
func checkSameParts(bk *bucket, key, uploadID string, numbers []int, srcs []partSrc) error {
	u, err := bk.ix.Upload(key, uploadID)
	if err != nil {
		return err
	}
	parts, err := u.SelectParts(numbers, nil)
	if err != nil {
		return err
	}
	for i, p := range parts {
		if p.Data.ID != srcs[i].id {
			return fmt.Errorf("%w: part %d of upload %q was replaced", storage.ErrPartNotFound, srcs[i].number, uploadID)
		}
	}
	return nil
}

// selectParts resolves the listed parts of an upload (bk locked) and checks
// the commit preconditions early.
func (b *Backend) selectParts(bk *bucket, key, uploadID string, numbers []int, opts storage.CommitOptions) ([]partSrc, error) {
	u, err := bk.ix.Upload(key, uploadID)
	if err != nil {
		return nil, err
	}
	parts, err := u.SelectParts(numbers, opts.PartETags)
	if err != nil {
		return nil, err
	}
	if err := bk.ix.CheckPreconditions(key, opts); err != nil {
		return nil, err
	}
	srcs := make([]partSrc, len(parts))
	for i, p := range parts {
		srcs[i] = partSrc{id: p.Data.ID, size: p.Info.Size, stored: p.Data.Size, enc: p.Data.Enc != "", number: p.Info.PartNumber}
		if p.Data.ID != "" {
			srcs[i].key = b.partKey(uploadID, p.Info.PartNumber, p.Data.ID)
		}
	}
	return srcs, nil
}

// copyPart copies the content of a part to w, decrypting an encrypted part
// with master, and verifies its size.
func (b *Backend) copyPart(ctx context.Context, w io.Writer, s partSrc, master []byte) error {
	if s.key == "" {
		return nil
	}
	br := b.newBlobReader(ctx, s.key, s.stored, false, missingPart(s.number))
	defer br.Close()
	var r io.Reader = br
	if s.enc {
		if want := storage.EncryptedSize(s.size); s.stored != want {
			return fmt.Errorf("%w: encrypted part %d has %d bytes, expected %d", errBlobMissing, s.number, s.stored, want)
		}
		cr, err := crypt.NewReader(br, crypt.ObjectDataKey(master, s.id), s.size)
		if err != nil {
			return err
		}
		r = cr
	}
	n, err := io.Copy(w, r)
	if err == nil && n != s.size {
		err = fmt.Errorf("%w: part %d has %d bytes, want %d", storage.ErrPartNotFound, s.number, n, s.size)
	}
	return err
}

// assemble builds the data blob of a completed upload from its parts
// (encrypted parts decrypted with partMaster), compressed with c and, with
// master, encrypted. It returns the content and the new blob's key (""
// for empty content), which is pending until the caller calls
// b.pending.done.
func (b *Backend) assemble(ctx context.Context, bucketID string, srcs []partSrc, c storage.Compression, partMaster, master []byte) (index.Content[blobRef], string, error) {
	var total int64
	plain := true
	for _, s := range srcs {
		total += s.size
		plain = plain && !s.enc
	}
	if total == 0 {
		return index.Content[blobRef]{}, "", nil
	}
	if !c.Enabled() && master == nil && plain {
		keys := make([]string, len(srcs))
		sizes := make([]int64, len(srcs))
		nums := make([]int, len(srcs))
		for i, s := range srcs {
			keys[i], sizes[i], nums[i] = s.key, s.size, s.number
		}
		if _, ok := composeRanges(keys, sizes, nums); ok && (len(srcs) > 1 || srcs[0].key != "") {
			id := randomID(16)
			dst := b.dataKey(bucketID, id)
			b.pending.add(dst)
			if err := b.compose(ctx, dst, keys, sizes, nums); err != nil {
				b.pending.done(dst)
				return index.Content[blobRef]{}, "", err
			}
			return index.Content[blobRef]{Data: blobRef{ID: id, Size: total}, Size: total}, dst, nil
		}
	}
	dw, err := b.newDataWriter(c, master)
	if err != nil {
		return index.Content[blobRef]{}, "", err
	}
	for _, s := range srcs {
		if err := b.copyPart(ctx, dw, s, partMaster); err != nil {
			dw.abort()
			return index.Content[blobRef]{}, "", err
		}
	}
	if _, err := dw.close(); err != nil {
		return index.Content[blobRef]{}, "", err
	}
	return dw.upload(ctx, bucketID)
}

// archiveParts streams the parts into a restic snapshot of bucket/key; the
// parts and preconditions are checked again before the snapshot is saved,
// so a refused or raced completion archives nothing.
func (b *Backend) archiveParts(ctx context.Context, bk *bucket, bucket, key, uploadID string, numbers []int, srcs []partSrc,
	opts storage.CommitOptions, partMaster []byte) (index.Content[blobRef], error) {
	w, err := b.newResticWriter(ctx, bk, bucket, key)
	if err != nil {
		return index.Content[blobRef]{}, err
	}
	defer w.a.Abort()
	for _, s := range srcs {
		if err := b.copyPart(ctx, w, s, partMaster); err != nil {
			return index.Content[blobRef]{}, err
		}
	}
	snap, err := w.a.Finish(func() error {
		bk.mu.RLock()
		defer bk.mu.RUnlock()
		if bk.gone() {
			return b.goneErr(bk)
		}
		if err := checkSameParts(bk, key, uploadID, numbers, srcs); err != nil {
			return err
		}
		return bk.ix.CheckPreconditions(key, opts)
	})
	if err != nil {
		return index.Content[blobRef]{}, err
	}
	return index.Content[blobRef]{Data: blobRef{Restic: snap}, Size: w.size}, nil
}

// AbortMultipartUpload removes the upload and its parts.
func (b *Backend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	if b.opts.Gateway {
		return errGateway
	}
	bk, err := b.lockBucket(ctx, bucket)
	if err != nil {
		return err
	}
	if _, err := bk.ix.Upload(key, uploadID); err != nil {
		lsn := bk.lsn
		bk.mu.Unlock()
		if werr := b.waitRead(ctx, lsn); werr != nil {
			return werr
		}
		return err
	}
	parts := b.partBlobs(bk.ix.RemoveUpload(uploadID))
	done := b.appendLocked(bk, &record{Op: opRmUpload, B: bucket, RmUpload: uploadID}, b.dropOnDurable(parts))
	bk.mu.Unlock()
	return await(ctx, done)
}
