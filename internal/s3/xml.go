package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

// --- Request bodies ---

// maxXMLBodySize is the default bound of XML request bodies that are
// buffered in memory (decodeXMLBody). The documents that list many entries
// (DeleteObjects, CompleteMultipartUpload) are streamed instead; see
// xml_stream.go.
const maxXMLBodySize = 2 << 20

// readBody reads a whole request body of at most max bytes. A larger body
// yields MaxMessageLengthExceeded; read errors from the authenticated body
// (signature/hash failures) are returned unchanged.
func readBody(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, mapStorageError(err)
	}
	if int64(len(data)) > max {
		return nil, s3err.ErrMaxMessageLengthExceeded
	}
	return data, nil
}

// decodeXMLBody reads at most max bytes (<= 0 means maxXMLBodySize) from r
// and unmarshals them into v. An empty body yields MissingRequestBodyError,
// a malformed document (or one whose root element does not match v's
// XMLName) MalformedXML.
func decodeXMLBody(r io.Reader, max int64, v any) error {
	if max <= 0 {
		max = maxXMLBodySize
	}
	data, err := readBody(r, max)
	if err != nil {
		return err
	}
	return decodeXML(data, v)
}

// decodeXML unmarshals data into v (see decodeXMLBody).
func decodeXML(data []byte, v any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return s3err.ErrMissingRequestBody
	}
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(v); err != nil {
		return s3err.ErrMalformedXML
	}
	return nil
}

// --- Owner ---

// Owner is the S3 <Owner> element. gos3 identifies owners by IAM user name:
// ID is the hex SHA-256 of the user name and DisplayName the user name;
// buckets and objects created by root (owner "") belong to "root".
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName,omitempty"`
}

// ownerFor returns the Owner of a user name ("" means root).
func ownerFor(user string) Owner {
	if user == "" {
		user = auth.RootUser
	}
	sum := sha256.Sum256([]byte(user))
	return Owner{ID: hex.EncodeToString(sum[:]), DisplayName: user}
}

// --- Times ---

// formatXMLTime formats t for XML bodies (RFC 3339, UTC, milliseconds).
func formatXMLTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// formatHTTPTime formats t for HTTP headers (RFC 1123, GMT).
func formatHTTPTime(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

// parseHTTPTime parses an HTTP date (RFC 1123, RFC 850 or ANSI C).
func parseHTTPTime(s string) (time.Time, bool) {
	t, err := http.ParseTime(strings.TrimSpace(s))
	return t, err == nil
}

// parseISOTime parses an ISO 8601 / RFC 3339 timestamp as used by object
// lock dates (with or without fractional seconds).
func parseISOTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z0700", "2006-01-02T15:04:05Z0700", "20060102T150405Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// --- encoding-type=url ---

// parseEncodingType validates the encoding-type list parameter: "" (none)
// or "url" (case-insensitive). It reports whether keys must be URL-encoded.
func parseEncodingType(q url.Values) (bool, error) {
	v := q.Get("encoding-type")
	switch {
	case v == "":
		return false, nil
	case strings.EqualFold(v, "url"):
		return true, nil
	}
	return false, s3err.ErrInvalidArgument.WithMessage("Invalid Encoding Method specified in Request").
		With("ArgumentName", "encoding-type").With("ArgumentValue", v)
}

// s3URLEncode encodes s like AWS does for encoding-type=url: unreserved
// characters and '/' are kept, space becomes '+', everything else is
// percent-encoded (uppercase hex) byte by byte.
func s3URLEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

// maybeURLEncode returns s3URLEncode(s) when enc is true, else s.
func maybeURLEncode(s string, enc bool) string {
	if enc {
		return s3URLEncode(s)
	}
	return s
}

// --- Tagging ---

// Tag limits.
const (
	maxObjectTags     = 10
	maxBucketTags     = 50
	maxTagKeyLength   = 128
	maxTagValueLength = 256
)

// Tagging is the <Tagging> document of object and bucket tagging. Set
// XMLNS to s3Namespace when writing it.
type Tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	TagSet  TagSet   `xml:"TagSet"`
}

// TagSet is the <TagSet> element.
type TagSet struct {
	Tags []Tag `xml:"Tag"`
}

// Tag is one key/value tag.
type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// taggingFromMap returns a Tagging document (namespace set) with tags
// sorted by key.
func taggingFromMap(tags map[string]string) Tagging {
	t := Tagging{XMLNS: s3Namespace, TagSet: TagSet{Tags: make([]Tag, 0, len(tags))}}
	for k, v := range tags {
		t.TagSet.Tags = append(t.TagSet.Tags, Tag{Key: k, Value: v})
	}
	sort.Slice(t.TagSet.Tags, func(i, j int) bool { return t.TagSet.Tags[i].Key < t.TagSet.Tags[j].Key })
	return t
}

