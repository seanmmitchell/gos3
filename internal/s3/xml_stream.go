package s3

// Streaming decoding of the request documents that carry a list of
// entries (DeleteObjects, CompleteMultipartUpload).
//
// Unmarshalling such a document with encoding/xml into a slice allocates
// one entry per element before the handler can check how many there are:
// a few MiB of "<Object/>" became about a million structs (hundreds of MiB)
// before DeleteObjects rejected the request, and DeleteObjects does this
// before it authorizes anyone. xmlStream instead walks the tokens itself,
// lets the caller stop at the first entry over the limit, and bounds the
// work any document can cause: its size in bytes, the number of elements
// and other tokens, the nesting depth and the size of a single tag (a
// start tag with many attributes is decoded in one piece).

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

const (
	// maxXMLDepth bounds element nesting in streamed documents; the
	// documents themselves nest three levels deep (Delete > Object > Key).
	maxXMLDepth = 16
	// maxXMLSegment bounds the bytes between two '<' (a tag and the text
	// that follows it). The longest legitimate one is a 1024-byte key with
	// every byte escaped as a 6-byte reference ("&quot;", "&#x0D;"):
	// 6144 bytes plus the "Key>" before it.
	maxXMLSegment = 16 << 10
)

// errXMLValue reports an element value encoding/xml would not have
// accepted (MalformedXML).
var errXMLValue = errors.New("s3: invalid value in XML document")

// xmlSource is the byte source of a streamed document. It stops with
// MaxMessageLengthExceeded after max bytes, having read at most one byte
// more, and with MalformedXML when a segment (see maxXMLSegment) gets too
// long. The first error, including those of the underlying reader (digest
// or signature mismatch, timeout, ...), is kept in err and returned by
// every later Read.
type xmlSource struct {
	r   io.Reader
	n   int64 // bytes read
	max int64
	seg int // bytes since the last '<'
	err error
}

func (s *xmlSource) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if rem := s.max + 1 - s.n; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := s.r.Read(p)
	s.n += int64(n)
	if s.n > s.max {
		s.err = s3err.ErrMaxMessageLengthExceeded
		return 0, s.err
	}
	for _, c := range p[:n] {
		if c == '<' {
			s.seg = 0
		} else if s.seg++; s.seg > maxXMLSegment {
			s.err = s3err.ErrMalformedXML
			return 0, s.err
		}
	}
	if err != nil && err != io.EOF {
		s.err = mapStorageError(err)
		return n, s.err
	}
	return n, err
}

// xmlStream reads a request document token by token (see the file
// comment). Its methods return errors already converted for the response
// (see fail).
type xmlStream struct {
	src         *xmlSource
	d           *xml.Decoder
	depth       int // open elements
	elements    int
	tokens      int
	maxElements int
}

// newXMLStream reads a document of at most maxBytes bytes and maxElements
// elements from r. Other tokens (text, comments, ...) are limited to four
// per element: an element with text and white space before it is four
// tokens (white space, start, text, end).
func newXMLStream(r io.Reader, maxBytes int64, maxElements int) *xmlStream {
	src := &xmlSource{r: r, max: maxBytes}
	return &xmlStream{src: src, d: xml.NewDecoder(src), maxElements: maxElements}
}

// token returns the next token, enforcing the element, token and depth
// limits (MalformedXML at once); other errors are returned unconverted.
func (x *xmlStream) token() (xml.Token, error) {
	if x.tokens++; x.tokens > 4*x.maxElements+64 {
		return nil, s3err.ErrMalformedXML
	}
	t, err := x.d.Token()
	if err != nil {
		return nil, err
	}
	switch t.(type) {
	case xml.StartElement:
		x.elements++
		if x.depth++; x.depth > maxXMLDepth || x.elements > x.maxElements {
			return nil, s3err.ErrMalformedXML
		}
	case xml.EndElement:
		x.depth--
	}
	return t, nil
}

// fail converts a decoding error. A failure of the body itself (too large,
// Content-MD5 / checksum / signature mismatch, timeout, ...) wins, as when
// the whole body was read before it was parsed; S3 errors (the limits,
// too many entries) are returned at once, without reading further. Any
// other error means a malformed document: the rest of the body is read
// (within the size limit) so that a body failure still takes precedence,
// then MalformedXML is returned.
func (x *xmlStream) fail(err error) error {
	if x.src.err != nil {
		return x.src.err
	}
	var se *s3err.Error
	if errors.As(err, &se) {
		return se
	}
	_, _ = io.Copy(io.Discard, x.src)
	if x.src.err != nil {
		return x.src.err
	}
	return s3err.ErrMalformedXML
}

// root reads up to the start of the root element, which must be called
// name (in any namespace, like an encoding/xml XMLName without namespace).
// A body of only white space is MissingRequestBodyError.
func (x *xmlStream) root(name string) error {
	blank := true
	for {
		t, err := x.token()
		if err == io.EOF && blank && x.src.err == nil {
			return s3err.ErrMissingRequestBody
		}
		if err != nil {
			return x.fail(err)
		}
		switch t := t.(type) {
		case xml.StartElement:
			if t.Name.Local != name {
				return x.fail(errXMLValue)
			}
			return nil
		case xml.CharData:
			blank = blank && len(bytes.TrimSpace(t)) == 0
		default:
			blank = false
		}
	}
}

// next returns the next child element of the root, which the caller must
// consume (text, fields or skip); ok is false at the end of the root.
func (x *xmlStream) next() (el xml.StartElement, ok bool, err error) {
	for {
		t, err := x.token()
		if err != nil {
			return el, false, x.fail(err)
		}
		switch t := t.(type) {
		case xml.StartElement:
			return t, true, nil
		case xml.EndElement:
			return el, false, nil
		}
	}
}

// text consumes the element just started and returns its character data,
// without that of nested elements (as encoding/xml fills a string field).
func (x *xmlStream) text() (string, error) {
	var b []byte
	level := x.depth
	for {
		t, err := x.token()
		if err != nil {
			return "", x.fail(err)
		}
		switch t := t.(type) {
		case xml.CharData:
			if x.depth == level {
				b = append(b, t...)
			}
		case xml.EndElement:
			if x.depth < level {
				return string(b), nil
			}
		}
	}
}

// skip consumes the element just started.
func (x *xmlStream) skip() error {
	level := x.depth
	for x.depth >= level {
		if _, err := x.token(); err != nil {
			return x.fail(err)
		}
	}
	return nil
}

// fields consumes the element just started (an entry such as <Object>) and
// calls set with the name and text of each child element.
func (x *xmlStream) fields(set func(name, value string)) error {
	level := x.depth
	for {
		t, err := x.token()
		if err != nil {
			return x.fail(err)
		}
		switch t := t.(type) {
		case xml.StartElement:
			v, err := x.text()
			if err != nil {
				return err
			}
			set(t.Name.Local, v)
		case xml.EndElement:
			if x.depth < level {
				return nil
			}
		}
	}
}

// finish reads the rest of the body after the root element (ignored, as
// encoding/xml does) so that the body's checks run; it returns their error.
func (x *xmlStream) finish() error {
	_, _ = io.Copy(io.Discard, x.src)
	return x.src.err
}

// parseXMLBool parses a boolean element value like encoding/xml: empty is
// false, otherwise strconv.ParseBool of the trimmed value.
func parseXMLBool(v string) (bool, error) {
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, errXMLValue
	}
	return b, nil
}
