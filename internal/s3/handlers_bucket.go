package s3

// This file holds the bucket operations (ListBuckets, CreateBucket,
// DeleteBucket, HeadBucket, GetBucketLocation); it is owned by the s3-bucket
// work item. The object listings (ListObjects, ListObjectsV2,
// ListObjectVersions) live in handlers_bucket_list.go.

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Limits of the bucket operations.
const (
	// maxCreateBucketBody bounds the CreateBucketConfiguration document.
	maxCreateBucketBody = 64 << 10
	// maxListBuckets is the largest max-buckets value ListBuckets accepts.
	maxListBuckets = 10000
	// maxContinuationToken bounds the length of continuation tokens
	// (encoded) accepted by ListBuckets and ListObjectsV2.
	maxContinuationToken = 4096
	// maxRegionLength bounds LocationConstraint values.
	maxRegionLength = 64
)

// createBucketOwnerships are the valid x-amz-object-ownership values.
var createBucketOwnerships = map[string]bool{
	"BucketOwnerPreferred": true,
	"ObjectWriter":         true,
	"BucketOwnerEnforced":  true,
}

// --- ListBuckets ---

// listAllMyBucketsResult is the ListBuckets response.
type listAllMyBucketsResult struct {
	XMLName           xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Owner             Owner          `xml:"Owner"`
	Buckets           []bucketResult `xml:"Buckets>Bucket"`
	ContinuationToken string         `xml:"ContinuationToken,omitempty"`
	Prefix            string         `xml:"Prefix,omitempty"`
}

// bucketResult is one <Bucket> of ListBuckets.
type bucketResult struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
	BucketRegion string `xml:"BucketRegion"`
}

// handleListBuckets implements ListBuckets.
//
// All buckets of the server are listed (gos3 is a single account; the
// caller needs s3:ListAllMyBuckets). The 2024 pagination parameters are
// supported: prefix and bucket-region filter the result; max-buckets
// (1..10000) limits it, and a truncated response carries an opaque
// ContinuationToken to pass back as continuation-token. Without
// max-buckets every matching bucket is returned. BucketRegion is always
// reported.
func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	q := rc.query
	limit := 0 // unlimited
	if q.Has("max-buckets") {
		v := q.Get("max-buckets")
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 || n > maxListBuckets {
			return s3err.ErrInvalidArgument.WithMessage("Argument max-buckets must be an integer between 1 and %d", maxListBuckets).
				With("ArgumentName", "max-buckets").With("ArgumentValue", v)
		}
		limit = n
	}
	after := ""
	if q.Has("continuation-token") {
		a, err := decodeContinuationToken(q.Get("continuation-token"))
		if err != nil {
			return err
		}
		after = a
	}
	prefix := q.Get("prefix")
	regionFilter := q.Get("bucket-region")

	buckets, err := s.backend.ListBuckets(r.Context())
	if err != nil {
		return mapStorageError(err)
	}
	res := listAllMyBucketsResult{
		Owner:   ownerFor(""), // the account owner
		Buckets: []bucketResult{},
		Prefix:  prefix,
	}
	for _, b := range buckets {
		if after != "" && b.Name <= after {
			continue
		}
		if !strings.HasPrefix(b.Name, prefix) {
			continue
		}
		region := s.regionOfBucket(b)
		if regionFilter != "" && !strings.EqualFold(region, regionFilter) {
			continue
		}
		if limit > 0 && len(res.Buckets) == limit {
			// At least one more bucket matches: the page is truncated.
			res.ContinuationToken = encodeContinuationToken(res.Buckets[len(res.Buckets)-1].Name)
			break
		}
		res.Buckets = append(res.Buckets, bucketResult{
			Name:         b.Name,
			CreationDate: formatXMLTime(b.CreatedAt),
			BucketRegion: region,
		})
	}
	return writeXML(w, http.StatusOK, res)
}

// regionOfBucket returns the region of a bucket (the server region when the
// bucket has none recorded).
func (s *Server) regionOfBucket(b storage.BucketInfo) string {
	if b.Region != "" {
		return b.Region
	}
	return s.region()
}