// validateObjectTags validates an object tag set and returns it as a map.
// More than 10 tags is BadRequest on AWS (not InvalidTag), both for the
// x-amz-tagging header and for PutObjectTagging.
func validateObjectTags(tags []Tag) (map[string]string, error) {
	if len(tags) > maxObjectTags {
		return nil, s3err.ErrBadRequest.WithMessage("Object tags cannot be greater than 10")
	}
	return validateTags(tags, maxObjectTags, "Object tags cannot be greater than 10")
}

// validateBucketTags validates a bucket tag set and returns it as a map.
func validateBucketTags(tags []Tag) (map[string]string, error) {
	return validateTags(tags, maxBucketTags, "Bucket tag count cannot be greater than 50")
}

func validateTags(tags []Tag, max int, tooMany string) (map[string]string, error) {
	if len(tags) > max {
		return nil, s3err.ErrInvalidTag.WithMessage("%s", tooMany)
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		if err := validateTag(t.Key, t.Value); err != nil {
			return nil, err
		}
		if _, dup := m[t.Key]; dup {
			return nil, s3err.ErrInvalidTag.WithMessage("Cannot provide multiple Tags with the same key")
		}
		m[t.Key] = t.Value
	}
	return m, nil
}

// validateTag checks one tag: key 1..128 and value 0..256 Unicode
// characters from letters, digits, whitespace and _ . : / = + - @, and no
// reserved "aws:" key prefix.
func validateTag(key, value string) error {
	switch n := len([]rune(key)); {
	case n == 0:
		return s3err.ErrInvalidTag.WithMessage("The TagKey you have provided is invalid")
	case n > maxTagKeyLength:
		return s3err.ErrInvalidTag.WithMessage("The TagKey you have provided is too long, max 128")
	}
	if len([]rune(value)) > maxTagValueLength {
		return s3err.ErrInvalidTag.WithMessage("The TagValue you have provided is too long, max 256")
	}
	if !validTagChars(key) {
		return s3err.ErrInvalidTag.WithMessage("The TagKey you have provided is invalid")
	}
	if !validTagChars(value) {
		return s3err.ErrInvalidTag.WithMessage("The TagValue you have provided is invalid")
	}
	if strings.HasPrefix(strings.ToLower(key), "aws:") {
		return s3err.ErrInvalidTag.WithMessage("Your TagKey cannot be prefixed with aws:")
	}
	return nil
}

// parseTaggingHeader parses an x-amz-tagging header (URL query format,
// e.g. "a=1&b=2") into a validated object tag map.
func parseTaggingHeader(v string) (map[string]string, error) {
	if strings.TrimSpace(v) == "" {
		return map[string]string{}, nil
	}
	bad := s3err.ErrInvalidArgument.WithMessage("The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.").
		With("ArgumentName", "x-amz-tagging").With("ArgumentValue", v)
	vals, err := url.ParseQuery(v)
	if err != nil {
		return nil, bad
	}
	tags := make([]Tag, 0, len(vals))
	for k, vs := range vals {
		if len(vs) != 1 {
			return nil, bad
		}
		tags = append(tags, Tag{Key: k, Value: vs[0]})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Key < tags[j].Key })
	return validateObjectTags(tags)
}

// --- Public access block ---

// PublicAccessBlockConfiguration is the <PublicAccessBlockConfiguration>
// document (storage.ConfigPublicAccessBlock). Set XMLNS when writing it.
type PublicAccessBlockConfiguration struct {
	XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
	XMLNS                 string   `xml:"xmlns,attr,omitempty"`
	BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
	IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
	BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
	RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
}

