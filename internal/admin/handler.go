package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/clientip"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// maxBodySize bounds JSON request bodies.
const maxBodySize = 1 << 20

// Error codes of the admin API (docs/admin-api.md "Errors").
const (
	codeInvalidRequest   = "InvalidRequest"
	codeInvalidName      = "InvalidName"
	codeInvalidPolicy    = "InvalidPolicy"
	codeInvalidSettings  = "InvalidSettings"
	codeUnauthenticated  = "Unauthenticated"
	codeAccessDenied     = "AccessDenied"
	codeNotFound         = "NotFound"
	codeMethodNotAllowed = "MethodNotAllowed"
	codeConflict         = "Conflict"
	codeInternalError    = "InternalError"
	codeProbeFailed      = "ProbeFailed"
	codeTooManyRequests  = "TooManyRequests"
)

// apiError is an error rendered as the admin JSON error envelope.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errorf(status int, code, format string, args ...any) *apiError {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &apiError{Status: status, Code: code, Message: msg}
}

func badRequest(format string, args ...any) *apiError {
	return errorf(http.StatusBadRequest, codeInvalidRequest, format, args...)
}

func notFound(format string, args ...any) *apiError {
	return errorf(http.StatusNotFound, codeNotFound, format, args...)
}

// reqCtx is the per-request state passed to endpoint handlers.
type reqCtx struct {
	r      *http.Request
	ctx    context.Context
	auth   *auth.Result
	id     auth.Identity
	action string // admin action, "" for endpoints open to any identity
	// keyOwner is the owner of the {accessKey} path value that the route's
	// action was authorized against (set by keyOwnerARN; "" when the key
	// was unknown and the action was authorized at service level).
	keyOwner string

	start         time.Time // when the endpoint received the request
	preAuthorized bool      // action was authorized on resource before the handler ran
	resource      string    // the resource action was authorized against
	// afterBody, when set, runs once readBody has read the whole body
	// (api.revalidateAfterBody).
	afterBody func() error
}

// handlerFunc is an authenticated endpoint. It returns the HTTP status and
// a value to encode as JSON (nil with 204), or an error.
type handlerFunc func(rc *reqCtx) (status int, body any, err error)

// route is one endpoint of the API.
type route struct {
	method string
	path   string // relative to Prefix, Go ServeMux pattern syntax
	action string // IAM action without the "admin:" prefix; "" = any identity
	res    target // what action is evaluated against
	h      handlerFunc
}

// target says which resource a route's action is evaluated against
// (docs/security.md "Scoping admin permissions").
type target struct {
	// resource returns the admin resource ARN of the request (nil: service
	// level, i.e. matched by "*" and arn:gos3:admin:::* only).
	resource func(rc *reqCtx) string
	// inHandler: the target or condition keys come from the request body,
	// so the handler authorizes rc.action itself (requireAction) after
	// decoding it and before acting on it.
	inHandler bool
}

var (
	service   = target{}
	inHandler = target{inHandler: true}
)

// pathTarget evaluates against the admin ARN that arn builds from the
// path value param (e.g. iam.AdminUserARN of {name}).
func pathTarget(arn func(string) string, param string) target {
	return target{resource: func(rc *reqCtx) string { return arn(rc.r.PathValue(param)) }}
}

// api is the admin API handler.
type api struct {
	encryptionMu sync.Mutex
	d            Deps
	log          *slog.Logger
	mux          *http.ServeMux
	started      time.Time
	now          func() time.Time

	usage  *ttlCache[storage.BucketUsage] // per bucket
	totals *ttlCache[storageTotals]       // key ""
	scans  chan struct{}                  // running network scans (semaphore)
	probes chan struct{}                  // running probes (semaphore)
}

// usageTTL is how long computed storage usage is cached.
const usageTTL = 30 * time.Second

// usageComputeTimeout bounds one (detached) usage computation.
const usageComputeTimeout = 2 * time.Minute

