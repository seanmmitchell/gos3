package s3

// PostObject: browser-based uploads with HTML forms (multipart/form-data).
// Owned by the s3-object work item.

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// PostObject limits.
const (
	// maxPostFieldsSize bounds the total size (names + values) of the form
	// fields preceding the file, as in S3 (20 KiB).
	maxPostFieldsSize = 20 << 10
	// maxPostPreamble bounds the raw bytes (part headers, boundaries,
	// fields, read-ahead) consumed before the file part starts.
	maxPostPreamble = 256 << 10
)

// errPostPreDataTooLarge is returned when the fields before the file are
// too large.
var errPostPreDataTooLarge = &s3err.Error{Code: "MaxPostPreDataLengthExceededError", HTTPStatus: http.StatusBadRequest,
	Message: "Your POST request fields preceding the upload file were too large."}

// postPreambleLimiter caps the bytes read from the request body until the
// file part starts; afterwards the payload reader bounds the file.
type postPreambleLimiter struct {
	r         io.Reader
	remaining int64
	unlimited bool
}

func (l *postPreambleLimiter) Read(p []byte) (int, error) {
	if l.unlimited {
		return l.r.Read(p)
	}
	if l.remaining <= 0 {
		return 0, errPostPreDataTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// postForm is a parsed POST upload form.
type postForm struct {
	fields   map[string]string // lowercased name -> value (fields before the file)
	file     *multipart.Part   // the file part (its content is not read yet)
	filename string            // the file part's file name
}

// malformedPost reports a body that is not valid multipart/form-data.
func malformedPost(err error) error {
	var s3e *s3err.Error
	if errors.As(err, &s3e) {
		return s3e
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return s3err.ErrIncompleteBody
	}
	return s3err.ErrMalformedPOSTRequest
}

// readPostForm parses the form fields up to the file part with bounded
// memory. Fields after the file are ignored, as in S3.
func readPostForm(r *http.Request, body io.Reader) (*postForm, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return nil, s3err.ErrMalformedPOSTRequest
	}
	lim := &postPreambleLimiter{r: body, remaining: maxPostPreamble}
	mr := multipart.NewReader(lim, params["boundary"])
	f := &postForm{fields: map[string]string{}}
	used := 0
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, malformedPost(err)
		}
		name := part.FormName()
		if name == "" {
			return nil, s3err.ErrMalformedPOSTRequest
		}
		lname := strings.ToLower(name)
		if lname == "file" {
			lim.unlimited = true
			f.file, f.filename = part, part.FileName()
			return f, nil
		}
		if _, dup := f.fields[lname]; dup {
			return nil, s3err.ErrInvalidArgument.WithMessage("Bucket POST must contain each form field at most once.").
				With("ArgumentName", truncateValue(name))
		}
		budget := maxPostFieldsSize - used - len(name)
		if budget < 0 {
			return nil, errPostPreDataTooLarge
		}
		val, err := io.ReadAll(io.LimitReader(part, int64(budget)+1))
		if err != nil {
			return nil, malformedPost(err)
		}
		if len(val) > budget {
			return nil, errPostPreDataTooLarge
		}
		used += len(name) + len(val)
		f.fields[lname] = string(val)
	}
	return nil, s3err.ErrInvalidArgument.WithMessage("POST requires exactly one file upload per request.").
		With("ArgumentName", "file").With("ArgumentValue", "0")
}

// postFieldHeaders maps form fields onto the request headers they stand
// for (Content-Type, x-amz-meta-*, x-amz-acl, checksums, ...).
func postFieldHeaders(fields map[string]string) http.Header {
	h := http.Header{}
	for k, v := range fields {
		switch {
		case k == "acl":
			h.Set("X-Amz-Acl", v)
		case k == "content-type", k == "content-disposition", k == "content-encoding", k == "content-language",
			k == "cache-control", k == "expires", k == "content-md5":
			h.Set(k, v)
		case strings.HasPrefix(k, "x-amz-meta-"), strings.HasPrefix(k, "x-amz-checksum-"),
			strings.HasPrefix(k, "x-amz-server-side-encryption"), strings.HasPrefix(k, "x-amz-object-lock-"),
			k == "x-amz-storage-class", k == "x-amz-website-redirect-location":
			h.Set(k, v)
		}
	}
	return h
}