// encodeContinuationToken returns the opaque continuation token for a
// listing position (the last key, common prefix or bucket name returned).
func encodeContinuationToken(after string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(after))
}

// decodeContinuationToken decodes a token made by encodeContinuationToken
// (standard base64, padded or not, is accepted too). Empty, oversized or
// undecodable tokens yield InvalidArgument.
func decodeContinuationToken(tok string) (string, error) {
	bad := s3err.ErrInvalidArgument.WithMessage("The continuation token provided is incorrect").
		With("ArgumentName", "continuation-token").With("ArgumentValue", tok)
	if tok == "" || len(tok) > maxContinuationToken {
		return "", bad
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(tok); err == nil {
			if len(b) == 0 {
				return "", bad
			}
			return string(b), nil
		}
	}
	return "", bad
}

// --- CreateBucket ---

// createBucketConfiguration is the optional CreateBucket request body.
type createBucketConfiguration struct {
	XMLName            xml.Name  `xml:"CreateBucketConfiguration"`
	LocationConstraint *string   `xml:"LocationConstraint"`
	Location           *struct{} `xml:"Location"`
	Bucket             *struct{} `xml:"Bucket"`
}

// createBucketOwnershipDoc is the OwnershipControls document stored under
// storage.ConfigOwnership when CreateBucket carries x-amz-object-ownership.
type createBucketOwnershipDoc struct {
	XMLName xml.Name                    `xml:"OwnershipControls"`
	XMLNS   string                      `xml:"xmlns,attr,omitempty"`
	Rules   []createBucketOwnershipRule `xml:"Rule"`
}

// createBucketOwnershipRule is one <Rule> of OwnershipControls.
type createBucketOwnershipRule struct {
	ObjectOwnership string `xml:"ObjectOwnership"`
}

// createBucketOwnershipXML renders the OwnershipControls document for one
// ObjectOwnership setting.
func createBucketOwnershipXML(ownership string) ([]byte, error) {
	return xml.Marshal(createBucketOwnershipDoc{
		XMLNS: s3Namespace,
		Rules: []createBucketOwnershipRule{{ObjectOwnership: ownership}},
	})
}

