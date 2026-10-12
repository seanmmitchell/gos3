package s3

// This file holds the object listings of the s3-bucket work item:
// ListObjects (V1), ListObjectsV2 and ListObjectVersions, built on the
// generic storage.ListObjects / storage.ListObjectVersions helpers.

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// listContents is one <Contents> element of ListObjects / ListObjectsV2.
type listContents struct {
	Key               string   `xml:"Key"`
	LastModified      string   `xml:"LastModified"`
	ETag              string   `xml:"ETag"`
	ChecksumAlgorithm []string `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType      string   `xml:"ChecksumType,omitempty"`
	Size              int64    `xml:"Size"`
	Owner             *Owner   `xml:"Owner,omitempty"`
	StorageClass      string   `xml:"StorageClass"`
}

// listCommonPrefix is one <CommonPrefixes> element.
type listCommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// listBucketResultV1 is the ListObjects (V1) response.
type listBucketResultV1 struct {
	XMLName        xml.Name           `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name           string             `xml:"Name"`
	Prefix         string             `xml:"Prefix"`
	Marker         string             `xml:"Marker"`
	NextMarker     string             `xml:"NextMarker,omitempty"`
	MaxKeys        int                `xml:"MaxKeys"`
	Delimiter      string             `xml:"Delimiter,omitempty"`
	IsTruncated    bool               `xml:"IsTruncated"`
	EncodingType   string             `xml:"EncodingType,omitempty"`
	Contents       []listContents     `xml:"Contents"`
	CommonPrefixes []listCommonPrefix `xml:"CommonPrefixes"`
}

// listBucketResultV2 is the ListObjectsV2 response.
type listBucketResultV2 struct {
	XMLName               xml.Name           `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name                  string             `xml:"Name"`
	Prefix                string             `xml:"Prefix"`
	KeyCount              int                `xml:"KeyCount"`
	MaxKeys               int                `xml:"MaxKeys"`
	Delimiter             string             `xml:"Delimiter,omitempty"`
	IsTruncated           bool               `xml:"IsTruncated"`
	EncodingType          string             `xml:"EncodingType,omitempty"`
	ContinuationToken     string             `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string             `xml:"NextContinuationToken,omitempty"`
	StartAfter            string             `xml:"StartAfter,omitempty"`
	Contents              []listContents     `xml:"Contents"`
	CommonPrefixes        []listCommonPrefix `xml:"CommonPrefixes"`
}

// listVersionsResult is the ListObjectVersions response.
type listVersionsResult struct {
	XMLName             xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListVersionsResult"`
	Name                string   `xml:"Name"`
	Prefix              string   `xml:"Prefix"`
	KeyMarker           string   `xml:"KeyMarker"`
	VersionIDMarker     string   `xml:"VersionIdMarker"`
	NextKeyMarker       string   `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string   `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int      `xml:"MaxKeys"`
	Delimiter           string   `xml:"Delimiter,omitempty"`
	IsTruncated         bool     `xml:"IsTruncated"`
	EncodingType        string   `xml:"EncodingType,omitempty"`
	// Entries holds <Version> and <DeleteMarker> elements interleaved in
	// listing order (each entry's MarshalXML picks the element name).
	Entries        []listVersionEntry
	CommonPrefixes []listCommonPrefix `xml:"CommonPrefixes"`
}

// listVersionEntry is a <Version> or <DeleteMarker> element; element
// selects which. Delete markers carry no ETag, checksum, Size or
// StorageClass.
type listVersionEntry struct {
	element string // "Version" or "DeleteMarker"
	listVersionFields
}

