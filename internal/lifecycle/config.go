// Package lifecycle implements S3 bucket lifecycle configuration
// (PutBucketLifecycleConfiguration / GetBucketLifecycleConfiguration):
// parsing and AWS-compatible validation of the LifecycleConfiguration XML
// document, the date arithmetic and rule matching S3 uses to decide when an
// object expires, and a background Worker that applies the expiration actions
// to the buckets of a storage.Backend.
//
// Supported (executed) actions:
//
//   - Expiration (Days or Date) of current object versions. In versioned or
//     versioning-suspended buckets this adds a delete marker; in unversioned
//     buckets the object is removed.
//   - Expiration ExpiredObjectDeleteMarker, and the implicit clean-up of
//     expired object delete markers performed by Days-based Expiration.
//   - NoncurrentVersionExpiration (NoncurrentDays, NewerNoncurrentVersions).
//   - AbortIncompleteMultipartUpload (DaysAfterInitiation).
//
// Transition and NoncurrentVersionTransition actions are parsed, validated
// and stored so that clients can round-trip their configuration, but gos3 has
// a single storage class and never executes them.
package lifecycle

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// Namespace is the S3 XML namespace written by Marshal.
const Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// Limits enforced by Parse.
const (
	// MaxRules is the maximum number of rules in a configuration.
	MaxRules = 1000
	// MaxIDLength is the maximum length of a rule ID, in characters.
	MaxIDLength = 255
	// MaxConfigSize bounds the size of a LifecycleConfiguration document
	// accepted by Parse. HTTP handlers should limit the request body to this
	// many bytes.
	MaxConfigSize = 2 << 20
	// MaxNewerNoncurrentVersions is the maximum value of
	// NewerNoncurrentVersions.
	MaxNewerNoncurrentVersions = 100
	// MaxTagKeyLength and MaxTagValueLength bound filter tags (characters).
	MaxTagKeyLength   = 128
	MaxTagValueLength = 256
)

// Rule status values.
const (
	StatusEnabled  = "Enabled"
	StatusDisabled = "Disabled"
)

// TransitionStorageClasses are the storage classes accepted in Transition
// and NoncurrentVersionTransition actions.
var TransitionStorageClasses = []string{
	"GLACIER", "STANDARD_IA", "ONEZONE_IA", "INTELLIGENT_TIERING", "DEEP_ARCHIVE", "GLACIER_IR",
}

// Configuration is an S3 LifecycleConfiguration document.
type Configuration struct {
	XMLName xml.Name `xml:"LifecycleConfiguration"`
	Rules   []Rule   `xml:"Rule"`
}

// Rule is one lifecycle rule. Exactly one of Filter and the deprecated
// top-level Prefix must be set (Validate enforces it); an empty Filter or an
// empty Prefix applies the rule to every object in the bucket.
type Rule struct {
	ID     string  `xml:"ID,omitempty"`
	Filter *Filter `xml:"Filter"`
	// Prefix is the legacy (pre-Filter) rule-level key prefix.
	Prefix                         *string                         `xml:"Prefix"`
	Status                         string                          `xml:"Status"`
	Expiration                     *Expiration                     `xml:"Expiration"`
	Transitions                    []Transition                    `xml:"Transition"`
	NoncurrentVersionTransitions   []NoncurrentVersionTransition   `xml:"NoncurrentVersionTransition"`
	NoncurrentVersionExpiration    *NoncurrentVersionExpiration    `xml:"NoncurrentVersionExpiration"`
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload `xml:"AbortIncompleteMultipartUpload"`
}

// Filter selects the objects a rule applies to. At most one field may be
// set; an empty Filter matches every object.
type Filter struct {
	Prefix                *string `xml:"Prefix"`
	Tag                   *Tag    `xml:"Tag"`
	ObjectSizeGreaterThan *int64  `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64  `xml:"ObjectSizeLessThan"`
	And                   *And    `xml:"And"`
}

// And combines several filter predicates; an object must satisfy all.
type And struct {
	Prefix                *string `xml:"Prefix"`
	Tags                  []Tag   `xml:"Tag"`
	ObjectSizeGreaterThan *int64  `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64  `xml:"ObjectSizeLessThan"`
}

// Tag is an object tag predicate: the object must carry Key with exactly
// Value.
type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// Expiration expires current object versions (Days or Date) or removes
// expired object delete markers (ExpiredObjectDeleteMarker). Exactly one
// field is set.
type Expiration struct {
	Days                      *int  `xml:"Days"`
	Date                      *Date `xml:"Date"`
	ExpiredObjectDeleteMarker *bool `xml:"ExpiredObjectDeleteMarker"`
}