func newAPI(d Deps) *api {
	if d.Backend == nil || d.Verifier == nil || d.IAM == nil || d.Settings == nil {
		panic("admin: Deps.Backend, Verifier, IAM and Settings are required")
	}
	a := &api{d: d, log: d.Logger, now: time.Now,
		scans:  make(chan struct{}, maxConcurrentScans),
		probes: make(chan struct{}, maxConcurrentProbes),
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	a.started = a.now()
	clock := func() time.Time { return a.now() }
	a.usage = newTTLCache[storage.BucketUsage](usageTTL, usageComputeTimeout, clock)
	a.totals = newTTLCache[storageTotals](usageTTL, usageComputeTimeout, clock)
	a.mux = http.NewServeMux()
	a.register(a.routes())
	return a
}

// routes lists every endpoint (docs/admin-api.md).
func (a *api) routes() []route {
	user := pathTarget(iam.AdminUserARN, "name")
	policy := pathTarget(iam.AdminPolicyARN, "name")
	bucket := pathTarget(iam.AdminBucketARN, "name")
	keyOwner := target{resource: a.keyOwnerARN}
	return []route{
		// Identity
		{"GET", "/whoami", "", service, a.whoami},
		{"GET", "/whoami/keys", "", service, a.listOwnKeys},
		{"POST", "/whoami/keys", "CreateOwnAccessKey", service, a.createOwnKey},
		{"DELETE", "/whoami/keys/{accessKey}", "", service, a.deleteOwnKey}, // unless admin:DeleteOwnAccessKey is denied
		// Server
		{"GET", "/info", "ServerInfo", service, a.serverInfo},
		{"GET", "/stats", "GetStats", service, a.stats},
		{"GET", "/config", "GetConfig", service, a.getConfig},
		{"PUT", "/config", "UpdateConfig", service, a.putConfig},
		// Users & access keys
		{"GET", "/users", "ListUsers", service, a.listUsers},
		{"POST", "/users", "CreateUser", inHandler, a.createUser}, // user/<body name>, admin:PolicyName
		{"GET", "/users/{name}", "GetUser", user, a.getUser},
		{"PATCH", "/users/{name}", "UpdateUser", inHandler, a.updateUser}, // user/{name}, admin:PolicyName
		{"DELETE", "/users/{name}", "DeleteUser", user, a.deleteUser},
		{"POST", "/users/{name}/keys", "CreateAccessKey", user, a.createAccessKey},
		{"PATCH", "/keys/{accessKey}", "UpdateAccessKey", keyOwner, a.updateAccessKey},
		{"DELETE", "/keys/{accessKey}", "DeleteAccessKey", keyOwner, a.deleteAccessKey},
		// Policies
		{"GET", "/policies", "ListPolicies", service, a.listPolicies},
		{"GET", "/policies/{name}", "GetPolicy", policy, a.getPolicy},
		{"PUT", "/policies/{name}", "PutPolicy", policy, a.putPolicy},
		{"DELETE", "/policies/{name}", "DeletePolicy", policy, a.deletePolicy},
		{"POST", "/authz/simulate", "SimulatePolicy", service, a.simulate},
		// Buckets
		{"GET", "/buckets", "ListBuckets", service, a.listBuckets},
		{"GET", "/buckets/{name}", "GetBucket", bucket, a.getBucket},
		{"DELETE", "/buckets/{name}", "ForceDeleteBucket", bucket, a.deleteBucket},
		// Storage backings and bucket placement
		{"GET", "/backings", "ListBackings", service, a.listBackings},
		{"GET", "/buckets/{name}/placement", "GetBucketPlacement", bucket, a.getPlacement},
		{"PUT", "/buckets/{name}/placement", "UpdateBucketPlacement", bucket, a.putPlacement},
		{"POST", "/buckets/{name}/placement/move", "UpdateBucketPlacement", bucket, a.startMove},
		{"DELETE", "/buckets/{name}/placement/move", "UpdateBucketPlacement", bucket, a.cancelMove},
		{"POST", "/buckets/{name}/placement/promote", "UpdateBucketPlacement", bucket, a.promote},
		{"POST", "/buckets/{name}/placement/resync", "UpdateBucketPlacement", bucket, a.resync},
		{"GET", "/buckets/{name}/encryption-provider", "GetBucketEncryption", bucket, a.getBucketEncryption},
		{"PUT", "/buckets/{name}/encryption-provider", "PutBucketEncryption", bucket, a.putBucketEncryption},
		{"GET", "/buckets/{name}/compression", "GetBucketCompression", bucket, a.getBucketCompression},
		{"PUT", "/buckets/{name}/compression", "PutBucketCompression", bucket, a.putBucketCompression},
		{"DELETE", "/buckets/{name}/compression", "PutBucketCompression", bucket, a.deleteBucketCompression},
		// Discovery
		{"GET", "/discovery", "GetDiscovery", service, a.getDiscovery},
		{"POST", "/discovery/probe", "ProbeNetwork", service, a.probe},
		{"POST", "/discovery/scan", "ScanNetwork", service, a.scan},
	}
}

// keyOwnerARN is the target of the {accessKey} routes: the user owning the
// key, or service level for unknown keys (so a delegated administrator
// cannot tell unknown keys from other users' keys). It records the owner
// in rc.keyOwner so the handler can bind the change to it (see
// updateAccessKey).
func (a *api) keyOwnerARN(rc *reqCtx) string {
	owner, err := a.d.IAM.AccessKeyOwner(rc.ctx, rc.r.PathValue("accessKey"))
	if err != nil {
		return ""
	}
	rc.keyOwner = owner
	return iam.AdminUserARN(owner)
}

// register installs the routes on the mux. Every path additionally gets a
// method-less pattern answering 405 with the Allow header (more specific
// method patterns win), and Prefix+"/" catches unknown paths with a JSON 404.
func (a *api) register(routes []route) {
	allowed := map[string][]string{}
	var order []string
	for _, rt := range routes {
		a.mux.Handle(rt.method+" "+Prefix+rt.path, a.endpoint(rt))
		if _, ok := allowed[rt.path]; !ok {
			order = append(order, rt.path)
		}
		allowed[rt.path] = append(allowed[rt.path], rt.method)
	}
	for _, p := range order {
		methods := allowed[p]
		for _, m := range methods {
			if m == http.MethodGet {
				methods = append(methods, http.MethodHead)
				break
			}
		}
		allow := strings.Join(append(methods, http.MethodOptions), ", ")
		a.mux.HandleFunc(Prefix+p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			writeError(w, errorf(http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method %s is not allowed on %s (allowed: %s)", r.Method, r.URL.Path, allow))
		})
	}
	a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, notFound("unknown admin API path %s", r.URL.Path))
	})
}

