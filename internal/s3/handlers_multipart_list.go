package s3

// ListParts and ListMultipartUploads. Owned by the s3-multipart work item.

import (
	"encoding/xml"
	"math"
	"net/http"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// maxListParts / maxListUploads cap max-parts and max-uploads.
const (
	maxListParts   = 1000
	maxListUploads = 1000
)

// --- ListParts ---

// listPartsResult is the ListParts response body.
type listPartsResult struct {
	XMLName              xml.Name   `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
	Bucket               string     `xml:"Bucket"`
	Key                  string     `xml:"Key"`
	UploadID             string     `xml:"UploadId"`
	Initiator            Owner      `xml:"Initiator"`
	Owner                Owner      `xml:"Owner"`
	StorageClass         string     `xml:"StorageClass"`
	PartNumberMarker     int        `xml:"PartNumberMarker"`
	NextPartNumberMarker int        `xml:"NextPartNumberMarker"`
	MaxParts             int        `xml:"MaxParts"`
	IsTruncated          bool       `xml:"IsTruncated"`
	Parts                []listPart `xml:"Part"`
	ChecksumAlgorithm    string     `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType         string     `xml:"ChecksumType,omitempty"`
}

// listPart is one <Part> of ListPartsResult.
type listPart struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	mpChecksums
}

// handleListParts implements ListParts: the parts of an upload in
// ascending part number order after part-number-marker, at most max-parts
// (default and cap 1000) per page. Parts report the checksum of the
// upload's declared algorithm, or every checksum stored for the part when
// none was declared.
func (s *Server) handleListParts(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	maxParts, err := parseMaxKeys(rc.query, "max-parts", maxListParts, maxListParts)
	if err != nil {
		return err
	}
	marker, err := parseMaxKeys(rc.query, "part-number-marker", 0, math.MaxInt32)
	if err != nil {
		return err
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	up, err := s.getUpload(ctx, rc)
	if err != nil {
		return err
	}
	parts, err := s.backend.ListParts(ctx, rc.bucket, rc.key, up.UploadID)
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	algo, typ := uploadChecksum(up.Meta)
	res := listPartsResult{
		Bucket:            rc.bucket,
		Key:               rc.key,
		UploadID:          up.UploadID,
		Initiator:         ownerFor(up.Initiator),
		Owner:             ownerFor(bi.Owner),
		StorageClass:      storageClassOrStandard(up.Meta.StorageClass),
		PartNumberMarker:  marker,
		MaxParts:          maxParts,
		ChecksumAlgorithm: algo,
		ChecksumType:      typ,
		Parts:             []listPart{},
	}
	for _, p := range parts {
		if p.PartNumber <= marker {
			continue
		}
		if len(res.Parts) == maxParts {
			res.IsTruncated = maxParts > 0
			break
		}
		lp := listPart{
			PartNumber:   p.PartNumber,
			LastModified: formatXMLTime(p.LastModified),
			ETag:         quoteETag(p.ETag),
			Size:         p.Size,
		}
		lp.setAll(p.Checksums, algo)
		res.Parts = append(res.Parts, lp)
		res.NextPartNumberMarker = p.PartNumber
	}
	s.setAbortHeaders(ctx, w.Header(), up)
	return writeXML(w, http.StatusOK, res)
}

// --- ListMultipartUploads ---

// listMultipartUploadsResult is the ListMultipartUploads response body.
type listMultipartUploadsResult struct {
	XMLName            xml.Name             `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListMultipartUploadsResult"`
	Bucket             string               `xml:"Bucket"`
	KeyMarker          string               `xml:"KeyMarker"`
	UploadIDMarker     string               `xml:"UploadIdMarker"`
	NextKeyMarker      string               `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string               `xml:"NextUploadIdMarker,omitempty"`
	Delimiter          string               `xml:"Delimiter,omitempty"`
	Prefix             string               `xml:"Prefix"`
	MaxUploads         int                  `xml:"MaxUploads"`
	IsTruncated        bool                 `xml:"IsTruncated"`
	Uploads            []listedUpload       `xml:"Upload"`
	CommonPrefixes     []uploadCommonPrefix `xml:"CommonPrefixes"`
	EncodingType       string               `xml:"EncodingType,omitempty"`
}

// listedUpload is one <Upload> of ListMultipartUploadsResult.
type listedUpload struct {
	Key               string `xml:"Key"`
	UploadID          string `xml:"UploadId"`
	Initiator         Owner  `xml:"Initiator"`
	Owner             Owner  `xml:"Owner"`
	StorageClass      string `xml:"StorageClass"`
	Initiated         string `xml:"Initiated"`
	ChecksumAlgorithm string `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType      string `xml:"ChecksumType,omitempty"`
}

// uploadCommonPrefix is one <CommonPrefixes> entry.
type uploadCommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// handleListMultipartUploads implements ListMultipartUploads (prefix,
// delimiter, key-marker, upload-id-marker, max-uploads <= 1000,
// encoding-type=url; see storage.ListUploads for the paging rules). A page
// is read through the backend's upload walker when it has one, so it costs
// time proportional to max-uploads rather than to the bucket's pending
// uploads.
func (s *Server) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	q := rc.query
	enc, err := parseEncodingType(q)
	if err != nil {
		return err
	}
	maxUploads, err := parseMaxKeys(q, "max-uploads", maxListUploads, maxListUploads)
	if err != nil {
		return err
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	p := storage.UploadListParams{
		Prefix:         q.Get("prefix"),
		Delimiter:      q.Get("delimiter"),
		KeyMarker:      q.Get("key-marker"),
		UploadIDMarker: q.Get("upload-id-marker"),
		MaxUploads:     maxUploads,
	}
	if p.KeyMarker == "" {
		p.UploadIDMarker = ""
	}
	page, err := storage.ListUploads(ctx, s.backend, rc.bucket, p)
	if err != nil {
		return bucketError(err, rc.bucket)
	}

	res := listMultipartUploadsResult{
		Bucket:             rc.bucket,
		KeyMarker:          maybeURLEncode(p.KeyMarker, enc),
		UploadIDMarker:     p.UploadIDMarker,
		NextKeyMarker:      maybeURLEncode(page.NextKeyMarker, enc),
		NextUploadIDMarker: page.NextUploadIDMarker,
		Delimiter:          maybeURLEncode(p.Delimiter, enc),
		Prefix:             maybeURLEncode(p.Prefix, enc),
		MaxUploads:         maxUploads,
		IsTruncated:        page.IsTruncated,
	}
	if enc {
		res.EncodingType = "url"
	}
	owner := ownerFor(bi.Owner)
	for _, u := range page.Uploads {
		algo, typ := uploadChecksum(u.Meta)
		res.Uploads = append(res.Uploads, listedUpload{
			Key:               maybeURLEncode(u.Key, enc),
			UploadID:          u.UploadID,
			Initiator:         ownerFor(u.Initiator),
			Owner:             owner,
			StorageClass:      storageClassOrStandard(u.Meta.StorageClass),
			Initiated:         formatXMLTime(u.Initiated),
			ChecksumAlgorithm: algo,
			ChecksumType:      typ,
		})
	}
	for _, cp := range page.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, uploadCommonPrefix{Prefix: maybeURLEncode(cp, enc)})
	}
	return writeXML(w, http.StatusOK, res)
}