// Transition moves current versions to another storage class. gos3 stores
// but never executes transitions.
type Transition struct {
	Days         *int   `xml:"Days"`
	Date         *Date  `xml:"Date"`
	StorageClass string `xml:"StorageClass"`
}

// NoncurrentVersionTransition moves noncurrent versions to another storage
// class. gos3 stores but never executes transitions.
type NoncurrentVersionTransition struct {
	NoncurrentDays          *int   `xml:"NoncurrentDays"`
	StorageClass            string `xml:"StorageClass"`
	NewerNoncurrentVersions *int   `xml:"NewerNoncurrentVersions"`
}

// NoncurrentVersionExpiration permanently deletes noncurrent versions
// NoncurrentDays after they became noncurrent, retaining the
// NewerNoncurrentVersions newest noncurrent versions.
type NoncurrentVersionExpiration struct {
	NoncurrentDays          *int `xml:"NoncurrentDays"`
	NewerNoncurrentVersions *int `xml:"NewerNoncurrentVersions"`
}

// AbortIncompleteMultipartUpload aborts multipart uploads that were not
// completed within DaysAfterInitiation days.
type AbortIncompleteMultipartUpload struct {
	DaysAfterInitiation *int `xml:"DaysAfterInitiation"`
}

// Date is a lifecycle action date. S3 requires it to be midnight UTC; it is
// written as "2006-01-02T00:00:00.000Z".
type Date struct{ time.Time }

// dateLayout is the format S3 uses when returning lifecycle dates.
const dateLayout = "2006-01-02T15:04:05.000Z"

// MarshalText implements encoding.TextMarshaler.
func (d Date) MarshalText() ([]byte, error) {
	return []byte(d.UTC().Format(dateLayout)), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. It accepts ISO 8601
// timestamps (RFC 3339, with or without fractional seconds or a zone offset)
// and plain dates ("2006-01-02", interpreted as midnight UTC). Whether the
// value is at midnight UTC is checked by Validate.
func (d *Date) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			d.Time = t.UTC()
			return nil
		}
	}
	return s3err.ErrMalformedXML.WithMessage("The XML you provided was not well-formed or did not validate against our published schema (invalid Date %q).", truncate(s, 64))
}

// isMidnightUTC reports whether t is exactly 00:00:00 UTC.
func isMidnightUTC(t time.Time) bool {
	t = t.UTC()
	return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
}

// Parse decodes and validates a LifecycleConfiguration document the way S3's
// PutBucketLifecycleConfiguration does. Rules without an ID get a generated
// unique one, so callers should persist the result of Marshal rather than the
// raw request body. Errors are *s3err.Error values (MalformedXML,
// InvalidArgument, InvalidRequest, InvalidTag or MaxMessageLengthExceeded).
func Parse(data []byte) (*Configuration, error) {
	if len(data) > MaxConfigSize {
		return nil, s3err.ErrMaxMessageLengthExceeded.WithMessage("The lifecycle configuration exceeds the maximum allowed size of %d bytes.", MaxConfigSize)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, s3err.ErrMalformedXML
	}
	if err := checkSchema(data); err != nil {
		return nil, err
	}
	var c Configuration
	if err := xml.Unmarshal(data, &c); err != nil {
		var se *s3err.Error
		if errors.As(err, &se) {
			return nil, se
		}
		return nil, s3err.ErrMalformedXML
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.assignIDs(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Marshal returns the canonical XML encoding of c (XML declaration, S3
// namespace). It does not validate c.
func (c *Configuration) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	enc := xml.NewEncoder(&buf)
	start := xml.StartElement{
		Name: xml.Name{Local: "LifecycleConfiguration"},
		Attr: []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: Namespace}},
	}
	if err := enc.EncodeElement(rulesOnly{Rules: c.Rules}, start); err != nil {
		return nil, fmt.Errorf("lifecycle: marshal: %w", err)
	}
	if err := enc.Flush(); err != nil {
		return nil, fmt.Errorf("lifecycle: marshal: %w", err)
	}
	return buf.Bytes(), nil
}

// rulesOnly is the element body written by Marshal.
type rulesOnly struct {
	Rules []Rule `xml:"Rule"`
}

// MarshalXML writes the rule's children in the order S3 uses.
func (r Rule) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	type ordered struct {
		ID                             string                          `xml:"ID,omitempty"`
		Filter                         *Filter                         `xml:"Filter"`
		Prefix                         *string                         `xml:"Prefix"`
		Status                         string                          `xml:"Status"`
		Transitions                    []Transition                    `xml:"Transition"`
		NoncurrentVersionTransitions   []NoncurrentVersionTransition   `xml:"NoncurrentVersionTransition"`
		Expiration                     *Expiration                     `xml:"Expiration"`
		NoncurrentVersionExpiration    *NoncurrentVersionExpiration    `xml:"NoncurrentVersionExpiration"`
		AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload `xml:"AbortIncompleteMultipartUpload"`
	}
	return e.EncodeElement(ordered{
		ID:                             r.ID,
		Filter:                         r.Filter,
		Prefix:                         r.Prefix,
		Status:                         r.Status,
		Transitions:                    r.Transitions,
		NoncurrentVersionTransitions:   r.NoncurrentVersionTransitions,
		Expiration:                     r.Expiration,
		NoncurrentVersionExpiration:    r.NoncurrentVersionExpiration,
		AbortIncompleteMultipartUpload: r.AbortIncompleteMultipartUpload,
	}, start)
}

