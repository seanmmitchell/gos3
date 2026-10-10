package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

const (
	signV4Algorithm = "AWS4-HMAC-SHA256"
	iso8601Format   = "20060102T150405Z"
	yyyymmdd        = "20060102"
	scopeTerminator = "aws4_request"
	serviceS3       = "s3"

	// MaxPresignExpires is the longest accepted X-Amz-Expires (7 days).
	MaxPresignExpires = 7 * 24 * time.Hour

	maxAuthorizationLen = 16 << 10
	maxSignedHeaders    = 256
)

// Presigned query parameter names.
const (
	amzAlgorithm     = "X-Amz-Algorithm"
	amzCredential    = "X-Amz-Credential"
	amzDate          = "X-Amz-Date"
	amzExpires       = "X-Amz-Expires"
	amzSignedHeaders = "X-Amz-SignedHeaders"
	amzSignature     = "X-Amz-Signature"
	amzSecurityToken = "X-Amz-Security-Token"
	amzContentSHA256 = "X-Amz-Content-Sha256"
)

// authKind selects the error family used for a given authentication method.
type authKind int

const (
	kindHeader authKind = iota
	kindQuery
	kindPost
)

// malformed returns the method-specific "malformed credentials" error.
func (k authKind) malformed(msg string) *s3err.Error {
	switch k {
	case kindHeader:
		return s3err.ErrAuthorizationHeaderMalformed.WithMessage("%s", "The authorization header is malformed; "+msg)
	case kindQuery:
		return s3err.ErrAuthorizationQueryParametersError.WithMessage("%s", "Error parsing the X-Amz-Credential parameter; "+msg)
	default:
		return s3err.ErrInvalidArgument.WithMessage("%s", "Invalid POST policy credential; "+msg).With("ArgumentName", "x-amz-credential")
	}
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// SigningKey derives the SigV4 signing key for a credential scope
// (date is YYYYMMDD).
func SigningKey(secretKey, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, scopeTerminator)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// credential is a parsed "AK/YYYYMMDD/region/service/aws4_request" value.
type credential struct {
	accessKey string
	date      string
	region    string
	service   string
}

func (c credential) scope() string {
	return c.date + "/" + c.region + "/" + c.service + "/" + scopeTerminator
}

// parseCredential parses and validates a credential string.
func (v *Verifier) parseCredential(s string, kind authKind) (credential, error) {
	parts := strings.Split(s, "/")
	n := len(parts)
	if n < 5 {
		return credential{}, kind.malformed(`the Credential is mal-formed; expecting "<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request".`)
	}
	c := credential{
		accessKey: strings.Join(parts[:n-4], "/"),
		date:      parts[n-4],
		region:    parts[n-3],
		service:   parts[n-2],
	}
	if c.accessKey == "" || c.region == "" {
		return credential{}, kind.malformed(`the Credential is mal-formed; expecting "<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request".`)
	}
	if parts[n-1] != scopeTerminator {
		return credential{}, kind.malformed(fmt.Sprintf("incorrect terminal %q. This endpoint uses %q.", truncate(parts[n-1], 64), scopeTerminator))
	}
	if c.service != serviceS3 {
		return credential{}, kind.malformed(fmt.Sprintf("incorrect service %q. This endpoint belongs to %q.", truncate(c.service, 64), serviceS3))
	}
	if _, err := time.Parse(yyyymmdd, c.date); err != nil || len(c.date) != 8 {
		return credential{}, kind.malformed(fmt.Sprintf("invalid credential date %q.", truncate(c.date, 64)))
	}
	if region := v.currentRegion(); v.opts.StrictRegion && region != "" && c.region != region {
		return credential{}, kind.malformed(fmt.Sprintf("the region '%s' is wrong; expecting '%s'", truncate(c.region, 64), region)).
			With("Region", region)
	}
	return c, nil
}

// checkCredentialDate verifies the scope date matches the request date.
func checkCredentialDate(c credential, t time.Time, kind authKind) error {
	if want := t.UTC().Format(yyyymmdd); c.date != want {
		return kind.malformed(fmt.Sprintf("Invalid credential date %q. This date is not the same as X-Amz-Date: %q.", c.date, want))
	}
	return nil
}

// parseSignedHeaders validates a SignedHeaders list and returns its names.
func parseSignedHeaders(s string, kind authKind) ([]string, map[string]bool, error) {
	bad := func(msg string) error {
		if kind == kindQuery {
			return s3err.ErrAuthorizationQueryParametersError.WithMessage("%s", msg)
		}
		return s3err.ErrAuthorizationHeaderMalformed.WithMessage("%s", "The authorization header is malformed; "+msg)
	}
	if s == "" {
		return nil, nil, bad("SignedHeaders must not be empty.")
	}
	names := strings.Split(s, ";")
	if len(names) > maxSignedHeaders {
		return nil, nil, bad("too many SignedHeaders.")
	}
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if !validHeaderName(name) {
			return nil, nil, bad(fmt.Sprintf("invalid header name %q in SignedHeaders.", truncate(name, 64)))
		}
		// A repeated name would add the header's full value to the
		// canonical request once per occurrence, letting one request of
		// MaxHeaderBytes cost hundreds of times that in memory and in the
		// SignatureDoesNotMatch body. Real signers never repeat a name.
		lname := strings.ToLower(name)
		if set[lname] {
			return nil, nil, bad(fmt.Sprintf("header %q is listed more than once in SignedHeaders.", truncate(lname, 64)))
		}
		set[lname] = true
	}
	if !set["host"] {
		return nil, nil, s3err.ErrAccessDenied.
			WithMessage("There were headers present in the request which were not signed").
			With("HeadersNotSigned", "host")
	}
	return names, set, nil
}