// postPolicyMaxSize returns the upper bound of a policy's
// content-length-range condition (-1 when there is none).
func postPolicyMaxSize(p *auth.PostPolicyConditions) int64 {
	if p == nil {
		return -1
	}
	max := int64(-1)
	for _, c := range p.Conditions {
		arr, ok := c.([]any)
		if !ok || len(arr) != 3 {
			continue
		}
		if op, _ := arr[0].(string); !strings.EqualFold(op, "content-length-range") {
			continue
		}
		var s string
		switch v := arr[2].(type) {
		case json.Number:
			s = v.String()
		case string:
			s = v
		case float64:
			s = strconv.FormatFloat(v, 'f', -1, 64)
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && n >= 0 && (max < 0 || n < max) {
			max = n
		}
	}
	return max
}

// postResponse is the 201 response body of PostObject.
type postResponse struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// handlePostObject implements PostObject (browser form uploads). The form
// is verified with the POST policy (auth.VerifyPostPolicy and
// auth.CheckPostPolicy, including content-length-range against the actual
// file size), the resulting identity is authorized for s3:PutObject
// (anonymous uploads need a bucket policy or ACL), and the file is
// streamed into the bucket with the metadata given by the fields.
func (s *Server) handlePostObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	body := rc.body
	if body == nil {
		body = r.Body
	}
	// Until the policy is verified the client is anonymous: bound the time
	// it may take to send the form fields, not just the idle time.
	if rc.in != nil {
		rc.in.setLimit(time.Now().Add(s.postPreambleTimeout))
	}
	form, err := readPostForm(r, body)
	if err != nil {
		return err
	}
	fields := form.fields

	key, ok := fields["key"]
	if !ok || key == "" {
		return s3err.ErrInvalidArgument.WithMessage("Bucket POST must contain a field named 'key'.  If it is specified, please check the order of the fields.").
			With("ArgumentName", "key")
	}
	key = strings.ReplaceAll(key, "${filename}", form.filename)
	if err := validateNewObjectKey(key); err != nil {
		return err
	}
	fields["key"] = key

	pform := auth.PostPolicyForm(maps.Clone(fields))
	pform["bucket"] = rc.bucket
	id, policy, err := s.verifier.VerifyPostPolicy(pform)
	if err != nil {
		return err
	}
	if err := auth.CheckPostPolicy(policy, pform, -1); err != nil {
		return err
	}
	if rc.in != nil {
		rc.in.setLimit(time.Time{})
	}
	res := *rc.auth
	res.Identity = id
	if policy != nil {
		res.Method = auth.MethodPostPolicy
		// VerifyPostPolicy validated the form's signing time.
		res.SignedAt, _ = time.Parse("20060102T150405Z", fields["x-amz-date"])
	}
	rc.auth, rc.identity, rc.key = &res, id, key

	hdr := postFieldHeaders(fields)
	meta, err := s.objectMetaFromHeaders(hdr)
	if err != nil {
		return err
	}
	tagging := fields["tagging"]
	if tagging != "" {
		var doc Tagging
		if err := decodeXML([]byte(tagging), &doc); err != nil {
			return err
		}
		if meta.Tags, err = validateObjectTags(doc.TagSet.Tags); err != nil {
			return err
		}
	}

	// The form fields stand in for the request headers: the condition keys
	// describing the object (ACL, storage class, encryption, website
	// redirect, object lock, tags) come from the parsed and normalised
	// fields, never from the POST request's own headers.
	acl := hdr.Get("X-Amz-Acl")
	legalHoldSet := hdr.Get("X-Amz-Object-Lock-Legal-Hold") != ""
	attrs := objectAttrConditions(meta, acl != "", legalHoldSet, tagging != "", s.now())
	putTarget := authzTarget{Action: "s3:PutObject", Bucket: rc.bucket, Key: key, ObjectAttrs: attrs}
	if err := s.authorizeTarget(ctx, r, rc, putTarget); err != nil {
		return err
	}
	objOpts := newObjectOpts{
		legalHoldSet: legalHoldSet,
		retentionSet: meta.RetentionMode != "",
		acl:          meta.ACL,
		aclSet:       acl != "",
		tagsSet:      tagging != "",
		attrs:        attrs,
	}
	if err := s.prepareNewObject(ctx, r, rc, bi, key, &meta, objOpts); err != nil {
		return err
	}

	prc := &reqContext{
		body: io.NopCloser(form.file),
		auth: &auth.Result{DecodedContentLength: -1, Trailer: func() http.Header { return http.Header{} }},
	}
	opts := payloadOptions{}
	if max := postPolicyMaxSize(policy); max >= 0 && max < s.maxObjectSize {
		opts.MaxSize = max
		opts.MaxSizeSet = true
	}
	p, err := s.newObjectPayload(&http.Request{Header: hdr}, prc, opts)
	if err != nil {
		return err
	}
	cond := storage.CommitOptions{BeforePublish: func(m, _ *storage.ObjectMeta, created time.Time, config func(string) []byte) error {
		return publishObjectRetention(m, created, config, objOpts.retentionSet)
	}}
	info, err := s.storeObject(ctx, rc.bucket, key, p, -1, meta, cond, func(m *storage.ObjectMeta) error {
		if err := auth.CheckPostPolicy(policy, pform, p.Size()); err != nil {
			return err
		}
		m.ETag = p.ETag()
		m.Checksums = p.Checksums()
		m.ChecksumType = p.ChecksumType()
		// The file may have taken long: the policy's signer (or, for an
		// anonymous form, the bucket's policy) must still allow it.
		return s.revalidateWrite(rc, func() error {
			conds := s.requestTimeConditions(r, rc, attrs)
			again := putTarget
			again.Conditions = conds
			if err := s.authorizeTarget(ctx, r, rc, again); err != nil {
				return err
			}
			return s.authorizeNewObject(ctx, r, rc, rc.bucket, key, m.Tags, objOpts, conds)
		})
	})
	if err != nil {
		return err
	}

	out := w.Header()
	etag := quoteETag(info.ETag)
	out.Set("ETag", etag)
	if bi.Versioning != storage.VersioningUnversioned {
		out.Set("X-Amz-Version-Id", info.VersionID)
	}
	setChecksumHeaders(out, info.Checksums, info.ChecksumType)
	setSSEHeaders(out, info.ObjectMeta)

	redirect := fields["success_action_redirect"]
	if redirect == "" {
		redirect = fields["redirect"]
	}
	if u, err := url.Parse(redirect); redirect != "" && err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		(policy != nil || s.sameOrigin(r, rc, u)) {
		q := u.Query()
		q.Set("bucket", rc.bucket)
		q.Set("key", key)
		q.Set("etag", etag)
		u.RawQuery = q.Encode()
		out.Set("Location", u.String())
		writeEmpty(w, http.StatusSeeOther)
		return nil
	}
	location := s.postObjectLocation(r, rc, key)
	out.Set("Location", location)
	switch strings.TrimSpace(fields["success_action_status"]) {
	case "200":
		writeEmpty(w, http.StatusOK)
	case "201":
		return writeXML(w, http.StatusCreated, postResponse{Location: location, Bucket: rc.bucket, Key: key, ETag: etag})
	default:
		writeNoContent(w)
	}
	return nil
}

// sameOrigin reports whether u has the origin (scheme, host and port) of the
// request r. PostObject only redirects an anonymous, unsigned form there
// (success_action_redirect): a signed POST policy has to cover the
// redirect field, but an unsigned form can be posted by any web page, which
// could otherwise use the server to bounce its visitors anywhere after the
// upload. The form then gets the success_action_status response.
func (s *Server) sameOrigin(r *http.Request, rc *reqContext, u *url.URL) bool {
	scheme := "http"
	if rc.secure {
		scheme = "https"
	}
	return u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil
}

// postObjectLocation returns the URL of an object uploaded by PostObject,
// in the addressing style of the request.
func (s *Server) postObjectLocation(r *http.Request, rc *reqContext, key string) string {
	scheme := "http"
	if rc.secure {
		scheme = "https"
	}
	p := "/" + rc.bucket + "/" + key
	if rc.virtualHost {
		p = "/" + key
	}
	u := url.URL{Scheme: scheme, Host: r.Host, Path: p}
	return u.String()
}