// assignIDs gives every rule without an ID a random unique one (S3 does the
// same).
func (c *Configuration) assignIDs() error {
	used := make(map[string]bool, len(c.Rules))
	for _, r := range c.Rules {
		used[r.ID] = true
	}
	for i := range c.Rules {
		if c.Rules[i].ID != "" {
			continue
		}
		for {
			id, err := generateID()
			if err != nil {
				return s3err.ErrInternalError
			}
			if !used[id] {
				used[id] = true
				c.Rules[i].ID = id
				break
			}
		}
	}
	return nil
}

// generateID returns a random 32-character rule ID.
func generateID() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Validate checks c against the S3 rules for lifecycle configurations. Rules
// with an empty ID are accepted (Parse assigns one). The returned error is an
// *s3err.Error.
func (c *Configuration) Validate() error {
	if len(c.Rules) == 0 {
		return s3err.ErrMalformedXML
	}
	if len(c.Rules) > MaxRules {
		return s3err.ErrInvalidRequest.WithMessage("The number of lifecycle rules should not exceed allowed limit of %d rules", MaxRules)
	}
	ids := make(map[string]bool, len(c.Rules))
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.ID != "" {
			if ids[r.ID] {
				return s3err.ErrInvalidArgument.WithMessage("Rule ID must be unique. Found same ID for more than one rule").
					With("ArgumentName", "ID").With("ArgumentValue", truncate(r.ID, MaxIDLength))
			}
			ids[r.ID] = true
		}
		if err := r.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r *Rule) validate() error {
	if utf8.RuneCountInString(r.ID) > MaxIDLength {
		return s3err.ErrInvalidArgument.WithMessage("ID length should not exceed allowed limit of %d", MaxIDLength).
			With("ArgumentName", "ID").With("ArgumentValue", truncate(r.ID, MaxIDLength))
	}
	if r.Status != StatusEnabled && r.Status != StatusDisabled {
		return s3err.ErrMalformedXML
	}
	// Exactly one of Filter and the legacy Prefix is required (an empty
	// <Filter/> or <Prefix/> selects every object).
	if (r.Filter == nil) == (r.Prefix == nil) {
		return s3err.ErrMalformedXML
	}
	if r.Filter != nil {
		if err := r.Filter.validate(); err != nil {
			return err
		}
	}
	if r.Expiration == nil && len(r.Transitions) == 0 && len(r.NoncurrentVersionTransitions) == 0 &&
		r.NoncurrentVersionExpiration == nil && r.AbortIncompleteMultipartUpload == nil {
		return s3err.ErrInvalidRequest.WithMessage("At least one action needs to be specified in a rule")
	}
	hasTags := r.hasTagFilter()
	desc := r.describe()

	var daysBased, dateBased bool
	if e := r.Expiration; e != nil {
		set := 0
		if e.Days != nil {
			set++
		}
		if e.Date != nil {
			set++
		}
		if set == 0 && e.ExpiredObjectDeleteMarker == nil {
			return s3err.ErrMalformedXML
		}
		if set > 1 {
			return s3err.ErrMalformedXML
		}
		if e.ExpiredObjectDeleteMarker != nil && set > 0 {
			return s3err.ErrInvalidArgument.WithMessage("ExpiredObjectDeleteMarker cannot be specified with Days or Date in a Lifecycle Expiration Policy.")
		}
		if e.Days != nil {
			if *e.Days <= 0 || *e.Days > math.MaxInt32 {
				return s3err.ErrInvalidArgument.WithMessage("'Days' for Expiration action must be a positive integer")
			}
			daysBased = true
		}
		if e.Date != nil {
			if !isMidnightUTC(e.Date.Time) {
				return s3err.ErrInvalidArgument.WithMessage("'Date' must be at midnight GMT")
			}
			dateBased = true
		}
		if e.ExpiredObjectDeleteMarker != nil && *e.ExpiredObjectDeleteMarker && hasTags {
			return s3err.ErrInvalidRequest.WithMessage("ExpiredObjectDeleteMarker cannot be specified with Tag-based filters.")
		}
	}

	seenClass := map[string]bool{}
	maxTransitionDays := -1
	var latestTransitionDate time.Time
	for _, t := range r.Transitions {
		if (t.Days == nil) == (t.Date == nil) {
			return s3err.ErrMalformedXML
		}
		if !validStorageClass(t.StorageClass) {
			return s3err.ErrMalformedXML
		}
		if seenClass[t.StorageClass] {
			return s3err.ErrInvalidRequest.WithMessage("'StorageClass' must be different for 'Transition' actions in same 'Rule' with %s", desc)
		}
		seenClass[t.StorageClass] = true
		if t.Days != nil {
			if *t.Days < 0 || *t.Days > math.MaxInt32 {
				return s3err.ErrInvalidArgument.WithMessage("'Days' in Transition action must be nonnegative")
			}
			if (t.StorageClass == "STANDARD_IA" || t.StorageClass == "ONEZONE_IA") && *t.Days < 30 {
				return s3err.ErrInvalidArgument.WithMessage("'Days' in Transition action must be greater than or equal to 30 for storageClass '%s'", t.StorageClass)
			}
			daysBased = true
			maxTransitionDays = max(maxTransitionDays, *t.Days)
		}
		if t.Date != nil {
			if !isMidnightUTC(t.Date.Time) {
				return s3err.ErrInvalidArgument.WithMessage("'Date' must be at midnight GMT")
			}
			dateBased = true
			if t.Date.After(latestTransitionDate) {
				latestTransitionDate = t.Date.Time
			}
		}
	}
	if daysBased && dateBased {
		return s3err.ErrInvalidRequest.WithMessage("Found mixed 'Date' and 'Days' based Expiration and Transition actions in lifecycle rule for %s", desc)
	}
	if e := r.Expiration; e != nil {
		if e.Days != nil && maxTransitionDays >= 0 && *e.Days <= maxTransitionDays {
			return s3err.ErrInvalidArgument.WithMessage("'Days' in the Expiration action for %s must be greater than 'Days' in the Transition action", desc)
		}
		if e.Date != nil && !latestTransitionDate.IsZero() && !e.Date.After(latestTransitionDate) {
			return s3err.ErrInvalidArgument.WithMessage("'Date' in the Expiration action for %s must be later than 'Date' in the Transition action", desc)
		}
	}

	seenNCClass := map[string]bool{}
	maxNCTransitionDays := -1
	for _, t := range r.NoncurrentVersionTransitions {
		if t.NoncurrentDays == nil {
			return s3err.ErrMalformedXML
		}
		if *t.NoncurrentDays < 0 || *t.NoncurrentDays > math.MaxInt32 {
			return s3err.ErrInvalidArgument.WithMessage("'NoncurrentDays' in NoncurrentVersionTransition action must be nonnegative")
		}
		if !validStorageClass(t.StorageClass) {
			return s3err.ErrMalformedXML
		}
		if seenNCClass[t.StorageClass] {
			return s3err.ErrInvalidRequest.WithMessage("'StorageClass' must be different for 'NoncurrentVersionTransition' actions in same 'Rule' with %s", desc)
		}
		seenNCClass[t.StorageClass] = true
		if (t.StorageClass == "STANDARD_IA" || t.StorageClass == "ONEZONE_IA") && *t.NoncurrentDays < 30 {
			return s3err.ErrInvalidArgument.WithMessage("'NoncurrentDays' in NoncurrentVersionTransition action must be greater than or equal to 30 for storageClass '%s'", t.StorageClass)
		}
		if err := validateNewer(t.NewerNoncurrentVersions, "NoncurrentVersionTransition"); err != nil {
			return err
		}
		maxNCTransitionDays = max(maxNCTransitionDays, *t.NoncurrentDays)
	}

	if n := r.NoncurrentVersionExpiration; n != nil {
		if n.NoncurrentDays == nil || *n.NoncurrentDays <= 0 || *n.NoncurrentDays > math.MaxInt32 {
			return s3err.ErrInvalidArgument.WithMessage("'NoncurrentDays' for NoncurrentVersionExpiration action must be a positive integer")
		}
		if err := validateNewer(n.NewerNoncurrentVersions, "NoncurrentVersionExpiration"); err != nil {
			return err
		}
		if maxNCTransitionDays >= 0 && *n.NoncurrentDays <= maxNCTransitionDays {
			return s3err.ErrInvalidArgument.WithMessage("'NoncurrentDays' in the NoncurrentVersionExpiration action for %s must be greater than 'NoncurrentDays' in the NoncurrentVersionTransition action", desc)
		}
	}

	if a := r.AbortIncompleteMultipartUpload; a != nil {
		if a.DaysAfterInitiation == nil || *a.DaysAfterInitiation <= 0 || *a.DaysAfterInitiation > math.MaxInt32 {
			return s3err.ErrInvalidArgument.WithMessage("'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a positive integer")
		}
		if hasTags {
			return s3err.ErrInvalidRequest.WithMessage("AbortIncompleteMultipartUpload cannot be specified with Tags.")
		}
	}
	return nil
}

