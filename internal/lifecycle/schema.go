package lifecycle

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// schema lists, for every container element of a LifecycleConfiguration,
// the child elements it may contain and whether each may repeat. Element
// names are context-free in this schema (e.g. Tag has the same children
// under Filter and And), so a flat map suffices. Elements that are not keys
// of schema are leaves holding text only.
var schema = map[string]map[string]bool{
	"LifecycleConfiguration": {"Rule": true},
	"Rule": {
		"ID": false, "Filter": false, "Prefix": false, "Status": false,
		"Expiration": false, "Transition": true, "NoncurrentVersionTransition": true,
		"NoncurrentVersionExpiration": false, "AbortIncompleteMultipartUpload": false,
	},
	"Filter": {
		"Prefix": false, "Tag": false, "ObjectSizeGreaterThan": false,
		"ObjectSizeLessThan": false, "And": false,
	},
	"And": {
		"Prefix": false, "Tag": true, "ObjectSizeGreaterThan": false, "ObjectSizeLessThan": false,
	},
	"Tag":                            {"Key": false, "Value": false},
	"Expiration":                     {"Days": false, "Date": false, "ExpiredObjectDeleteMarker": false},
	"Transition":                     {"Days": false, "Date": false, "StorageClass": false},
	"NoncurrentVersionTransition":    {"NoncurrentDays": false, "StorageClass": false, "NewerNoncurrentVersions": false},
	"NoncurrentVersionExpiration":    {"NoncurrentDays": false, "NewerNoncurrentVersions": false},
	"AbortIncompleteMultipartUpload": {"DaysAfterInitiation": false},
}

// boolLeaves are leaf elements whose text must be exactly "true" or "false"
// (encoding/xml would also accept "1", "t", ...).
var boolLeaves = map[string]bool{"ExpiredObjectDeleteMarker": true}

// maxLeafText bounds the text content collected for validated leaves.
const maxLeafText = 64

// checkSchema validates the element structure of a LifecycleConfiguration
// document: the root element, that every element is known in its parent,
// that non-repeatable elements occur at most once, that leaves contain no
// elements and that containers contain no text. encoding/xml silently
// ignores unknown and duplicated elements, while S3 rejects such documents
// with MalformedXML. It also rejects documents with more than MaxRules rules
// early, before they are decoded.
func checkSchema(data []byte) error {
	type frame struct {
		name string
		seen map[string]bool
		text strings.Builder // collected text of validated leaves
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		stack []*frame
		root  bool
		rules int
	)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s3err.ErrMalformedXML
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if len(stack) == 0 {
				if root || name != "LifecycleConfiguration" {
					return s3err.ErrMalformedXML
				}
				root = true
				stack = append(stack, &frame{name: name, seen: map[string]bool{}})
				continue
			}
			parent := stack[len(stack)-1]
			children, container := schema[parent.name]
			if !container {
				return s3err.ErrMalformedXML // element inside a leaf
			}
			repeat, ok := children[name]
			if !ok {
				return s3err.ErrMalformedXML
			}
			if parent.seen[name] && !repeat {
				return s3err.ErrMalformedXML
			}
			parent.seen[name] = true
			if name == "Rule" {
				rules++
				if rules > MaxRules {
					return s3err.ErrInvalidRequest.WithMessage("The number of lifecycle rules should not exceed allowed limit of %d rules", MaxRules)
				}
			}
			f := &frame{name: name}
			if _, isContainer := schema[name]; isContainer {
				f.seen = map[string]bool{}
			}
			stack = append(stack, f)
		case xml.EndElement:
			if len(stack) == 0 {
				return s3err.ErrMalformedXML
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if boolLeaves[f.name] {
				if v := strings.TrimSpace(f.text.String()); v != "true" && v != "false" {
					return s3err.ErrMalformedXML
				}
			}
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(t)) != 0 {
					return s3err.ErrMalformedXML
				}
				continue
			}
			f := stack[len(stack)-1]
			if _, isContainer := schema[f.name]; isContainer {
				if len(bytes.TrimSpace(t)) != 0 {
					return s3err.ErrMalformedXML
				}
			} else if boolLeaves[f.name] {
				if f.text.Len()+len(t) > maxLeafText {
					return s3err.ErrMalformedXML
				}
				f.text.Write(t)
			}
		case xml.Directive:
			// DTDs are not part of the S3 schema.
			return s3err.ErrMalformedXML
		}
	}
	if !root || len(stack) != 0 {
		return s3err.ErrMalformedXML
	}
	return nil
}