// handleCreateBucket implements CreateBucket.
//
// The name is validated strictly (validateBucketName). The optional
// CreateBucketConfiguration body may carry a LocationConstraint, stored as
// the bucket region (absent or empty: the server region; "EU" means
// eu-west-1; any well-formed region name is accepted). Headers:
// x-amz-bucket-object-lock-enabled (object lock + versioning), x-amz-acl
// (canned ACL stored under storage.ConfigACL), x-amz-object-ownership
// (OwnershipControls stored under storage.ConfigOwnership). As on AWS,
// those headers additionally require s3:PutBucketObjectLockConfiguration +
// s3:PutBucketVersioning, s3:PutBucketAcl and s3:PutBucketOwnershipControls.
// An existing bucket yields BucketAlreadyOwnedByYou when the caller is its
// owner (or root) and BucketAlreadyExists otherwise.
func (s *Server) handleCreateBucket(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	bucket := rc.bucket
	if err := validateBucketName(bucket); err != nil {
		return err
	}
	h := r.Header
	if err := checkGrantHeaders(h); err != nil {
		return err
	}
	acl, err := validateCannedACL(h.Get("X-Amz-Acl"), true)
	if err != nil {
		return err
	}
	lock, err := parseCreateBucketBool(h, "X-Amz-Bucket-Object-Lock-Enabled")
	if err != nil {
		return err
	}
	ownership, err := parseCreateBucketOwnership(h.Get("X-Amz-Object-Ownership"))
	if err != nil {
		return err
	}
	if ownership == "BucketOwnerEnforced" && acl != "" && acl != aclBucketOwnerFullControl {
		return s3err.ErrInvalidBucketAclWithObjectOwnership
	}
	region, err := s.createBucketRegion(r)
	if err != nil {
		return err
	}
	// The (optional) body may have taken long: the caller must still be
	// allowed to create the bucket.
	if err := s.revalidateWrite(rc, func() error { return s.reauthorizeOp(ctx, r, rc, nil) }); err != nil {
		return err
	}

	// Extra permissions AWS requires for the optional settings.
	var extra []string
	if lock {
		extra = append(extra, "s3:PutBucketObjectLockConfiguration", "s3:PutBucketVersioning")
	}
	if h.Get("X-Amz-Acl") != "" {
		extra = append(extra, "s3:PutBucketAcl")
	}
	if ownership != "" {
		extra = append(extra, "s3:PutBucketOwnershipControls")
	}
	for _, action := range extra {
		if err := s.authorizeTarget(ctx, r, rc, authzTarget{Action: action, Bucket: bucket, NoBucketLookup: true}); err != nil {
			return err
		}
	}

	owner := ""
	if !rc.identity.IsRoot && !rc.identity.IsAnonymous {
		owner = rc.identity.User
	}
	err = s.backend.CreateBucket(ctx, bucket, storage.CreateBucketOptions{Region: region, ObjectLockEnabled: lock, Owner: owner})
	if errors.Is(err, storage.ErrBucketExists) {
		return s.bucketExistsError(r, rc)
	}
	if err != nil {
		return bucketError(err, bucket)
	}
	rc.forgetBucket()
	s.invalidateBucketConfig(bucket)

	if err := s.storeCreateBucketConfigs(r, bucket, acl, ownership); err != nil {
		// Do not leave a bucket behind whose requested access settings are
		// missing; the bucket is still empty, so this cannot fail on
		// contents.
		_ = s.backend.DeleteBucket(ctx, bucket)
		s.invalidateBucketConfig(bucket)
		return err
	}

	w.Header().Set("Location", s.bucketLocationHeader(r, rc))
	writeEmpty(w, http.StatusOK)
	return nil
}

// storeCreateBucketConfigs persists the canned ACL and object ownership
// requested at bucket creation.
func (s *Server) storeCreateBucketConfigs(r *http.Request, bucket, acl, ownership string) error {
	ctx := r.Context()
	if acl != "" {
		if err := s.backend.PutBucketConfig(ctx, bucket, storage.ConfigACL, []byte(acl)); err != nil {
			return bucketError(err, bucket)
		}
	}
	if ownership != "" {
		doc, err := createBucketOwnershipXML(ownership)
		if err != nil {
			return err
		}
		if err := s.backend.PutBucketConfig(ctx, bucket, storage.ConfigOwnership, doc); err != nil {
			return bucketError(err, bucket)
		}
	}
	return nil
}

// bucketExistsError returns BucketAlreadyOwnedByYou or BucketAlreadyExists
// for a CreateBucket of an existing bucket.
func (s *Server) bucketExistsError(r *http.Request, rc *reqContext) error {
	bi, err := s.lookupBucket(r.Context(), rc.bucket)
	if err == nil && (rc.identity.IsRoot || (!rc.identity.IsAnonymous && bi.Owner == rc.identity.User)) {
		return s3err.ErrBucketAlreadyOwnedByYou.With("BucketName", rc.bucket)
	}
	return s3err.ErrBucketAlreadyExists.With("BucketName", rc.bucket)
}

// bucketLocationHeader returns the Location header of a created bucket:
// "/<bucket>" for path-style requests and "<scheme>://<host>/" for
// virtual-hosted-style ones, as AWS does.
func (s *Server) bucketLocationHeader(r *http.Request, rc *reqContext) string {
	if rc.virtualHost && r.Host != "" {
		scheme := "http"
		if rc.secure {
			scheme = "https"
		}
		return scheme + "://" + r.Host + "/"
	}
	return "/" + rc.bucket
}