func validateNewer(n *int, action string) error {
	if n != nil && (*n <= 0 || *n > MaxNewerNoncurrentVersions) {
		return s3err.ErrInvalidArgument.WithMessage("'NewerNoncurrentVersions' for %s action must be a positive integer up to %d", action, MaxNewerNoncurrentVersions)
	}
	return nil
}

func validStorageClass(sc string) bool {
	for _, c := range TransitionStorageClasses {
		if sc == c {
			return true
		}
	}
	return false
}

func (f *Filter) validate() error {
	n := 0
	if f.Prefix != nil {
		n++
	}
	if f.Tag != nil {
		n++
		if err := f.Tag.validate(); err != nil {
			return err
		}
	}
	if f.ObjectSizeGreaterThan != nil {
		n++
	}
	if f.ObjectSizeLessThan != nil {
		n++
	}
	if f.And != nil {
		n++
	}
	if n > 1 {
		return s3err.ErrMalformedXML
	}
	if err := validateSizes(f.ObjectSizeGreaterThan, f.ObjectSizeLessThan); err != nil {
		return err
	}
	if a := f.And; a != nil {
		if a.Prefix == nil && len(a.Tags) == 0 && a.ObjectSizeGreaterThan == nil && a.ObjectSizeLessThan == nil {
			return s3err.ErrMalformedXML
		}
		keys := make(map[string]bool, len(a.Tags))
		for i := range a.Tags {
			if err := a.Tags[i].validate(); err != nil {
				return err
			}
			if keys[a.Tags[i].Key] {
				return s3err.ErrInvalidRequest.WithMessage("Duplicate Tag Keys are not allowed.")
			}
			keys[a.Tags[i].Key] = true
		}
		if err := validateSizes(a.ObjectSizeGreaterThan, a.ObjectSizeLessThan); err != nil {
			return err
		}
	}
	return nil
}