// checkUnsignedAmzHeaders rejects requests carrying x-amz-* headers that are
// not covered by the signature (as AWS does), so a captured signature cannot
// be replayed with e.g. an added x-amz-acl.
func checkUnsignedAmzHeaders(h http.Header, signed map[string]bool) error {
	var missing []string
	for k := range h {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-amz-") || signed[lk] {
			continue
		}
		switch lk {
		case "x-amz-content-sha256", "x-amz-date":
			// Both are covered implicitly: the payload hash and the request
			// date are part of the string to sign.
			continue
		}
		missing = append(missing, lk)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return s3err.ErrAccessDenied.
		WithMessage("There were headers present in the request which were not signed").
		With("HeadersNotSigned", strings.Join(missing, ", "))
}

// headerValues returns the values of a header, looking it up case
// insensitively (also for non-canonical keys that net/http kept verbatim).
func headerValues(h http.Header, name string) []string {
	if v := h.Values(name); len(v) > 0 {
		return v
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// requestHost returns the Host the client sent.
func requestHost(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	if r.URL != nil {
		return r.URL.Host
	}
	return ""
}

// canonicalHeaders builds the canonical headers block for the given signed
// header names (in the order given), using host for the host header.
func canonicalHeaders(r *http.Request, names []string, host string) string {
	var b strings.Builder
	for _, name := range names {
		lname := strings.ToLower(name)
		var vals []string
		switch lname {
		case "host":
			vals = []string{host}
		case "content-length":
			vals = headerValues(r.Header, "Content-Length")
			if len(vals) == 0 && r.ContentLength >= 0 {
				vals = []string{strconv.FormatInt(r.ContentLength, 10)}
			}
		case "transfer-encoding":
			vals = headerValues(r.Header, "Transfer-Encoding")
			if len(vals) == 0 && len(r.TransferEncoding) > 0 {
				vals = []string{strings.Join(r.TransferEncoding, ",")}
			}
		case "expect":
			vals = headerValues(r.Header, "Expect")
			if len(vals) == 0 && r.ProtoMajor >= 2 {
				// Go's HTTP/2 server removes "Expect: 100-continue" from
				// the request headers after taking over the continue
				// handling (HTTP/1.1 keeps it); that is the only value it
				// strips, so restore it for the signature.
				vals = []string{"100-continue"}
			}
		default:
			vals = headerValues(r.Header, name)
		}
		b.WriteString(name)
		b.WriteByte(':')
		for i, v := range vals {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(canonicalHeaderValue(v))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// buildCanonicalRequest assembles the SigV4 canonical request.
func buildCanonicalRequest(method, uri, query, headers, signedHeaders, payloadHash string) string {
	var b strings.Builder
	b.Grow(len(method) + len(uri) + len(query) + len(headers) + len(signedHeaders) + len(payloadHash) + 6)
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(uri)
	b.WriteByte('\n')
	b.WriteString(query)
	b.WriteByte('\n')
	b.WriteString(headers)
	b.WriteByte('\n')
	b.WriteString(signedHeaders)
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

func buildStringToSign(amzDate, scope, canonicalRequest string) string {
	return signV4Algorithm + "\n" + amzDate + "\n" + scope + "\n" + sha256Hex(canonicalRequest)
}

// sigInput is everything needed to compute a request signature.
type sigInput struct {
	r             *http.Request
	key           []byte
	amzDate       string
	scope         string
	signedNames   []string
	signedHeaders string // as sent
	query         string // canonical query string
	payloadHash   string
}

// pathCandidates returns the canonical URIs to try: the AWS canonical form
// (each segment decoded and re-encoded) and, if different, the path exactly
// as sent (clients that sign the path the way their URL library escaped it).
func pathCandidates(r *http.Request) []string {
	raw := requestEscapedPath(r)
	c := canonicalURIPath(raw)
	if raw != c && strings.HasPrefix(raw, "/") {
		return []string{c, raw}
	}
	return []string{c}
}

// hostCandidates returns the host values to try: as sent and, when it carries
// a default port (:80/:443), without it (SDKs strip default ports).
func hostCandidates(r *http.Request) []string {
	h := requestHost(r)
	for _, p := range []string{":80", ":443"} {
		if strings.HasSuffix(h, p) {
			return []string{h, strings.TrimSuffix(h, p)}
		}
	}
	return []string{h}
}

// matchSignature computes the signature for every candidate canonical form
// and reports whether provided matches one of them (constant time per
// comparison). On mismatch it returns the first candidate's string to sign
// and canonical request for the error response.
func matchSignature(in sigInput, provided string) (ok bool, sts, creq string) {
	uris := pathCandidates(in.r)
	for _, host := range hostCandidates(in.r) {
		headers := canonicalHeaders(in.r, in.signedNames, host)
		for _, uri := range uris {
			cr := buildCanonicalRequest(in.r.Method, uri, in.query,
				headers, in.signedHeaders, in.payloadHash)
			s := buildStringToSign(in.amzDate, in.scope, cr)
			sig := hex.EncodeToString(hmacSHA256(in.key, s))
			if hmac.Equal([]byte(sig), []byte(provided)) {
				return true, s, cr
			}
			if sts == "" {
				sts, creq = s, cr
			}
		}
	}
	return false, sts, creq
}

// maxEchoedCanonical bounds the StringToSign and CanonicalRequest echoed in
// a SignatureDoesNotMatch body: they help debug client signers, but must
// not let a request carrying large headers amplify into a huge response.
const maxEchoedCanonical = 16 << 10

func signatureMismatch(accessKey, provided, sts, creq string) error {
	return s3err.ErrSignatureDoesNotMatch.
		With("AWSAccessKeyId", accessKey).
		With("SignatureProvided", truncate(provided, 128)).
		With("StringToSign", truncate(sts, maxEchoedCanonical)).
		With("CanonicalRequest", truncate(creq, maxEchoedCanonical))
}

// lookup resolves an access key through the credential provider.
func (v *Verifier) lookup(accessKey string) (string, Identity, error) {
	if v.creds == nil {
		return "", Identity{}, s3err.ErrInvalidAccessKeyID.With("AWSAccessKeyId", accessKey)
	}
	secret, id, err := v.creds.LookupCredentials(accessKey)
	if err != nil {
		if errors.Is(err, s3err.ErrInvalidAccessKeyID) {
			return "", Identity{}, s3err.ErrInvalidAccessKeyID.With("AWSAccessKeyId", accessKey)
		}
		return "", Identity{}, s3err.As(err)
	}
	if secret == "" {
		return "", Identity{}, s3err.ErrInvalidAccessKeyID.With("AWSAccessKeyId", accessKey)
	}
	if id.AccessKey == "" {
		id.AccessKey = accessKey
	}
	id.IsAnonymous = false
	return secret, id, nil
}

var errNeedDate = s3err.ErrAccessDenied.WithMessage("AWS authentication requires a valid Date or x-amz-date header")

// headerRequestTime returns the request time from x-amz-date (or Date) and
// the ISO 8601 value used in the string to sign.
func headerRequestTime(r *http.Request) (time.Time, string, error) {
	if vals := r.Header.Values("X-Amz-Date"); len(vals) > 0 {
		if len(vals) != 1 {
			return time.Time{}, "", errNeedDate
		}
		t, err := time.Parse(iso8601Format, vals[0])
		if err != nil {
			return time.Time{}, "", errNeedDate
		}
		return t, vals[0], nil
	}
	if d := r.Header.Get("Date"); d != "" {
		t, err := http.ParseTime(d)
		if err != nil {
			return time.Time{}, "", errNeedDate
		}
		t = t.UTC()
		return t, t.Format(iso8601Format), nil
	}
	return time.Time{}, "", errNeedDate
}

// authHeader is a parsed SigV4 Authorization header.
type authHeader struct {
	credential    string
	signedHeaders string
	signature     string
}

// parseAuthorizationHeader parses
// "AWS4-HMAC-SHA256 Credential=..., SignedHeaders=..., Signature=...".
func parseAuthorizationHeader(h string) (authHeader, error) {
	h = strings.TrimSpace(h)
	if len(h) > maxAuthorizationLen {
		return authHeader{}, s3err.ErrAuthorizationHeaderMalformed.WithMessage("The authorization header is too long.")
	}
	if strings.HasPrefix(h, "AWS ") {
		return authHeader{}, errSigV2
	}
	algo, rest, _ := strings.Cut(h, " ")
	if algo != signV4Algorithm {
		if strings.HasPrefix(algo, "AWS4-ECDSA-P256-SHA256") {
			return authHeader{}, s3err.ErrNotImplemented.WithMessage("Signature Version 4A (AWS4-ECDSA-P256-SHA256) is not supported.")
		}
		return authHeader{}, s3err.ErrInvalidArgument.WithMessage("Unsupported Authorization Type").
			With("ArgumentName", "Authorization").
			With("ArgumentValue", truncate(h, 128))
	}
	var a authHeader
	var seen int
	for _, comp := range strings.Split(rest, ",") {
		comp = strings.TrimSpace(comp)
		if comp == "" {
			continue
		}
		k, val, ok := strings.Cut(comp, "=")
		k = strings.TrimSpace(k)
		val = strings.TrimSpace(val)
		if !ok {
			return authHeader{}, s3err.ErrAuthorizationHeaderMalformed.WithMessage("%s",
				fmt.Sprintf("The authorization header is malformed; the authorization component %q is malformed.", truncate(comp, 128)))
		}
		var dst *string
		var bit int
		switch k {
		case "Credential":
			dst, bit = &a.credential, 1
		case "SignedHeaders":
			dst, bit = &a.signedHeaders, 2
		case "Signature":
			dst, bit = &a.signature, 4
		default:
			return authHeader{}, s3err.ErrAuthorizationHeaderMalformed.WithMessage("%s",
				fmt.Sprintf("The authorization header is malformed; the authorization component %q is malformed.", truncate(comp, 128)))
		}
		if seen&bit != 0 {
			return authHeader{}, s3err.ErrAuthorizationHeaderMalformed.WithMessage("%s",
				fmt.Sprintf("The authorization header is malformed; the authorization component %q is duplicated.", k))
		}
		seen |= bit
		*dst = val
	}
	if seen != 7 || a.credential == "" || a.signedHeaders == "" || a.signature == "" {
		return authHeader{}, s3err.ErrAuthorizationHeaderMalformed.WithMessage(
			"The authorization header is malformed; the authorization header requires three components: Credential, SignedHeaders, and Signature.")
	}
	return a, nil
}

var errSigV2 = s3err.ErrAccessDenied.WithMessage("Signature Version 2 is not supported by this server; please use AWS Signature Version 4 (AWS4-HMAC-SHA256).")

// authenticateHeader verifies Authorization-header SigV4.
func (v *Verifier) authenticateHeader(r *http.Request, authz string, params []queryParam) (*Result, error) {
	ah, err := parseAuthorizationHeader(authz)
	if err != nil {
		return nil, err
	}
	cred, err := v.parseCredential(ah.credential, kindHeader)
	if err != nil {
		return nil, err
	}
	names, signedSet, err := parseSignedHeaders(ah.signedHeaders, kindHeader)
	if err != nil {
		return nil, err
	}
	t, amzDateStr, err := headerRequestTime(r)
	if err != nil {
		return nil, err
	}
	if err := checkCredentialDate(cred, t, kindHeader); err != nil {
		return nil, err
	}
	now := v.opts.Now().UTC()
	if d := now.Sub(t); d > v.opts.MaxClockSkew || -d > v.opts.MaxClockSkew {
		return nil, s3err.ErrRequestTimeTooSkewed.
			With("RequestTime", amzDateStr).
			With("ServerTime", now.Format(time.RFC3339)).
			With("MaxAllowedSkewMilliseconds", strconv.FormatInt(v.opts.MaxClockSkew.Milliseconds(), 10))
	}
	if len(r.Header.Values("X-Amz-Security-Token")) > 0 {
		return nil, s3err.ErrInvalidToken
	}
	modes := r.Header.Values("X-Amz-Content-Sha256")
	if len(modes) == 0 {
		return nil, s3err.ErrInvalidRequest.WithMessage("Missing required header for this request: x-amz-content-sha256")
	}
	if len(modes) > 1 {
		return nil, s3err.ErrInvalidArgument.WithMessage("Multiple x-amz-content-sha256 headers are not allowed.").
			With("ArgumentName", "x-amz-content-sha256")
	}
	mode := modes[0]
	kind, err := validatePayloadMode(mode)
	if err != nil {
		return nil, err
	}
	secret, id, err := v.lookup(cred.accessKey)
	if err != nil {
		return nil, err
	}
	if err := checkUnsignedAmzHeaders(r.Header, signedSet); err != nil {
		return nil, err
	}
	key := SigningKey(secret, cred.date, cred.region, cred.service)
	in := sigInput{
		r: r, key: key, amzDate: amzDateStr, scope: cred.scope(),
		signedNames: names, signedHeaders: ah.signedHeaders,
		query: canonicalQueryString(params, ""), payloadHash: mode,
	}
	ok, sts, creq := matchSignature(in, ah.signature)
	if !ok {
		return nil, signatureMismatch(cred.accessKey, ah.signature, sts, creq)
	}
	res := &Result{
		Identity:    id,
		Method:      MethodHeaderV4,
		Region:      cred.region,
		Service:     cred.service,
		PayloadMode: mode,
	}
	signer := &chunkSigner{key: key, amzDate: amzDateStr, scope: cred.scope(), prevSig: ah.signature}
	if err := setBody(res, r, kind, mode, signer); err != nil {
		return nil, err
	}
	return res, nil
}

// presignParams extracts the single-valued presigned parameters.
func presignParams(params []queryParam) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range params {
		switch p.key {
		case amzAlgorithm, amzCredential, amzDate, amzExpires, amzSignedHeaders, amzSignature, amzSecurityToken, amzContentSHA256:
			if _, dup := out[p.key]; dup {
				return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage("%s",
					fmt.Sprintf("The %s query parameter must not be specified more than once.", p.key))
			}
			out[p.key] = p.value
		}
	}
	return out, nil
}

// authenticatePresigned verifies a presigned (query string) SigV4 request.
func (v *Verifier) authenticatePresigned(r *http.Request, params []queryParam) (*Result, error) {
	q, err := presignParams(params)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{amzAlgorithm, amzCredential, amzSignature, amzDate, amzSignedHeaders, amzExpires} {
		if q[k] == "" {
			return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage(
				"Query-string authentication version 4 requires the X-Amz-Algorithm, X-Amz-Credential, X-Amz-Signature, X-Amz-Date, X-Amz-SignedHeaders, and X-Amz-Expires parameters.")
		}
	}
	if q[amzAlgorithm] != signV4Algorithm {
		return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage(`X-Amz-Algorithm only supports "AWS4-HMAC-SHA256"`)
	}
	t, err := time.Parse(iso8601Format, q[amzDate])
	if err != nil {
		return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage(`X-Amz-Date must be in the ISO8601 Long Format "yyyyMMdd'T'HHmmss'Z'"`)
	}
	expStr := q[amzExpires]
	expSecs, err := strconv.ParseInt(expStr, 10, 64)
	switch {
	case err != nil || expStr[0] == '+':
		return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage("X-Amz-Expires should be a number")
	case expSecs < 0:
		return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage("X-Amz-Expires must be non-negative")
	case expSecs == 0:
		return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage("X-Amz-Expires must be at least 1 second")
	case expSecs > int64(MaxPresignExpires/time.Second):
		return nil, s3err.ErrAuthorizationQueryParametersError.WithMessage("X-Amz-Expires must be less than a week (in seconds) that is 604800")
	}
	cred, err := v.parseCredential(q[amzCredential], kindQuery)
	if err != nil {
		return nil, err
	}
	if err := checkCredentialDate(cred, t, kindQuery); err != nil {
		return nil, err
	}
	names, signedSet, err := parseSignedHeaders(q[amzSignedHeaders], kindQuery)
	if err != nil {
		return nil, err
	}
	if _, ok := q[amzSecurityToken]; ok || len(r.Header.Values("X-Amz-Security-Token")) > 0 {
		return nil, s3err.ErrInvalidToken
	}
	now := v.opts.Now().UTC()
	if t.Sub(now) > v.opts.MaxClockSkew {
		return nil, s3err.ErrAccessDenied.WithMessage("Request is not valid yet").
			With("X-Amz-Date", q[amzDate]).
			With("ServerTime", now.Format(time.RFC3339))
	}
	expiresAt := t.Add(time.Duration(expSecs) * time.Second)
	if now.After(expiresAt) {
		return nil, s3err.ErrAccessDenied.WithMessage("Request has expired").
			With("X-Amz-Expires", expStr).
			With("Expires", expiresAt.Format(time.RFC3339)).
			With("ServerTime", now.Format(time.RFC3339))
	}
	payloadHash := PayloadUnsigned
	if h, ok := q[amzContentSHA256]; ok {
		if _, err := validatePayloadMode(h); err != nil {
			return nil, err
		}
		payloadHash = h
	}
	secret, id, err := v.lookup(cred.accessKey)
	if err != nil {
		return nil, err
	}
	if err := checkUnsignedAmzHeaders(r.Header, signedSet); err != nil {
		return nil, err
	}
	key := SigningKey(secret, cred.date, cred.region, cred.service)
	in := sigInput{
		r: r, key: key, amzDate: q[amzDate], scope: cred.scope(),
		signedNames: names, signedHeaders: q[amzSignedHeaders],
		query: canonicalQueryString(params, amzSignature), payloadHash: payloadHash,
	}
	ok, sts, creq := matchSignature(in, q[amzSignature])
	if !ok {
		return nil, signatureMismatch(cred.accessKey, q[amzSignature], sts, creq)
	}

	// The body mode: the signed query hash, else a header declaring a hash or
	// unsigned-trailer streaming (signed streaming needs it in the signature).
	mode := payloadHash
	if mode == PayloadUnsigned {
		if hv := r.Header.Get("X-Amz-Content-Sha256"); hv != "" {
			mode = hv
		}
	}
	kind, err := validatePayloadMode(mode)
	if err != nil {
		return nil, err
	}
	if (kind == payloadStreamingSigned || kind == payloadStreamingSignedTrailer) && mode != payloadHash {
		return nil, s3err.ErrInvalidRequest.WithMessage("Signed streaming payloads require the payload mode to be part of the presigned signature (X-Amz-Content-Sha256).")
	}
	res := &Result{
		Identity: id,
		Method:   MethodPresignedV4,
		Region:   cred.region,
		Service:  cred.service,
	}
	if mode != PayloadUnsigned {
		res.PayloadMode = mode
	}
	signer := &chunkSigner{key: key, amzDate: q[amzDate], scope: cred.scope(), prevSig: q[amzSignature]}
	if err := setBody(res, r, kind, mode, signer); err != nil {
		return nil, err
	}
	return res, nil
}

// authenticateAnonymous builds the Result for unauthenticated requests,
// still honouring a declared payload hash or unsigned aws-chunked encoding.
func (v *Verifier) authenticateAnonymous(r *http.Request) (*Result, error) {
	res := &Result{Identity: Anonymous, Method: MethodAnonymous}
	mode := r.Header.Get("X-Amz-Content-Sha256")
	if mode == "" && hasAWSChunkedEncoding(r.Header) && r.Header.Get("X-Amz-Trailer") != "" {
		mode = PayloadStreamingUnsignedTrailer
	}
	if mode == "" {
		mode = PayloadUnsigned
	}
	kind, err := validatePayloadMode(mode)
	if err != nil {
		return nil, err
	}
	if kind == payloadStreamingSigned || kind == payloadStreamingSignedTrailer {
		return nil, s3err.ErrAccessDenied.WithMessage("%s", "Signed streaming payloads ("+mode+") require AWS Signature Version 4 authentication.")
	}
	if mode != PayloadUnsigned {
		res.PayloadMode = mode
	}
	if err := setBody(res, r, kind, mode, nil); err != nil {
		return nil, err
	}
	return res, nil
}

// hasAWSChunkedEncoding reports whether Content-Encoding lists aws-chunked.
func hasAWSChunkedEncoding(h http.Header) bool {
	for _, v := range h.Values("Content-Encoding") {
		for _, e := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(e), "aws-chunked") {
				return true
			}
		}
	}
	return false
}

// setBody installs Body, DecodedContentLength and Trailer on res according
// to the payload mode.
func setBody(res *Result, r *http.Request, kind payloadKind, mode string, signer *chunkSigner) error {
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	res.Trailer = emptyTrailer
	res.DecodedContentLength = r.ContentLength
	switch kind {
	case payloadHex:
		res.Body = newSHA256Reader(body, mode)
	case payloadUnsigned:
		res.Body = passthroughBody{r: body}
	case payloadStreamingSigned, payloadStreamingSignedTrailer, payloadStreamingUnsignedTrailer:
		declared, err := parseDecodedContentLength(r.Header)
		if err != nil {
			return err
		}
		if declared < 0 && kind != payloadStreamingUnsignedTrailer {
			return s3err.ErrMissingContentLength.WithMessage("You must provide the x-amz-decoded-content-length HTTP header.")
		}
		withTrailer := kind != payloadStreamingSigned
		var expected map[string]bool
		if withTrailer {
			if expected, err = parseTrailerDeclaration(r.Header); err != nil {
				return err
			}
		}
		if kind == payloadStreamingUnsignedTrailer {
			signer = nil
		} else if signer == nil {
			return s3err.ErrAccessDenied.WithMessage("Signed streaming payloads require AWS Signature Version 4 authentication.")
		}
		th := &trailerHolder{}
		res.Body = newChunkedReader(body, signer, withTrailer, declared, expected, th)
		res.DecodedContentLength = declared
		res.Trailer = th.get
	default:
		return s3err.ErrInvalidArgument.WithMessage("Unsupported x-amz-content-sha256 value.")
	}
	return nil
}

// authenticate dispatches to the right authentication method.
func (v *Verifier) authenticate(r *http.Request) (*Result, error) {
	params, ok := parseRawQuery(requestRawQuery(r))
	if !ok {
		return nil, s3err.ErrInvalidArgument.WithMessage("Too many query parameters.")
	}
	var hasV4Query, hasV2Query, hasV2Sig, hasV2Expires bool
	for _, p := range params {
		switch p.key {
		case amzAlgorithm, amzCredential, amzSignature:
			hasV4Query = true
		case "AWSAccessKeyId":
			hasV2Query = true
		case "Signature":
			hasV2Sig = true
		case "Expires":
			hasV2Expires = true
		}
	}
	hasV2Query = hasV2Query || (hasV2Sig && hasV2Expires)
	authz := r.Header.Values("Authorization")
	if (len(authz) > 0 && (hasV4Query || hasV2Query)) || (hasV4Query && hasV2Query) {
		return nil, s3err.ErrInvalidArgument.
			WithMessage("Only one auth mechanism allowed; only the X-Amz-Algorithm query parameter, Signature query string parameter or the Authorization header should be specified").
			With("ArgumentName", "Authorization")
	}
	switch {
	case len(authz) > 1:
		return nil, s3err.ErrAuthorizationHeaderMalformed.WithMessage("The authorization header is malformed; multiple Authorization headers are not allowed.")
	case len(authz) == 1:
		return v.authenticateHeader(r, authz[0], params)
	case hasV4Query:
		return v.authenticatePresigned(r, params)
	case hasV2Query:
		return nil, errSigV2
	}
	return v.authenticateAnonymous(r)
}