// ServeHTTP implements http.Handler.
func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			a.log.Error("admin: panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
			writeError(w, errorf(http.StatusInternalServerError, codeInternalError, "internal server error"))
		}
	}()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	cors := a.d.Settings.Get().CORS
	if r.Method == http.MethodOptions {
		a.preflight(w, r, cors)
		return
	}
	applyCORS(w, r, cors)

	p := r.URL.Path
	if p != Prefix && !strings.HasPrefix(p, Prefix+"/") {
		writeError(w, notFound("unknown admin API path %s", p))
		return
	}
	// Reject non-canonical paths ourselves: the mux would answer them with a
	// (non-JSON) redirect.
	if cleaned := path.Clean(p); cleaned != p && cleaned+"/" != p {
		writeError(w, notFound("unknown admin API path %s", p))
		return
	}
	a.mux.ServeHTTP(w, r)
}

// endpoint wraps a route with authentication, authorization and response
// rendering.
func (a *api) endpoint(rt route) http.Handler {
	action := ""
	if rt.action != "" {
		action = "admin:" + rt.action
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, err := a.d.Verifier.Authenticate(r)
		if err != nil {
			e := s3err.As(err)
			writeError(w, errorf(http.StatusUnauthorized, codeUnauthenticated, "%s: %s", e.Code, e.Message))
			return
		}
		if res.Body != nil {
			defer res.Body.Close()
		}
		if res.Identity.IsAnonymous {
			writeError(w, errorf(http.StatusUnauthorized, codeUnauthenticated,
				"AccessDenied: anonymous requests are not allowed; sign the request with AWS Signature Version 4"))
			return
		}
		if e := checkSignedPayload(r, res); e != nil {
			writeError(w, e)
			return
		}
		rc := &reqCtx{r: r, ctx: r.Context(), auth: res, id: res.Identity, action: action, start: a.now()}
		rc.afterBody = func() error { return a.revalidateAfterBody(rc) }
		if action != "" && !rt.res.inHandler {
			if rt.res.resource != nil {
				rc.resource = rt.res.resource(rc)
			}
			if err := a.requireAction(rc, action, rc.resource, nil); err != nil {
				writeError(w, a.toAPIError(err))
				return
			}
			rc.preAuthorized = true
		}
		status, body, err := rt.h(rc)
		if err != nil {
			ae := a.toAPIError(err)
			if ae.Status >= 500 {
				a.log.Error("admin: request failed", "method", r.Method, "path", r.URL.Path,
					"user", rc.id.User, "err", err)
			}
			writeError(w, ae)
			return
		}
		if status == http.StatusNoContent || body == nil {
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, body)
	})
}

// signedPayloadMessage explains why checkSignedPayload refused a request.
const signedPayloadMessage = "admin API requests with a body must sign the payload: " +
	"set X-Amz-Content-Sha256 to the hex SHA-256 of the body"

