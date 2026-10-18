package admin

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/discovery"
)

// Relay metadata is in the query so SigV4 always binds the complete destination
// request, including headers excluded by browser signers (e.g. Content-Type).
// POST carries the raw, independently signed request body. GET is also usable
// with a destination-bound presigned relay URL for media and downloads.
type relayRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
}

const maxRelayBody int64 = 5 << 30
const relayResultHeader = "X-Gos3-Proxy"

func (a *api) serveProxy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(relayResultHeader, "error")
	// Never let upstream data execute as a document under the console origin.
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Access-Control-Expose-Headers", "X-Gos3-Proxy, ETag, Content-Range, Accept-Ranges, Content-Disposition, x-amz-version-id, x-amz-request-id, x-amz-id-2, x-amz-checksum-crc32, x-amz-checksum-crc32c, x-amz-checksum-crc64nvme, x-amz-checksum-sha1, x-amz-checksum-sha256")
	fail := func(e *apiError) { writeError(w, e) }
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
		fail(errorf(405, codeMethodNotAllowed, "relay requires GET, HEAD or POST"))
		return
	}
	result, err := a.d.Verifier.Authenticate(r)
	if err != nil {
		fail(errorf(401, codeUnauthenticated, "relay authentication failed"))
		return
	}
	if result.Body != nil {
		defer result.Body.Close()
	}
	if result.Identity.IsAnonymous {
		fail(errorf(401, codeUnauthenticated, "sign the relay request"))
		return
	}
	if e := checkSignedPayload(r, result); e != nil {
		fail(e)
		return
	}
	rc := &reqCtx{r: r, ctx: r.Context(), auth: result, id: result.Identity, action: "admin:ProxyRequest", start: a.now(), preAuthorized: true}
	if err := a.requireAction(rc, rc.action, "", nil); err != nil {
		fail(a.toAPIError(err))
		return
	}
	// Limit concurrent disk spools and outbound requests independently of discovery.
	select {
	case a.relays <- struct{}{}:
		defer func() { <-a.relays }()
	default:
		fail(errorf(429, codeTooManyRequests, "too many relay requests"))
		return
	}
	encoded := r.URL.Query().Get("request")
	if len(encoded) == 0 || len(encoded) > 48<<10 {
		fail(badRequest("invalid relay metadata size"))
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		fail(badRequest("invalid relay metadata"))
		return
	}
	var meta relayRequest
	if err := decodeStrict(data, &meta); err != nil {
		fail(badRequest("invalid relay metadata: %v", err))
		return
	}
	target, err := url.Parse(meta.URL)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.Fragment != "" || target.Opaque != "" {
		fail(badRequest("destination must be an absolute HTTP(S) URL without userinfo or fragment"))
		return
	}
	// A relay cannot be used to build another relay hop, even via a base path.
	if strings.Contains(path.Clean(target.Path), Prefix+"/proxy") {
		fail(badRequest("nested relays are not allowed"))
		return
	}
	switch meta.Method {
	case "GET", "HEAD", "PUT", "POST", "PATCH", "DELETE":
	default:
		fail(badRequest("unsupported destination method"))
		return
	}
	if (meta.Method == "HEAD") != (r.Method == "HEAD") {
		fail(badRequest("destination HEAD requires an outer HEAD request"))
		return
	}
	if r.Method == "GET" && meta.Method != "GET" {
		fail(badRequest("GET relay URLs can only read destinations"))
		return
	}
	var body io.Reader
	var size int64
	if r.Method == "POST" {
		if r.ContentLength > maxRelayBody {
			fail(badRequest("relay body exceeds 5 GiB"))
			return
		}
		// Fully verify before a write can reach its destination. Disk spooling
		// bounds memory and prevents a forged payload from causing side effects.
		f, err := os.CreateTemp("", "gos3-relay-*")
		if err != nil {
			fail(errorf(503, codeInternalError, "cannot spool relay body"))
			return
		}
		defer func() { f.Close(); os.Remove(f.Name()) }()
		if result.Body != nil {
			size, err = io.Copy(f, io.LimitReader(result.Body, maxRelayBody+1))
		}
		if err != nil {
			fail(badRequest("relay payload verification failed"))
			return
		}
		if size > maxRelayBody {
			fail(badRequest("relay body exceeds 5 GiB"))
			return
		}
		if err := a.revalidateAfterBody(rc); err != nil {
			fail(a.toAPIError(err))
			return
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			fail(errorf(503, codeInternalError, "cannot read relay body"))
			return
		}
		if size > 0 {
			body = f
		}
	} else if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		fail(badRequest("GET relay must not carry a body"))
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), meta.Method, meta.URL, body)
	if err != nil {
		fail(badRequest("invalid destination request"))
		return
	}
	upstream.ContentLength = size
	for name, value := range meta.Headers {
		lower := strings.ToLower(name)
		if lower == "host" {
			if value != target.Host {
				fail(badRequest("destination Host does not match URL"))
				return
			}
			continue
		}
		if blockedRelayHeader(lower) {
			fail(badRequest("header %s cannot be forwarded", name))
			return
		}
		upstream.Header.Set(name, value)
	}
	// Native media elements vary Range while reusing the same presigned URL.
	// Only these read headers may be added outside the signed metadata.
	if r.Method == "GET" {
		for _, name := range []string{"Range", "If-Range"} {
			if value := r.Header.Get(name); value != "" {
				upstream.Header.Set(name, value)
			}
		}
	}
	tr := discovery.NewGuardedTransport()
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(upstream)
	if err != nil {
		fail(errorf(502, "ProxyFailed", "relay could not reach destination"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
		fail(errorf(502, "ProxyFailed", "destination redirect refused"))
		return
	}
	// Do not forward Set-Cookie, CORS, redirects, or upstream policy headers.
	connectionHeaders := strings.ToLower(strings.Join(resp.Header.Values("Connection"), ","))
	for name, values := range resp.Header {
		lower := strings.ToLower(name)
		if blockedRelayResponseHeader(lower) || strings.HasPrefix(lower, "access-control-") || lower == "content-security-policy" || lower == "referrer-policy" || lower == "x-content-type-options" || strings.Contains(","+strings.ReplaceAll(connectionHeaders, " ", "")+",", ","+lower+",") {
			continue
		}
		w.Header()[name] = values
	}
	// Expose all forwarded metadata, including provider-specific x-amz-meta-*
	// and gos3 backing headers, for consoles on another allowed origin.
	w.Header().Set("X-Gos3-Proxy-Cache-Control", resp.Header.Get("Cache-Control"))
	exposed := []string{relayResultHeader}
	for name := range w.Header() {
		exposed = append(exposed, name)
	}
	w.Header().Set("Access-Control-Expose-Headers", strings.Join(exposed, ", "))
	w.Header().Set(relayResultHeader, "upstream")
	w.Header().Set("Cache-Control", "no-store")
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	if meta.Method != "HEAD" {
		if _, err := io.Copy(w, resp.Body); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func blockedRelayHeader(name string) bool {
	switch name {
	case "cookie", "cookie2", "set-cookie", "set-cookie2", "connection", "proxy-connection", "proxy-authorization", "proxy-authenticate", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade", "content-length", "forwarded", "origin", "referer", "location", "refresh":
		return true
	}
	return strings.HasPrefix(name, "x-forwarded-") || strings.HasPrefix(name, "x-gos3-proxy")
}

// Upstream servers must not change the console origin's security policy,
// storage, reporting destinations, or service-worker scope.
func blockedRelayResponseHeader(name string) bool {
	switch name {
	case "clear-site-data", "alt-svc", "strict-transport-security", "public-key-pins", "public-key-pins-report-only", "report-to", "reporting-endpoints", "nel", "link", "service-worker-allowed", "cross-origin-opener-policy", "cross-origin-embedder-policy", "cross-origin-resource-policy":
		return true
	}
	return blockedRelayHeader(name)
}
