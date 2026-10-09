package auth

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultRegion is used by the client helpers when region is empty.
const DefaultRegion = "us-east-1"

// pinWirePath makes Go send the path exactly as canonicalised (AWS
// encoding) so the server sees the same canonical URI that was signed.
func pinWirePath(u *url.URL, canonical string) {
	if u.Opaque != "" {
		return
	}
	if p, err := url.PathUnescape(canonical); err == nil && p == u.Path {
		u.RawPath = canonical
	}
}

// signRequest implements SignRequest.
func signRequest(r *http.Request, c Credentials, region, service, payloadHash string, t time.Time) error {
	if r == nil || r.URL == nil {
		return errors.New("auth: SignRequest: nil request or URL")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("auth: SignRequest: empty credentials")
	}
	if region == "" {
		region = DefaultRegion
	}
	if service == "" {
		service = serviceS3
	}
	if payloadHash == "" {
		if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
			payloadHash = PayloadEmptySHA256
		} else {
			payloadHash = PayloadUnsigned
		}
	}
	host := requestHost(r)
	if host == "" {
		return errors.New("auth: SignRequest: request has no host")
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	if r.Header == nil {
		r.Header = http.Header{}
	}
	t = t.UTC()
	amzDateStr := t.Format(iso8601Format)
	date := t.Format(yyyymmdd)
	r.Header.Del("Authorization")
	r.Header.Set("X-Amz-Date", amzDateStr)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)

	uri := canonicalURIPath(escapedURLPath(r.URL))
	pinWirePath(r.URL, uri)
	params, ok := parseRawQuery(r.URL.RawQuery)
	if !ok {
		return errors.New("auth: SignRequest: too many query parameters")
	}

	names := []string{"host"}
	for k := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5" {
			names = append(names, lk)
		}
	}
	sort.Strings(names)
	names = dedupSorted(names)
	signedHeaders := strings.Join(names, ";")

	creq := buildCanonicalRequest(method, uri, canonicalQueryString(params, ""),
		canonicalHeaders(r, names, host), signedHeaders, payloadHash)
	scope := date + "/" + region + "/" + service + "/" + scopeTerminator
	sts := buildStringToSign(amzDateStr, scope, creq)
	sig := hex.EncodeToString(hmacSHA256(SigningKey(c.SecretKey, date, region, service), sts))
	r.Header.Set("Authorization", signV4Algorithm+" Credential="+c.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
	return nil
}

func dedupSorted(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// presignURL implements PresignURL.
func presignURL(method, rawURL string, c Credentials, region string, expires time.Duration, t time.Time) (string, error) {
	if c.AccessKey == "" || c.SecretKey == "" {
		return "", errors.New("auth: PresignURL: empty credentials")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("auth: PresignURL: %w", err)
	}
	if u.Host == "" {
		return "", errors.New("auth: PresignURL: URL has no host")
	}
	secs := int64(expires / time.Second)
	if secs < 1 || expires > MaxPresignExpires {
		return "", fmt.Errorf("auth: PresignURL: expires must be between 1s and %s", MaxPresignExpires)
	}
	if region == "" {
		region = DefaultRegion
	}
	if method == "" {
		method = http.MethodGet
	}
	t = t.UTC()
	date := t.Format(yyyymmdd)
	scope := date + "/" + region + "/" + serviceS3 + "/" + scopeTerminator

	existing, ok := parseRawQuery(u.RawQuery)
	if !ok {
		return "", errors.New("auth: PresignURL: too many query parameters")
	}
	params := make([]queryParam, 0, len(existing)+5)
	for _, p := range existing {
		switch p.key {
		case amzAlgorithm, amzCredential, amzDate, amzExpires, amzSignedHeaders, amzSignature:
			continue // re-presigning: drop stale auth parameters
		}
		params = append(params, p)
	}
	params = append(params,
		queryParam{amzAlgorithm, signV4Algorithm},
		queryParam{amzCredential, c.AccessKey + "/" + scope},
		queryParam{amzDate, t.Format(iso8601Format)},
		queryParam{amzExpires, strconv.FormatInt(secs, 10)},
		queryParam{amzSignedHeaders, "host"},
	)
	if u.Path == "" && u.Opaque == "" {
		u.Path = "/"
	}
	uri := canonicalURIPath(escapedURLPath(u))
	query := canonicalQueryString(params, "")
	creq := buildCanonicalRequest(method, uri, query,
		"host:"+canonicalHeaderValue(u.Host)+"\n", "host", PayloadUnsigned)
	sts := buildStringToSign(t.Format(iso8601Format), scope, creq)
	sig := hex.EncodeToString(hmacSHA256(SigningKey(c.SecretKey, date, region, serviceS3), sts))
	pinWirePath(u, uri)
	u.RawQuery = query + "&" + amzSignature + "=" + sig
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}