// checkSignedPayload refuses admin requests whose body the signature does
// not cover. With UNSIGNED-PAYLOAD (or a presigned URL, or unsigned
// streaming) SigV4 signs only the method, path, query and headers, so a
// captured request could be replayed within the clock-skew window with a
// different body: a policy document, a user's policies, the settings.
// Every POST, PUT and PATCH, and any other request carrying a body, must
// therefore use header authentication with the body's hex SHA-256 (which
// auth verifies while the handler reads the body) or signed aws-chunked
// streaming.
func checkSignedPayload(r *http.Request, res *auth.Result) *apiError {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		if r.ContentLength == 0 && len(r.TransferEncoding) == 0 {
			return nil
		}
	}
	if res.Method == auth.MethodHeaderV4 {
		switch m := res.PayloadMode; {
		case m == auth.PayloadStreamingSigned, m == auth.PayloadStreamingSignedTrailer, isHexSHA256(m):
			return nil
		}
	}
	return errorf(http.StatusBadRequest, codeInvalidRequest, "%s", signedPayloadMessage)
}

// isHexSHA256 reports whether s is a hex-encoded SHA-256 digest.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// authorize evaluates an admin action for the request's identity against
// resource (an iam.Admin*ARN; "" = service level) with the request's
// condition keys plus extra.
func (a *api) authorize(rc *reqCtx, action, resource string, extra map[string][]string) iam.Decision {
	conds := requestConditions(rc.r, a.d.TrustProxyHeaders)
	for k, v := range extra {
		conds[k] = v
	}
	return a.d.IAM.Authorize(rc.ctx, iam.AuthzRequest{
		Identity:   rc.id,
		Action:     action,
		Resource:   resource,
		Conditions: conds,
	})
}

// requireAction returns a 403 AccessDenied error unless the caller is
// allowed action on resource (see authorize). Handlers of inHandler routes
// use it for the route's own action once the body names the target, and
// any handler for actions beyond the route's own, e.g. when one request
// body also does what another route gates separately.
func (a *api) requireAction(rc *reqCtx, action, resource string, extra map[string][]string) error {
	if !a.authorize(rc, action, resource, extra).Allowed {
		on := ""
		if resource != "" {
			on = " on " + resource
		}
		return errorf(http.StatusForbidden, codeAccessDenied,
			"%s is not allowed to perform %s%s", describeIdentity(rc.id), action, on)
	}
	return nil
}

// requestConditions builds the IAM condition context of a request.
// aws:SourceIp and aws:SecureTransport come from the connection or, behind a
// trusted proxy, from X-Forwarded-For / X-Forwarded-Proto (see ClientInfo).
func requestConditions(r *http.Request, proxy clientip.Resolver) map[string][]string {
	ip, secure := ClientInfo(r, proxy)
	c := map[string][]string{
		"aws:SecureTransport": {strconv.FormatBool(secure)},
	}
	if ip != "" {
		c["aws:SourceIp"] = []string{ip}
	}
	if ua := r.UserAgent(); ua != "" {
		c["aws:UserAgent"] = []string{ua}
	}
	if ref := r.Referer(); ref != "" {
		c["aws:Referer"] = []string{ref}
	}
	return c
}

// ClientInfo returns the client IP address of r and whether it reached the
// server over TLS, honouring X-Forwarded-For / X-Forwarded-Proto from the
// trusted proxies of proxy: the same rule the S3 API applies (see package
// clientip).
func ClientInfo(r *http.Request, proxy clientip.Resolver) (ip string, secure bool) {
	return proxy.ClientInfo(r)
}

func describeIdentity(id auth.Identity) string {
	switch {
	case id.IsAnonymous:
		return "anonymous"
	case id.IsRoot:
		return "root"
	case id.User != "":
		return "user " + strconv.Quote(id.User)
	}
	return "access key " + strconv.Quote(id.AccessKey)
}