// createBucketRegion reads the optional CreateBucketConfiguration body and
// returns the region of the new bucket.
func (s *Server) createBucketRegion(r *http.Request) (string, error) {
	data, err := readBody(r.Body, maxCreateBucketBody)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return s.region(), nil
	}
	var cfg createBucketConfiguration
	if err := decodeXML(data, &cfg); err != nil {
		return "", err
	}
	if cfg.Location != nil || cfg.Bucket != nil {
		return "", s3err.ErrNotImplemented.WithMessage("Directory buckets are not supported.")
	}
	if cfg.LocationConstraint == nil {
		return s.region(), nil
	}
	return s.normalizeLocationConstraint(*cfg.LocationConstraint)
}

// normalizeLocationConstraint validates a LocationConstraint and returns
// the region it names: "" means the server region and the legacy "EU"
// means eu-west-1. Any well-formed region name is accepted: 1..64
// lowercase letters, digits and hyphens, starting with a letter and not
// ending with a hyphen.
func (s *Server) normalizeLocationConstraint(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "":
		return s.region(), nil
	case "EU":
		return "eu-west-1", nil
	}
	if !validLocationConstraint(v) {
		return "", s3err.ErrInvalidLocationConstraint.With("LocationConstraint", v)
	}
	return v, nil
}

// validLocationConstraint reports whether v looks like a region name.
func validLocationConstraint(v string) bool {
	if v == "" || len(v) > maxRegionLength || v[0] < 'a' || v[0] > 'z' || v[len(v)-1] == '-' {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// parseCreateBucketBool parses an optional boolean header ("true"/"false", any
// case; absent means false).
func parseCreateBucketBool(h http.Header, name string) (bool, error) {
	v := strings.TrimSpace(h.Get(name))
	switch {
	case v == "":
		return false, nil
	case strings.EqualFold(v, "true"):
		return true, nil
	case strings.EqualFold(v, "false"):
		return false, nil
	}
	return false, s3err.ErrInvalidArgument.WithMessage("Invalid value for header %s", strings.ToLower(name)).
		With("ArgumentName", strings.ToLower(name)).With("ArgumentValue", v)
}

// parseCreateBucketOwnership validates an x-amz-object-ownership value ("" when
// absent).
func parseCreateBucketOwnership(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || createBucketOwnerships[v] {
		return v, nil
	}
	if !utf8.ValidString(v) {
		v = strings.ToValidUTF8(v, "�")
	}
	return "", s3err.ErrInvalidArgument.WithMessage("Invalid x-amz-object-ownership header: %s", v).
		With("ArgumentName", "x-amz-object-ownership").With("ArgumentValue", v)
}

// --- DeleteBucket ---

// handleDeleteBucket implements DeleteBucket: the bucket must hold no
// object versions, delete markers or in-progress multipart uploads
// (BucketNotEmpty otherwise). Its configuration documents go with it.
func (s *Server) handleDeleteBucket(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	if err := s.backend.DeleteBucket(r.Context(), rc.bucket); err != nil {
		return bucketError(err, rc.bucket)
	}
	rc.forgetBucket()
	s.invalidateBucketConfig(rc.bucket)
	writeNoContent(w)
	return nil
}

// --- HeadBucket ---

// handleHeadBucket implements HeadBucket: 200 with x-amz-bucket-region and
// x-amz-access-point-alias: false; a missing bucket is 404 (no body).
func (s *Server) handleHeadBucket(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	bi, err := s.bucketInfo(r.Context(), rc)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("X-Amz-Bucket-Region", s.regionOfBucket(bi))
	h.Set("X-Amz-Access-Point-Alias", "false")
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- GetBucketLocation ---

// locationConstraintResult is the GetBucketLocation response.
type locationConstraintResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
	Region  string   `xml:",chardata"`
}

// handleGetBucketLocation implements GetBucketLocation. As on AWS, buckets
// in us-east-1 report an empty LocationConstraint.
func (s *Server) handleGetBucketLocation(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	bi, err := s.bucketInfo(r.Context(), rc)
	if err != nil {
		return err
	}
	region := s.regionOfBucket(bi)
	if region == defaultRegion {
		region = ""
	}
	return writeXML(w, http.StatusOK, locationConstraintResult{Region: region})
}