func validateSizes(gt, lt *int64) error {
	if gt != nil && *gt < 0 {
		return s3err.ErrInvalidArgument.WithMessage("'ObjectSizeGreaterThan' must be a nonnegative integer")
	}
	if lt != nil && *lt <= 0 {
		return s3err.ErrInvalidArgument.WithMessage("'ObjectSizeLessThan' must be a positive integer")
	}
	if gt != nil && lt != nil && *gt >= *lt {
		return s3err.ErrInvalidArgument.WithMessage("'ObjectSizeGreaterThan' must be less than 'ObjectSizeLessThan'")
	}
	return nil
}

func (t *Tag) validate() error {
	if t.Key == "" || utf8.RuneCountInString(t.Key) > MaxTagKeyLength {
		return s3err.ErrInvalidTag.WithMessage("The TagKey you have provided is invalid")
	}
	if utf8.RuneCountInString(t.Value) > MaxTagValueLength {
		return s3err.ErrInvalidTag.WithMessage("The TagValue you have provided is invalid")
	}
	return nil
}

// hasTagFilter reports whether the rule's filter has any tag predicate.
func (r *Rule) hasTagFilter() bool {
	return r.Filter != nil && (r.Filter.Tag != nil || (r.Filter.And != nil && len(r.Filter.And.Tags) > 0))
}

// describe renders the rule's selector for error messages, like S3 does
// ("prefix 'logs/'" or "filter '(prefix=logs/)'").
func (r *Rule) describe() string {
	if r.Filter == nil {
		p := ""
		if r.Prefix != nil {
			p = *r.Prefix
		}
		return fmt.Sprintf("prefix '%s'", truncate(p, 64))
	}
	return fmt.Sprintf("filter '(prefix=%s)'", truncate(r.KeyPrefix(), 64))
}

// truncate shortens s to at most n runes for inclusion in error messages.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