// listVersionFields are the children of a listVersionEntry element.
type listVersionFields struct {
	Key               string   `xml:"Key"`
	VersionID         string   `xml:"VersionId"`
	IsLatest          bool     `xml:"IsLatest"`
	LastModified      string   `xml:"LastModified"`
	ETag              string   `xml:"ETag,omitempty"`
	ChecksumAlgorithm []string `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType      string   `xml:"ChecksumType,omitempty"`
	Size              *int64   `xml:"Size,omitempty"`
	StorageClass      string   `xml:"StorageClass,omitempty"`
	Owner             *Owner   `xml:"Owner,omitempty"`
}

// MarshalXML writes the entry as <Version> or <DeleteMarker> in the
// enclosing S3 namespace. A dynamic XMLName field would make encoding/xml
// emit xmlns="" on the element, moving it and its children out of the S3
// namespace for namespace-aware parsers.
func (e listVersionEntry) MarshalXML(enc *xml.Encoder, _ xml.StartElement) error {
	return enc.EncodeElement(e.listVersionFields, xml.StartElement{Name: xml.Name{Local: e.element}})
}

// objectListParams holds the parameters shared by the three listings.
type objectListParams struct {
	prefix    string
	delimiter string
	maxKeys   int  // 0..1000; 0 means an empty, untruncated result
	encode    bool // encoding-type=url
}

// parseObjectListParams parses prefix, delimiter, max-keys and encoding-type.
func parseObjectListParams(q url.Values) (objectListParams, error) {
	enc, err := parseEncodingType(q)
	if err != nil {
		return objectListParams{}, err
	}
	n, err := parseMaxKeys(q, "max-keys", maxListKeys, maxListKeys)
	if err != nil {
		return objectListParams{}, err
	}
	return objectListParams{prefix: q.Get("prefix"), delimiter: q.Get("delimiter"), maxKeys: n, encode: enc}, nil
}

// encodingType returns the EncodingType element value.
func (p objectListParams) encodingType() string {
	if p.encode {
		return "url"
	}
	return ""
}

// enc applies encoding-type=url to a key-like value.
func (p objectListParams) enc(s string) string { return maybeURLEncode(s, p.encode) }

// commonPrefixes converts common prefixes to response elements.
func (p objectListParams) commonPrefixes(cps []string) []listCommonPrefix {
	out := make([]listCommonPrefix, len(cps))
	for i, cp := range cps {
		out[i] = listCommonPrefix{Prefix: p.enc(cp)}
	}
	return out
}

// contents converts objects to <Contents> elements; owner is nil when the
// Owner element must be omitted.
func (p objectListParams) contents(objs []storage.ObjectInfo, owner *Owner) []listContents {
	out := make([]listContents, len(objs))
	for i := range objs {
		o := &objs[i]
		algos, typ := listingChecksums(o.ObjectMeta)
		out[i] = listContents{
			Key:               p.enc(o.Key),
			LastModified:      formatXMLTime(o.LastModified),
			ETag:              quoteETag(o.ETag),
			ChecksumAlgorithm: algos,
			ChecksumType:      typ,
			Size:              o.Size,
			Owner:             owner,
			StorageClass:      storageClassOrStandard(o.StorageClass),
		}
	}
	return out
}

// listingChecksums returns the checksum algorithms of an object (in canonical
// order) and its checksum type for listings: the stored type, or
// FULL_OBJECT when checksums exist without one; both empty when the object
// has no checksum.
func listingChecksums(m storage.ObjectMeta) ([]string, string) {
	if len(m.Checksums) == 0 {
		return nil, ""
	}
	var algos []string
	for _, a := range checksumAlgorithms {
		if m.Checksums[a] != "" {
			algos = append(algos, a)
		}
	}
	if len(algos) == 0 {
		return nil, ""
	}
	typ := m.ChecksumType
	if typ == "" {
		typ = ChecksumTypeFullObject
	}
	return algos, typ
}

// listingOwner returns the Owner element reported for the addressed
// bucket's objects. gos3 records no per-object owner; objects are reported
// as owned by the bucket owner (as with ObjectOwnership
// BucketOwnerEnforced).
func listingOwner(bi storage.BucketInfo) *Owner {
	o := ownerFor(bi.Owner)
	return &o
}

// handleListObjects implements ListObjects (V1): prefix, delimiter, marker,
// max-keys (0..1000) and encoding-type=url. NextMarker is only returned
// when a delimiter was given and the result is truncated (clients use the
// last Key otherwise); Owner is always included.
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	p, err := parseObjectListParams(rc.query)
	if err != nil {
		return err
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	marker := rc.query.Get("marker")
	res := listBucketResultV1{
		Name:         rc.bucket,
		Prefix:       p.enc(p.prefix),
		Marker:       p.enc(marker),
		MaxKeys:      p.maxKeys,
		Delimiter:    p.enc(p.delimiter),
		EncodingType: p.encodingType(),
	}
	if p.maxKeys > 0 {
		lr, err := storage.ListObjects(ctx, s.backend, rc.bucket, storage.ListParams{
			Prefix: p.prefix, Delimiter: p.delimiter, After: marker, MaxKeys: p.maxKeys,
		})
		if err != nil {
			return bucketError(err, rc.bucket)
		}
		res.IsTruncated = lr.IsTruncated
		if lr.IsTruncated && p.delimiter != "" {
			res.NextMarker = p.enc(lr.NextAfter)
		}
		res.Contents = p.contents(lr.Objects, listingOwner(bi))
		res.CommonPrefixes = p.commonPrefixes(lr.CommonPrefixes)
	}
	return writeXML(w, http.StatusOK, res)
}

// handleListObjectsV2 implements ListObjectsV2: prefix, delimiter,
// max-keys, encoding-type=url (applied to Key, Prefix, Delimiter,
// StartAfter and CommonPrefixes), start-after, fetch-owner and an opaque
// continuation-token (base64 of the position to resume after; it takes
// precedence over start-after).
func (s *Server) handleListObjectsV2(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	q := rc.query
	p, err := parseObjectListParams(q)
	if err != nil {
		return err
	}
	fetchOwner, err := parseFetchOwner(q)
	if err != nil {
		return err
	}
	startAfter := q.Get("start-after")
	after := startAfter
	token := q.Get("continuation-token")
	if q.Has("continuation-token") {
		if after, err = decodeContinuationToken(token); err != nil {
			return err
		}
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	res := listBucketResultV2{
		Name:              rc.bucket,
		Prefix:            p.enc(p.prefix),
		MaxKeys:           p.maxKeys,
		Delimiter:         p.enc(p.delimiter),
		EncodingType:      p.encodingType(),
		ContinuationToken: token,
		StartAfter:        p.enc(startAfter),
	}
	if p.maxKeys > 0 {
		lr, err := storage.ListObjects(ctx, s.backend, rc.bucket, storage.ListParams{
			Prefix: p.prefix, Delimiter: p.delimiter, After: after, MaxKeys: p.maxKeys,
		})
		if err != nil {
			return bucketError(err, rc.bucket)
		}
		var owner *Owner
		if fetchOwner {
			owner = listingOwner(bi)
		}
		res.IsTruncated = lr.IsTruncated
		if lr.IsTruncated {
			res.NextContinuationToken = encodeContinuationToken(lr.NextAfter)
		}
		res.Contents = p.contents(lr.Objects, owner)
		res.CommonPrefixes = p.commonPrefixes(lr.CommonPrefixes)
		res.KeyCount = len(res.Contents) + len(res.CommonPrefixes)
	}
	return writeXML(w, http.StatusOK, res)
}

// parseFetchOwner parses the fetch-owner parameter ("true"/"false", any
// case; absent means false).
func parseFetchOwner(q url.Values) (bool, error) {
	v := strings.TrimSpace(q.Get("fetch-owner"))
	switch {
	case v == "", strings.EqualFold(v, "false"):
		return false, nil
	case strings.EqualFold(v, "true"):
		return true, nil
	}
	return false, s3err.ErrInvalidArgument.WithMessage("Invalid value for fetch-owner").
		With("ArgumentName", "fetch-owner").With("ArgumentValue", v)
}

// handleListObjectVersions implements ListObjectVersions: prefix,
// delimiter, key-marker, version-id-marker (only together with
// key-marker), max-keys and encoding-type=url (applied to Key, Prefix,
// Delimiter, KeyMarker, NextKeyMarker and CommonPrefixes). Versions and
// delete markers are returned interleaved in listing order (keys
// ascending, newest version first); null versions report VersionId
// "null".
func (s *Server) handleListObjectVersions(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	q := rc.query
	p, err := parseObjectListParams(q)
	if err != nil {
		return err
	}
	keyMarker := q.Get("key-marker")
	vidMarker := q.Get("version-id-marker")
	if vidMarker != "" && keyMarker == "" {
		return s3err.ErrInvalidArgument.WithMessage("A version-id marker cannot be specified without a key marker.").
			With("ArgumentName", "version-id-marker").With("ArgumentValue", vidMarker)
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	res := listVersionsResult{
		Name:            rc.bucket,
		Prefix:          p.enc(p.prefix),
		KeyMarker:       p.enc(keyMarker),
		VersionIDMarker: vidMarker,
		MaxKeys:         p.maxKeys,
		Delimiter:       p.enc(p.delimiter),
		EncodingType:    p.encodingType(),
	}
	if p.maxKeys > 0 {
		vr, err := storage.ListObjectVersions(ctx, s.backend, rc.bucket, storage.VersionListParams{
			Prefix: p.prefix, Delimiter: p.delimiter, KeyMarker: keyMarker, VersionIDMarker: vidMarker, MaxKeys: p.maxKeys,
		})
		if err != nil {
			return bucketError(err, rc.bucket)
		}
		res.IsTruncated = vr.IsTruncated
		if vr.IsTruncated {
			res.NextKeyMarker = p.enc(vr.NextKeyMarker)
			res.NextVersionIDMarker = vr.NextVersionIDMarker
		}
		owner := listingOwner(bi)
		res.Entries = make([]listVersionEntry, len(vr.Versions))
		for i := range vr.Versions {
			res.Entries[i] = p.versionEntry(&vr.Versions[i], owner)
		}
		res.CommonPrefixes = p.commonPrefixes(vr.CommonPrefixes)
	}
	return writeXML(w, http.StatusOK, res)
}

// versionEntry converts one version or delete marker.
func (p objectListParams) versionEntry(v *storage.ObjectInfo, owner *Owner) listVersionEntry {
	vid := v.VersionID
	if vid == "" {
		vid = storage.NullVersionID
	}
	e := listVersionEntry{listVersionFields: listVersionFields{
		Key:          p.enc(v.Key),
		VersionID:    vid,
		IsLatest:     v.IsLatest,
		LastModified: formatXMLTime(v.LastModified),
		Owner:        owner,
	}}
	if v.DeleteMarker {
		e.element = "DeleteMarker"
		return e
	}
	e.element = "Version"
	e.ETag = quoteETag(v.ETag)
	e.ChecksumAlgorithm, e.ChecksumType = listingChecksums(v.ObjectMeta)
	size := v.Size
	e.Size = &size
	e.StorageClass = storageClassOrStandard(v.StorageClass)
	return e
}
