package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// s3Namespace is the XML namespace of S3 documents.
const s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// xmlHeader starts every XML response body.
const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

// writeXML writes v as an XML response with the given status. The document
// is encoded before anything is written, so an encoding failure is returned
// (and rendered as InternalError by the core) instead of producing a
// truncated response. Response types should carry the S3 namespace, either
// in their XMLName tag or through an `xml:"xmlns,attr"` field.
func writeXML(w http.ResponseWriter, status int, v any) error {
	var buf bytes.Buffer
	buf.WriteString(xmlHeader)
	if err := xml.NewEncoder(&buf).Encode(v); err != nil {
		return fmt.Errorf("s3: encode %T: %w", v, err)
	}
	writeBody(w, status, "application/xml", buf.Bytes())
	return nil
}

// writeBody writes a complete response with a known body.
func writeBody(w http.ResponseWriter, status int, contentType string, body []byte) {
	h := w.Header()
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body) // a failed write means the client is gone
}

// writeNoContent writes an empty 204 response.
func writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// writeEmpty writes an empty response with the given status
// (Content-Length: 0).
func writeEmpty(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// errorDoc is the S3 error response body.
type errorDoc struct {
	Code      string
	Message   string
	Extra     map[string]string
	Resource  string
	RequestID string
	HostID    string
}

// MarshalXML renders <Error> with the extra elements (sorted) between
// Message and Resource, as AWS does.
func (e errorDoc) MarshalXML(enc *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{Name: xml.Name{Local: "Error"}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	el := func(name, val string) error {
		return enc.EncodeElement(val, xml.StartElement{Name: xml.Name{Local: name}})
	}
	if err := el("Code", e.Code); err != nil {
		return err
	}
	if err := el("Message", e.Message); err != nil {
		return err
	}
	names := make([]string, 0, len(e.Extra))
	for k := range e.Extra {
		if validXMLName(k) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		if err := el(k, e.Extra[k]); err != nil {
			return err
		}
	}
	if e.Resource != "" {
		if err := el("Resource", e.Resource); err != nil {
			return err
		}
	}
	if err := el("RequestId", e.RequestID); err != nil {
		return err
	}
	if err := el("HostId", e.HostID); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}

// validXMLName reports whether s is a simple XML element name.
func validXMLName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case i > 0 && (c >= '0' && c <= '9' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// headersClearedOnError are response headers describing a success body that
// must not leak into an error response.
var headersClearedOnError = []string{
	"Content-Length", "Content-Type", "Content-Range", "Content-Encoding",
	"Content-Disposition", "Content-Language", "Content-MD5", "Accept-Ranges",
}

// writeError renders err as an S3 error response. Non-S3 errors are mapped
// with mapStorageError; anything still unknown becomes InternalError and is
// logged. HEAD requests and 304 responses get no body. When the response
// has already started nothing is written (the error is only logged).
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, rc *reqContext, err error) {
	e := s.toS3Error(r.Context(), rc, err)
	rc.errCode = e.Code
	if rw, ok := w.(*responseWriter); ok && rw.wroteHeader {
		s.log.LogAttrs(r.Context(), slog.LevelWarn, "s3: error after response started",
			slog.String("request_id", rc.requestID), slog.String("op", rc.opName), slog.String("error", err.Error()))
		return
	}
	h := w.Header()
	for _, k := range headersClearedOnError {
		h.Del(k)
	}
	status := e.HTTPStatus
	if status < 300 || status > 599 {
		status = http.StatusInternalServerError
	}
	if status != http.StatusNotModified {
		h.Del("ETag")
		h.Del("Last-Modified")
	}
	if size := e.Extra["ActualObjectSize"]; status == http.StatusRequestedRangeNotSatisfiable && size != "" {
		h.Set("Content-Range", "bytes */"+size)
	}
	if r.Method == http.MethodHead || status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	doc := errorDoc{
		Code:      e.Code,
		Message:   e.Message,
		Extra:     e.Extra,
		Resource:  errorResource(r),
		RequestID: rc.requestID,
		HostID:    s.hostID(rc.requestID),
	}
	var buf bytes.Buffer
	buf.WriteString(xmlHeader)
	if xerr := xml.NewEncoder(&buf).Encode(doc); xerr != nil {
		buf.Reset() // cannot happen with string fields; fail safe anyway
		buf.WriteString(xmlHeader + "<Error><Code>InternalError</Code></Error>")
	}
	writeBody(w, status, "application/xml", buf.Bytes())
}

// errorResource is the <Resource> of an error: the decoded request path.
func errorResource(r *http.Request) string {
	if r.URL != nil && r.URL.Path != "" {
		return r.URL.Path
	}
	return "/"
}

// toS3Error converts err to an *s3err.Error and adds the identifying
// elements AWS includes (BucketName, Key, VersionId, UploadId). Unknown
// errors are logged and become InternalError.
func (s *Server) toS3Error(ctx context.Context, rc *reqContext, err error) *s3err.Error {
	var e *s3err.Error
	if !errors.As(err, &e) {
		if !errors.As(mapStorageError(err), &e) {
			s.log.LogAttrs(ctx, slog.LevelError, "s3: internal error",
				slog.String("request_id", rc.requestID), slog.String("op", rc.opName),
				slog.String("bucket", rc.bucket), slog.String("key", rc.key), slog.String("error", err.Error()))
			return s3err.ErrInternalError
		}
	}
	if e.HTTPStatus >= 500 && !errors.Is(err, s3err.ErrNotImplemented) {
		s.log.LogAttrs(ctx, slog.LevelError, "s3: server error",
			slog.String("request_id", rc.requestID), slog.String("op", rc.opName), slog.String("error", err.Error()))
	}
	has := func(k string) bool { _, ok := e.Extra[k]; return ok }
	switch e.Code {
	case s3err.ErrNoSuchBucket.Code, s3err.ErrBucketNotEmpty.Code, s3err.ErrBucketAlreadyExists.Code,
		s3err.ErrBucketAlreadyOwnedByYou.Code, s3err.ErrNoSuchBucketPolicy.Code, s3err.ErrNoSuchCORSConfiguration.Code,
		s3err.ErrNoSuchLifecycleConfiguration.Code, s3err.ErrNoSuchTagSet.Code, s3err.ErrNoSuchWebsiteConfiguration.Code:
		if !has("BucketName") && rc.bucket != "" {
			e = e.With("BucketName", rc.bucket)
		}
	case s3err.ErrNoSuchKey.Code:
		if !has("Key") && rc.key != "" {
			e = e.With("Key", rc.key)
		}
	case s3err.ErrNoSuchVersion.Code:
		if !has("Key") && rc.key != "" {
			e = e.With("Key", rc.key)
		}
		if v := rc.versionID(); !has("VersionId") && v != "" {
			e = e.With("VersionId", v)
		}
	case s3err.ErrNoSuchUpload.Code:
		if v := rc.query.Get("uploadId"); !has("UploadId") && v != "" {
			e = e.With("UploadId", v)
		}
	}
	return e
}

// mapStorageError converts storage sentinel errors (and a few transport
// errors) to S3 errors. *s3err.Error values pass through unchanged; other
// errors are returned unchanged (the core renders them as InternalError).
// storage.ErrConfigNotFound has no generic mapping: handlers map it to the
// configuration-specific code (NoSuchCORSConfiguration, NoSuchTagSet, ...).
func mapStorageError(err error) error {
	if err == nil {
		return nil
	}
	var e *s3err.Error
	if errors.As(err, &e) {
		return e
	}
	var maxBytes *http.MaxBytesError
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		return s3err.ErrNoSuchBucket
	case errors.Is(err, storage.ErrBucketExists):
		return s3err.ErrBucketAlreadyOwnedByYou
	case errors.Is(err, storage.ErrBucketNotEmpty):
		return s3err.ErrBucketNotEmpty
	case errors.Is(err, storage.ErrObjectNotFound):
		return s3err.ErrNoSuchKey
	case errors.Is(err, storage.ErrVersionNotFound):
		return s3err.ErrNoSuchVersion
	case errors.Is(err, storage.ErrUploadNotFound):
		return s3err.ErrNoSuchUpload
	case errors.Is(err, storage.ErrPartNotFound):
		return s3err.ErrInvalidPart
	case errors.Is(err, storage.ErrPreconditionFailed):
		return s3err.ErrPreconditionFailed
	case errors.Is(err, storage.ErrInvalidName):
		return s3err.ErrInvalidArgument.WithMessage("The specified name is not valid.")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return s3err.ErrRequestTimeout
	case errors.As(err, &maxBytes):
		return s3err.ErrMaxMessageLengthExceeded
	case errors.Is(err, io.ErrUnexpectedEOF):
		return s3err.ErrIncompleteBody
	}
	return err
}