// toAPIError maps handler errors (iam, storage, s3err, context) to the
// admin error envelope.
func (a *api) toAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	msg := strings.TrimPrefix(err.Error(), "iam: ")
	msg = strings.TrimPrefix(msg, "storage: ")
	msg = strings.TrimPrefix(msg, "multi: ")
	if pe := placementError(err, msg); pe != nil {
		return pe
	}
	switch {
	// iam (order matters: some errors wrap others)
	case errors.Is(err, iam.ErrUserNotFound), errors.Is(err, iam.ErrKeyNotFound), errors.Is(err, iam.ErrPolicyNotFound):
		return errorf(http.StatusNotFound, codeNotFound, "%s", msg)
	case errors.Is(err, iam.ErrUserExists), errors.Is(err, iam.ErrPolicyInUse):
		return errorf(http.StatusConflict, codeConflict, "%s", msg)
	case errors.Is(err, iam.ErrInvalidName):
		return errorf(http.StatusBadRequest, codeInvalidName, "%s", msg)
	case errors.Is(err, iam.ErrInvalidPolicy):
		return errorf(http.StatusBadRequest, codeInvalidPolicy, "%s", msg)
	case errors.Is(err, iam.ErrTooManyKeys), errors.Is(err, iam.ErrPolicyBuiltin), errors.Is(err, iam.ErrRootImmutable):
		return errorf(http.StatusBadRequest, codeInvalidRequest, "%s", msg)
	// storage
	case errors.Is(err, storage.ErrBucketNotFound):
		return errorf(http.StatusNotFound, codeNotFound, "%s", msg)
	case errors.Is(err, storage.ErrBucketNotEmpty):
		return errorf(http.StatusConflict, codeConflict, "%s", msg)
	case errors.Is(err, storage.ErrInvalidName):
		return errorf(http.StatusBadRequest, codeInvalidName, "%s", msg)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errorf(http.StatusServiceUnavailable, codeInternalError, "request canceled or timed out: %s", msg)
	}
	var se *s3err.Error
	if errors.As(err, &se) && se.HTTPStatus >= 400 && se.HTTPStatus < 500 {
		return errorf(http.StatusBadRequest, codeInvalidRequest, "%s: %s", se.Code, se.Message)
	}
	return errorf(http.StatusInternalServerError, codeInternalError, "%s", msg)
}

// errorBody is the JSON error envelope.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, errorBody{Error: errorDetail{Code: e.Code, Message: e.Message}})
}

// writeJSON encodes v (fully, before writing headers) and sends it.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		buf.Reset()
		status = http.StatusInternalServerError
		_ = json.NewEncoder(&buf).Encode(errorBody{Error: errorDetail{
			Code: codeInternalError, Message: "encoding response: " + err.Error(),
		}})
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// readBody reads the (verified) request body, bounded by maxBodySize.
// Signature / payload hash failures surface here (auth verifies at EOF).
// Once the body is read, rc.afterBody re-checks that the caller may still
// make the request (see api.revalidateAfterBody).
func (rc *reqCtx) readBody() ([]byte, error) {
	if rc.auth == nil || rc.auth.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(rc.auth.Body, maxBodySize+1))
	if err != nil {
		var se *s3err.Error
		if errors.As(err, &se) {
			if se.Code == s3err.ErrSignatureDoesNotMatch.Code {
				return nil, errorf(http.StatusUnauthorized, codeUnauthenticated, "%s: %s", se.Code, se.Message)
			}
			return nil, badRequest("%s: %s", se.Code, se.Message)
		}
		return nil, badRequest("reading request body: %v", err)
	}
	if len(data) > maxBodySize {
		return nil, badRequest("request body exceeds %d bytes", maxBodySize)
	}
	if rc.afterBody != nil {
		if err := rc.afterBody(); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// decodeBody strictly decodes a JSON body into v (unknown fields and
// trailing data are rejected). An empty body is an error unless optional.
func (rc *reqCtx) decodeBody(v any, optional bool) error {
	data, err := rc.readBody()
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if optional {
			return nil
		}
		return badRequest("request body is required")
	}
	return decodeStrict(data, v)
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %s", jsonErrorMessage(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return badRequest("invalid JSON body: unexpected data after the JSON value")
	}
	return nil
}

func jsonErrorMessage(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if te.Field != "" {
			return fmt.Sprintf("field %q must be of type %s", te.Field, te.Type)
		}
		return fmt.Sprintf("value must be of type %s", te.Type)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("%v (at offset %d)", se, se.Offset)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected end of JSON input"
	}
	return strings.TrimPrefix(err.Error(), "json: ")
}

// queryBool parses an optional boolean query parameter.
func queryBool(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, badRequest("query parameter %q must be a boolean, got %q", name, v)
	}
	return b, nil
}

// info returns this server's identity document (zero value when unset).
func (a *api) info() discovery.Info {
	if a.d.Info == nil {
		return discovery.Info{Service: discovery.ServiceName, APIVersion: "v1", Features: []string{}}
	}
	inf := a.d.Info()
	if inf.Features == nil {
		inf.Features = []string{}
	}
	return inf
}
