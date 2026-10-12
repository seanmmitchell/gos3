package auth

import (
	"net/http"
	"net/url"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// QueryValues returns the query parameters of r decoded exactly as the
// SigV4 verifier decodes them for the canonical request: in the order
// sent, '+' as a space, invalid %-escapes and ';' kept literally. Routing
// and handlers must use these values rather than r.URL.Query(), which
// silently drops every pair containing ';' or an invalid escape: a
// parameter covered by a presigned URL's signature could otherwise be
// rewritten (%3B to ';') into one the server ignores without breaking
// the signature. More than 1000 parameters is InvalidArgument (as in
// Authenticate).
func QueryValues(r *http.Request) (url.Values, error) {
	params, ok := parseRawQuery(requestRawQuery(r))
	if !ok {
		return nil, s3err.ErrInvalidArgument.WithMessage("Too many query parameters.")
	}
	q := make(url.Values, len(params))
	for _, p := range params {
		q[p.key] = append(q[p.key], p.value)
	}
	return q, nil
}
