package s3

// ListParts and ListMultipartUploads. Owned by the s3-multipart work item.

import (
	"encoding/xml"
	"math"
	"net/http"
	"strings"

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

// uploadListParams are the ListMultipartUploads query parameters.
type uploadListParams struct {
	prefix, delimiter, keyMarker, uploadIDMarker string
	maxUploads                                   int
}

// uploadListing is a page of in-progress uploads and common prefixes.
type uploadListing struct {
	uploads                           []storage.UploadInfo
	prefixes                          []string
	truncated                         bool
	nextKeyMarker, nextUploadIDMarker string
}

// listUploads pages through uploads (sorted by key, then initiation, as
// storage.Backend.ListMultipartUploads returns them) with S3 semantics:
//
//   - only keys with the prefix are listed; with a delimiter, keys whose
//     remainder after the prefix contains it roll up into one common
//     prefix each (counting as one entry toward maxUploads);
//   - key-marker alone starts after every upload of that key; with
//     upload-id-marker the uploads of key-marker following that upload
//     are included too (those with a greater upload ID when the marker
//     upload no longer exists); upload-id-marker without key-marker is
//     ignored; a key-marker equal to one of the listing's common prefixes
//     skips everything under it;
//   - when truncated, the next markers identify the last entry returned
//     (a common prefix has an empty upload ID marker).
func listUploads(all []storage.UploadInfo, p uploadListParams) uploadListing {
	var out uploadListing
	if p.maxUploads <= 0 {
		return out
	}
	skipUnder := ""
	if cp, ok := storage.CommonPrefix(p.keyMarker, p.prefix, p.delimiter); ok && cp == p.keyMarker {
		skipUnder = p.keyMarker
	}
	markerFound := false
	if p.keyMarker != "" && p.uploadIDMarker != "" {
		for _, u := range all {
			if u.Key == p.keyMarker && u.UploadID == p.uploadIDMarker {
				markerFound = true
				break
			}
		}
	}
	pastMarker := false
	lastCP := ""
	count := 0
	for _, u := range all {
		if !strings.HasPrefix(u.Key, p.prefix) {
			continue
		}
		if p.keyMarker != "" {
			if skipUnder != "" && strings.HasPrefix(u.Key, skipUnder) {
				continue
			}
			if u.Key < p.keyMarker {
				continue
			}
			if u.Key == p.keyMarker {
				if p.uploadIDMarker == "" {
					continue
				}
				if markerFound {
					if !pastMarker {
						pastMarker = u.UploadID == p.uploadIDMarker
						continue
					}
				} else if u.UploadID <= p.uploadIDMarker {
					continue
				}
			}
		}
		cp, isCP := storage.CommonPrefix(u.Key, p.prefix, p.delimiter)
		if isCP && cp == lastCP {
			continue
		}
		if count == p.maxUploads {
			out.truncated = true
			break
		}
		count++
		if isCP {
			out.prefixes = append(out.prefixes, cp)
			lastCP = cp
			out.nextKeyMarker, out.nextUploadIDMarker = cp, ""
			continue
		}
		out.uploads = append(out.uploads, u)
		out.nextKeyMarker, out.nextUploadIDMarker = u.Key, u.UploadID
	}
	if !out.truncated {
		out.nextKeyMarker, out.nextUploadIDMarker = "", ""
	}
	return out
}

// handleListMultipartUploads implements ListMultipartUploads (prefix,
// delimiter, key-marker, upload-id-marker, max-uploads <= 1000,
// encoding-type=url; see listUploads for the paging rules).
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
	p := uploadListParams{
		prefix:         q.Get("prefix"),
		delimiter:      q.Get("delimiter"),
		keyMarker:      q.Get("key-marker"),
		uploadIDMarker: q.Get("upload-id-marker"),
		maxUploads:     maxUploads,
	}
	if p.keyMarker == "" {
		p.uploadIDMarker = ""
	}
	all, err := s.backend.ListMultipartUploads(ctx, rc.bucket)
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	page := listUploads(all, p)

	res := listMultipartUploadsResult{
		Bucket:             rc.bucket,
		KeyMarker:          maybeURLEncode(p.keyMarker, enc),
		UploadIDMarker:     p.uploadIDMarker,
		NextKeyMarker:      maybeURLEncode(page.nextKeyMarker, enc),
		NextUploadIDMarker: page.nextUploadIDMarker,
		Delimiter:          maybeURLEncode(p.delimiter, enc),
		Prefix:             maybeURLEncode(p.prefix, enc),
		MaxUploads:         maxUploads,
		IsTruncated:        page.truncated,
	}
	if enc {
		res.EncodingType = "url"
	}
	owner := ownerFor(bi.Owner)
	for _, u := range page.uploads {
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
	for _, cp := range page.prefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, uploadCommonPrefix{Prefix: maybeURLEncode(cp, enc)})
	}
	return writeXML(w, http.StatusOK, res)
}