// parsePublicAccessBlock parses a PublicAccessBlockConfiguration document.
func parsePublicAccessBlock(data []byte) (*PublicAccessBlockConfiguration, error) {
	var c PublicAccessBlockConfiguration
	if err := decodeXML(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// --- ACLs ---

// Canned ACL grantee group URIs.
const (
	groupAllUsers           = "http://acs.amazonaws.com/groups/global/AllUsers"
	groupAuthenticatedUsers = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
	groupLogDelivery        = "http://acs.amazonaws.com/groups/s3/LogDelivery"
	xsiNamespace            = "http://www.w3.org/2001/XMLSchema-instance"
)

// AccessControlPolicy is the <AccessControlPolicy> document of Get/Put
// Bucket/Object ACL. Set XMLNS when writing it.
type AccessControlPolicy struct {
	XMLName xml.Name `xml:"AccessControlPolicy"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Owner   *Owner   `xml:"Owner,omitempty"`
	Grants  []Grant  `xml:"AccessControlList>Grant"`
}

// Grant is one ACL grant.
type Grant struct {
	Grantee    Grantee `xml:"Grantee"`
	Permission string  `xml:"Permission"` // FULL_CONTROL, READ, WRITE, READ_ACP, WRITE_ACP
}

// Grantee is an ACL grantee; Type is the xsi:type ("CanonicalUser",
// "Group" or "AmazonCustomerByEmail").
type Grantee struct {
	Type         string
	ID           string
	DisplayName  string
	URI          string
	EmailAddress string
}

type granteeXML struct {
	ID           string `xml:"ID,omitempty"`
	DisplayName  string `xml:"DisplayName,omitempty"`
	URI          string `xml:"URI,omitempty"`
	EmailAddress string `xml:"EmailAddress,omitempty"`
}

// MarshalXML writes the grantee with literal xmlns:xsi / xsi:type
// attributes, as AWS does (SDKs match the prefixed attribute name).
func (g Grantee) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = []xml.Attr{
		{Name: xml.Name{Local: "xmlns:xsi"}, Value: xsiNamespace},
		{Name: xml.Name{Local: "xsi:type"}, Value: g.Type},
	}
	return e.EncodeElement(granteeXML{ID: g.ID, DisplayName: g.DisplayName, URI: g.URI, EmailAddress: g.EmailAddress}, start)
}

// UnmarshalXML reads the grantee and its xsi:type attribute.
func (g *Grantee) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var x granteeXML
	if err := d.DecodeElement(&x, &start); err != nil {
		return err
	}
	*g = Grantee{ID: x.ID, DisplayName: x.DisplayName, URI: x.URI, EmailAddress: x.EmailAddress}
	for _, a := range start.Attr {
		if a.Name.Local == "type" || a.Name.Local == "xsi:type" {
			g.Type = a.Value
		}
	}
	return nil
}

// Canned ACL names.
const (
	aclPrivate                = "private"
	aclPublicRead             = "public-read"
	aclPublicReadWrite        = "public-read-write"
	aclAuthenticatedRead      = "authenticated-read"
	aclAWSExecRead            = "aws-exec-read"
	aclBucketOwnerRead        = "bucket-owner-read"
	aclBucketOwnerFullControl = "bucket-owner-full-control"
	aclLogDeliveryWrite       = "log-delivery-write"
)

// aclPolicyForCanned renders a canned ACL as the equivalent
// AccessControlPolicy owned by owner (namespace set).
func aclPolicyForCanned(canned string, owner Owner) AccessControlPolicy {
	o := owner
	p := AccessControlPolicy{XMLNS: s3Namespace, Owner: &o}
	p.Grants = append(p.Grants, Grant{
		Grantee:    Grantee{Type: "CanonicalUser", ID: owner.ID, DisplayName: owner.DisplayName},
		Permission: "FULL_CONTROL",
	})
	group := func(uri, perm string) {
		p.Grants = append(p.Grants, Grant{Grantee: Grantee{Type: "Group", URI: uri}, Permission: perm})
	}
	switch strings.ToLower(canned) {
	case aclPublicRead:
		group(groupAllUsers, "READ")
	case aclPublicReadWrite:
		group(groupAllUsers, "READ")
		group(groupAllUsers, "WRITE")
	case aclAuthenticatedRead:
		group(groupAuthenticatedUsers, "READ")
	case aclLogDeliveryWrite:
		group(groupLogDelivery, "WRITE")
		group(groupLogDelivery, "READ_ACP")
	}
	return p
}

// cannedFromACLPolicy derives the canned ACL whose grants an
// AccessControlPolicy is equivalent to for authorization purposes:
// AllUsers READ (+WRITE) => public-read (public-read-write), else
// AuthenticatedUsers READ => authenticated-read, else private ("").
func cannedFromACLPolicy(p *AccessControlPolicy) string {
	var allRead, allWrite, authRead bool
	for _, g := range p.Grants {
		uri := strings.TrimSpace(g.Grantee.URI)
		perm := strings.ToUpper(strings.TrimSpace(g.Permission))
		switch uri {
		case groupAllUsers:
			allRead = allRead || perm == "READ" || perm == "FULL_CONTROL"
			allWrite = allWrite || perm == "WRITE" || perm == "FULL_CONTROL"
		case groupAuthenticatedUsers:
			authRead = authRead || perm == "READ" || perm == "FULL_CONTROL"
		}
	}
	switch {
	case allRead && allWrite:
		return aclPublicReadWrite
	case allRead:
		return aclPublicRead
	case authRead:
		return aclAuthenticatedRead
	}
	return ""
}

// cannedACLFromConfig interprets a stored storage.ConfigACL document (a
// canned ACL name or AccessControlPolicy XML) as a canned ACL ("" =
// private).
func cannedACLFromConfig(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "<") {
		var p AccessControlPolicy
		if xml.Unmarshal([]byte(s), &p) != nil {
			return ""
		}
		return cannedFromACLPolicy(&p)
	}
	s = strings.ToLower(s)
	if s == aclPrivate {
		return ""
	}
	return s
}

// aclsDisabledByOwnership reports whether a stored OwnershipControls
// document sets ObjectOwnership BucketOwnerEnforced (ACLs disabled).
func aclsDisabledByOwnership(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var oc struct {
		Rules []struct {
			ObjectOwnership string `xml:"ObjectOwnership"`
		} `xml:"Rule"`
	}
	if xml.Unmarshal(raw, &oc) != nil {
		return bytes.Contains(raw, []byte("BucketOwnerEnforced"))
	}
	for _, r := range oc.Rules {
		if strings.TrimSpace(r.ObjectOwnership) == "BucketOwnerEnforced" {
			return true
		}
	}
	return false
}
