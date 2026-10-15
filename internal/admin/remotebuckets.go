package admin

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/s3account"
)

// Remote buckets and containers of s3 backings (docs/admin-api.md "Remote
// buckets and containers (s3 backings)"). An s3 backing's backend, as the
// router holds it (multi.Router.BackingBackend), implements
// s3account.RemoteManager: the browser of the remote service's buckets and
// the registry of the containers holding wrapped buckets. Remote I/O runs
// without the router's locks; buckets are created through the router
// (CreateBucket with a storage.BucketTarget), so their placement is
// recorded like any other bucket's.

// Admin actions on arn:gos3:admin:::backing/<name> (besides the routes'
// own) that a request body can require.
const (
	actionCreateRemoteBucket = "admin:CreateRemoteBucket"
	actionAdoptRemoteBucket  = "admin:AdoptRemoteBucket"
	actionManageContainers   = "admin:ManageContainers"
)

// Bounds of the remote bucket browser's query parameters.
const (
	maxRemoteBucketsPage   = 1000 // maxBuckets
	maxRemotePrefixLen     = 255  // prefix (bytes)
	maxRemoteContinuation  = 4096 // continuationToken (bytes)
	maxContainerID         = 63   // container IDs (see validContainerID)
	maxContainerLabelBytes = 128  // container labels
)

// remoteBacking returns the router and the remote manager of the backing
// name: 404 for an unknown backing, 400 for one that is not an s3 backing.
func (a *api) remoteBacking(name string) (*multi.Router, s3account.RemoteManager, error) {
	r, err := a.router()
	if err != nil {
		return nil, nil, err
	}
	b, ok := r.BackingBackend(name)
	if !ok {
		return nil, nil, notFound("unknown backing %q", name)
	}
	rm, ok := b.(s3account.RemoteManager)
	if !ok {
		return nil, nil, badRequest("backing %q is not an s3 backing: it has no remote buckets or containers", name)
	}
	return r, rm, nil
}

// remoteManagerOf returns the remote manager of the backing name, if it is
// an s3 backing.
func remoteManagerOf(r *multi.Router, name string) (s3account.RemoteManager, bool) {
	b, ok := r.BackingBackend(name)
	if !ok {
		return nil, false
	}
	rm, ok := b.(s3account.RemoteManager)
	return rm, ok
}

// containersOf returns the containers of rm, never nil.
func containersOf(rc *reqCtx, rm s3account.RemoteManager) []s3account.ContainerInfo {
	if cs := rm.Containers(rc.ctx); cs != nil {
		return cs
	}
	return []s3account.ContainerInfo{}
}

func (a *api) listRemoteBuckets(rc *reqCtx) (int, any, error) {
	_, rm, err := a.remoteBacking(rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	q := rc.r.URL.Query()
	opts := s3account.ListRemoteOptions{Prefix: q.Get("prefix"), ContinuationToken: q.Get("continuationToken")}
	if len(opts.Prefix) > maxRemotePrefixLen || !utf8.ValidString(opts.Prefix) {
		return 0, nil, badRequest("query parameter \"prefix\" must be valid UTF-8 of at most %d bytes", maxRemotePrefixLen)
	}
	if len(opts.ContinuationToken) > maxRemoteContinuation {
		return 0, nil, badRequest("query parameter \"continuationToken\" is too long")
	}
	if v := q.Get("maxBuckets"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxRemoteBucketsPage {
			return 0, nil, badRequest("query parameter \"maxBuckets\" must be an integer from 1 to %d, got %q", maxRemoteBucketsPage, v)
		}
		opts.MaxBuckets = n
	}
	if opts.Probe, err = queryBool(rc.r, "probe"); err != nil {
		return 0, nil, err
	}
	list, err := rm.RemoteBuckets(rc.ctx, opts)
	if err != nil {
		return 0, nil, err
	}
	if list.Buckets == nil {
		list.Buckets = []s3account.RemoteBucket{}
	}
	return http.StatusOK, list, nil
}

// createRemoteBucketRequest is the body of POST
// /backings/{name}/remote-buckets.
type createRemoteBucketRequest struct {
	Name       string `json:"name"`
	Region     string `json:"region"`
	ObjectLock bool   `json:"objectLock"`
}

func (a *api) createRemoteBucket(rc *reqCtx) (int, any, error) {
	name := rc.r.PathValue("name")
	_, rm, err := a.remoteBacking(name)
	if err != nil {
		return 0, nil, err
	}
	var req createRemoteBucketRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	if !validBucketName(req.Name) {
		return 0, nil, badRequest("\"name\" must be a valid bucket name (3-63 lowercase letters, digits, '-' and '.'), got %q", req.Name)
	}
	if err := checkRegion("region", req.Region); err != nil {
		return 0, nil, err
	}
	if err := rm.CreateRemoteBucket(rc.ctx, req.Name, req.Region, req.ObjectLock); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: remote bucket created", "backing", name, "remoteBucket", req.Name, "region", req.Region,
		"objectLock", req.ObjectLock, "by", describeIdentity(rc.id))
	return http.StatusCreated, s3account.RemoteBucket{Name: req.Name, Region: req.Region, Status: "unmanaged"}, nil
}

func (a *api) listContainers(rc *reqCtx) (int, any, error) {
	_, rm, err := a.remoteBacking(rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"containers": containersOf(rc, rm)}, nil
}

func (a *api) addContainer(rc *reqCtx) (int, any, error) {
	name := rc.r.PathValue("name")
	_, rm, err := a.remoteBacking(name)
	if err != nil {
		return 0, nil, err
	}
	var spec s3account.ContainerSpec
	if err := rc.decodeBody(&spec, false); err != nil {
		return 0, nil, err
	}
	if err := checkContainerSpec("", &spec); err != nil {
		return 0, nil, err
	}
	if err := a.requireContainerSource(rc, name, spec); err != nil {
		return 0, nil, err
	}
	ci, err := rm.AddContainer(rc.ctx, spec)
	if err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: container added", "backing", name, "container", ci.ID, "location", ci.Location.String(),
		"createBucket", spec.CreateBucket, "attach", spec.Attach, "by", describeIdentity(rc.id))
	return http.StatusCreated, ci, nil
}

// requireContainerSource authorizes using the remote location of a new
// container on the backing name, beyond admin:ManageContainers: creating
// its remote bucket needs admin:CreateRemoteBucket, using an existing one
// admin:AdoptRemoteBucket (the same rights as for a native bucket there).
func (a *api) requireContainerSource(rc *reqCtx, name string, spec s3account.ContainerSpec) error {
	action := actionAdoptRemoteBucket
	if spec.CreateBucket {
		action = actionCreateRemoteBucket
	}
	return a.requireAction(rc, action, iam.AdminBackingARN(name), nil)
}

// defaultContainerRequest is the body of PUT
// /backings/{name}/containers/default.
type defaultContainerRequest struct {
	ID string `json:"id"`
}

func (a *api) setDefaultContainer(rc *reqCtx) (int, any, error) {
	name := rc.r.PathValue("name")
	_, rm, err := a.remoteBacking(name)
	if err != nil {
		return 0, nil, err
	}
	var req defaultContainerRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	if _, ok := findContainer(rc, rm, req.ID); !ok {
		return 0, nil, badRequest("backing %q has no container %q", name, req.ID)
	}
	if err := rm.SetDefaultContainer(rc.ctx, req.ID); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: default container changed", "backing", name, "container", req.ID, "by", describeIdentity(rc.id))
	return http.StatusOK, map[string]any{"containers": containersOf(rc, rm)}, nil
}

func (a *api) removeContainer(rc *reqCtx) (int, any, error) {
	name, id := rc.r.PathValue("name"), rc.r.PathValue("id")
	_, rm, err := a.remoteBacking(name)
	if err != nil {
		return 0, nil, err
	}
	// The backing checks again under its own lock; these give the
	// documented answers whatever error it would use.
	ci, ok := findContainer(rc, rm, id)
	switch {
	case !ok:
		return 0, nil, notFound("backing %q has no container %q", name, id)
	case ci.Role == string(s3account.RoleHome):
		return 0, nil, badRequest("container %q is the backing's home; it is removed with the home (backing configuration)", id)
	case ci.Buckets > 0:
		return 0, nil, errorf(http.StatusConflict, codeConflict,
			"container %q holds %d bucket(s); delete or move them first", id, ci.Buckets)
	}
	if err := rm.RemoveContainer(rc.ctx, id); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: container removed (its remote data is kept)", "backing", name, "container", id,
		"location", ci.Location.String(), "by", describeIdentity(rc.id))
	return http.StatusNoContent, nil, nil
}

// findContainer returns the container id of rm.
func findContainer(rc *reqCtx, rm s3account.RemoteManager, id string) (s3account.ContainerInfo, bool) {
	if id == "" {
		return s3account.ContainerInfo{}, false
	}
	for _, c := range rm.Containers(rc.ctx) {
		if c.ID == id {
			return c, true
		}
	}
	return s3account.ContainerInfo{}, false
}

// createOnBackingRequest is the CreateOnBacking body of POST
// /backings/{name}/buckets.
type createOnBackingRequest struct {
	Bucket string `json:"bucket"`
	Kind   string `json:"kind"`
	// Native buckets.
	RemoteBucket string `json:"remoteBucket"`
	Region       string `json:"region"`
	Adopt        bool   `json:"adopt"`
	ObjectLock   bool   `json:"objectLock"`
	// Wrapped buckets.
	Container    string                   `json:"container"`
	NewContainer *s3account.ContainerSpec `json:"newContainer"`
}

// kind returns the bucket kind the request asks for: kind, else the kind
// its other fields imply, else def (the backing's default kind). It
// refuses fields of the other kind.
func (req *createOnBackingRequest) kind(def storage.BucketKind) (storage.BucketKind, error) {
	var native, wrapped []string // fields given that only apply to one kind
	for _, f := range []struct {
		set  bool
		name string
	}{{req.RemoteBucket != "", "remoteBucket"}, {req.Region != "", "region"}, {req.Adopt, "adopt"}} {
		if f.set {
			native = append(native, f.name)
		}
	}
	if req.Container != "" {
		wrapped = append(wrapped, "container")
	}
	if req.NewContainer != nil {
		wrapped = append(wrapped, "newContainer")
	}
	kind := storage.BucketKind(strings.ToLower(req.Kind))
	switch kind {
	case "":
		switch {
		case len(native) > 0 && len(wrapped) > 0:
			return "", badRequest("%s (native buckets) cannot be combined with %s (wrapped buckets)",
				strings.Join(native, ", "), strings.Join(wrapped, ", "))
		case len(native) > 0:
			kind = storage.BucketKindNative
		case len(wrapped) > 0:
			kind = storage.BucketKindWrapped
		default:
			kind = def
		}
	case storage.BucketKindNative, storage.BucketKindWrapped:
	default:
		return "", badRequest("\"kind\" must be %q or %q, got %q", storage.BucketKindNative, storage.BucketKindWrapped, req.Kind)
	}
	switch {
	case kind == storage.BucketKindNative && len(wrapped) > 0:
		return "", badRequest("%s only applies to wrapped buckets", strings.Join(wrapped, ", "))
	case kind == storage.BucketKindWrapped && len(native) > 0:
		return "", badRequest("%s only applies to native buckets", strings.Join(native, ", "))
	case kind != storage.BucketKindNative && kind != storage.BucketKindWrapped:
		return "", badRequest("the backing has no default bucket kind; set \"kind\"")
	}
	return kind, nil
}

// validate checks the fields' syntax and the combinations that do not
// depend on the bucket kind.
func (req *createOnBackingRequest) validate() error {
	if !validBucketName(req.Bucket) {
		return badRequest("\"bucket\" must be a valid bucket name (3-63 lowercase letters, digits, '-' and '.'), got %q", req.Bucket)
	}
	switch {
	case req.RemoteBucket == "":
	case req.Adopt && !validRemoteBucketName(req.RemoteBucket):
		return badRequest("\"remoteBucket\" is not a valid bucket name: %q", req.RemoteBucket)
	case !req.Adopt && !validBucketName(req.RemoteBucket):
		return badRequest("\"remoteBucket\" must be a valid bucket name (3-63 lowercase letters, digits, '-' and '.'), got %q", req.RemoteBucket)
	}
	if err := checkRegion("region", req.Region); err != nil {
		return err
	}
	if req.Adopt && req.ObjectLock {
		return badRequest("\"objectLock\" cannot be combined with \"adopt\": an adopted remote bucket keeps its own object lock setting")
	}
	if req.Container != "" && !validContainerID(req.Container) {
		return badRequest("\"container\" must be a container ID (\"home\" or up to %d lowercase letters, digits and '-'), got %q",
			maxContainerID, req.Container)
	}
	if req.Container != "" && req.NewContainer != nil {
		return badRequest("\"container\" and \"newContainer\" cannot be combined")
	}
	if req.NewContainer != nil {
		return checkContainerSpec("newContainer.", req.NewContainer)
	}
	return nil
}

// createBucketOnBacking creates a gos3 bucket on an s3 backing with an
// explicit kind and location: a native bucket on a new or an adopted
// remote bucket, or a wrapped bucket in a container (optionally added
// first). Authorization (beyond the request's identity) is evaluated here:
// admin:CreateBucket on the new bucket, plus on the backing
// admin:AdoptRemoteBucket to adopt, admin:CreateRemoteBucket to create a
// native bucket's remote bucket, and for newContainer admin:ManageContainers
// with the right to its location (see requireContainerSource).
func (a *api) createBucketOnBacking(rc *reqCtx) (int, any, error) {
	name := rc.r.PathValue("name")
	var req createOnBackingRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	// The route's own action is evaluated here: its target (the new
	// bucket) comes from the body.
	if err := a.requireAction(rc, rc.action, iam.AdminBucketARN(req.Bucket), nil); err != nil {
		return 0, nil, err
	}
	if err := req.validate(); err != nil {
		return 0, nil, err
	}
	r, rm, err := a.remoteBacking(name)
	if err != nil {
		return 0, nil, err
	}
	kind, err := req.kind(rm.Info().DefaultKind)
	if err != nil {
		return 0, nil, err
	}
	backingARN := iam.AdminBackingARN(name)
	switch {
	case kind == storage.BucketKindNative && req.Adopt:
		err = a.requireAction(rc, actionAdoptRemoteBucket, backingARN, nil)
	case kind == storage.BucketKindNative:
		err = a.requireAction(rc, actionCreateRemoteBucket, backingARN, nil)
	case req.NewContainer != nil:
		if err = a.requireAction(rc, actionManageContainers, backingARN, nil); err == nil {
			err = a.requireContainerSource(rc, name, *req.NewContainer)
		}
	}
	if err != nil {
		return 0, nil, err
	}

	target := &storage.BucketTarget{Kind: kind}
	if kind == storage.BucketKindNative {
		target.RemoteBucket, target.RemoteRegion, target.Adopt = req.RemoteBucket, req.Region, req.Adopt
		if target.RemoteBucket == "" {
			target.RemoteBucket = req.Bucket
		}
	} else {
		target.Container = req.Container
	}
	// A taken name is refused before anything is done remotely (the
	// router checks again when creating).
	if _, err := r.Summary(req.Bucket); err == nil {
		return 0, nil, errorf(http.StatusConflict, codeConflict, "bucket %q already exists", req.Bucket)
	}
	// Remote checks and container loading run before the router takes its
	// locks for the creation itself.
	var added string // container added for this bucket
	switch {
	case req.Adopt:
		region, err := rm.CheckAdoptable(rc.ctx, target.RemoteBucket)
		if err != nil {
			return 0, nil, err
		}
		if target.RemoteRegion == "" {
			target.RemoteRegion = region
		}
	case req.NewContainer != nil:
		ci, err := rm.AddContainer(rc.ctx, *req.NewContainer)
		if err != nil {
			return 0, nil, err
		}
		added, target.Container = ci.ID, ci.ID
		a.log.Info("admin: container added", "backing", name, "container", ci.ID, "location", ci.Location.String(),
			"createBucket", req.NewContainer.CreateBucket, "attach", req.NewContainer.Attach, "by", describeIdentity(rc.id))
	}

	owner := ""
	if !rc.id.IsRoot {
		owner = rc.id.User
	}
	err = r.CreateBucket(rc.ctx, req.Bucket, storage.CreateBucketOptions{
		Region:            a.region(),
		ObjectLockEnabled: req.ObjectLock,
		Owner:             owner,
		Backing:           name,
		Target:            target,
	})
	if err != nil {
		if added != "" {
			// The container stays registered (its location now holds a
			// gos3 store): name it so the bucket can be created there.
			return 0, nil, fmt.Errorf("%w (container %q was added and is kept; retry with \"container\": %q)", err, added, added)
		}
		return 0, nil, err
	}
	a.log.Info("admin: bucket created on backing", "bucket", req.Bucket, "backing", name, "kind", kind,
		"remoteBucket", target.RemoteBucket, "adopt", target.Adopt, "container", target.Container, "by", describeIdentity(rc.id))
	info, err := r.GetBucket(rc.ctx, req.Bucket)
	if err != nil {
		return 0, nil, fmt.Errorf("bucket %q was created, but reading it back failed: %w", req.Bucket, err)
	}
	// Without usage: counting an adopted bucket's objects can take long.
	s, err := a.summarize(rc, info, false)
	if err != nil {
		return 0, nil, fmt.Errorf("bucket %q was created, but describing it failed: %w", req.Bucket, err)
	}
	return http.StatusCreated, s, nil
}

// region is the region of new buckets: the server's, as for S3
// CreateBucket without a LocationConstraint.
func (a *api) region() string {
	if r := a.d.Settings.Get().Region; r != "" {
		return r
	}
	return "us-east-1"
}

// releaseBucket forgets a native bucket of an s3 backing, keeping its
// remote bucket and objects.
func (a *api) releaseBucket(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	name := rc.r.PathValue("name")
	if err := r.ReleaseBucket(rc.ctx, name); err != nil {
		if errors.Is(err, storage.ErrNotSupported) {
			return 0, nil, badRequest("bucket %q cannot be released: only native buckets of s3 backings can be "+
				"(other buckets' data would be orphaned: delete the bucket instead)", name)
		}
		return 0, nil, err
	}
	a.invalidateUsage(name)
	a.log.Warn("admin: bucket released (its remote bucket and objects are kept)", "bucket", name, "by", describeIdentity(rc.id))
	return http.StatusNoContent, nil, nil
}

// checkContainerSpec validates and normalizes a ContainerSpec (field
// names in errors are prefixed with prefix): the remote bucket (a valid
// name for a bucket to create, a plausible one for an existing bucket),
// the key prefix (made to end in "/"), the region and the label.
func checkContainerSpec(prefix string, s *s3account.ContainerSpec) error {
	switch {
	case s.Bucket == "":
		return badRequest("%q (the remote bucket) is required", prefix+"bucket")
	case s.CreateBucket && !validBucketName(s.Bucket):
		return badRequest("%q must be a valid bucket name (3-63 lowercase letters, digits, '-' and '.'), got %q", prefix+"bucket", s.Bucket)
	case !validRemoteBucketName(s.Bucket):
		return badRequest("%q is not a valid bucket name: %q", prefix+"bucket", s.Bucket)
	}
	if s.CreateBucket && s.Attach {
		return badRequest("%q and %q cannot be combined: attach uses a location that already holds a gos3 store",
			prefix+"createBucket", prefix+"attach")
	}
	if s.Prefix != "" && !strings.HasSuffix(s.Prefix, "/") {
		s.Prefix += "/"
	}
	if err := checkKeyPrefix(s.Prefix); err != nil {
		return badRequest("%q: %v", prefix+"prefix", err)
	}
	if err := checkRegion(prefix+"region", s.Region); err != nil {
		return err
	}
	s.Label = strings.TrimSpace(s.Label)
	if len(s.Label) > maxContainerLabelBytes || !utf8.ValidString(s.Label) ||
		strings.IndexFunc(s.Label, unicode.IsControl) >= 0 {
		return badRequest("%q must be at most %d bytes of printable UTF-8", prefix+"label", maxContainerLabelBytes)
	}
	return nil
}

// checkKeyPrefix checks a container's key prefix ("" or ending in "/"):
// at most config.MaxS3PrefixLen bytes of UTF-8 without control
// characters, not starting with "/", without empty, "." or ".." segments
// (the rules of an s3 backing's home prefix).
func checkKeyPrefix(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > config.MaxS3PrefixLen || !utf8.ValidString(p) || strings.HasPrefix(p, "/") ||
		strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return fmt.Errorf("must be valid UTF-8 of at most %d bytes without control characters, not starting with '/'", config.MaxS3PrefixLen)
	}
	if slices.ContainsFunc(strings.Split(strings.TrimSuffix(p, "/"), "/"), func(seg string) bool {
		return seg == "" || seg == "." || seg == ".."
	}) {
		return fmt.Errorf("%q has an empty, \".\" or \"..\" segment", p)
	}
	return nil
}

// checkRegion refuses a region that is not "" or a region name.
func checkRegion(field, region string) error {
	if region != "" && !config.ValidRegion(region) {
		return badRequest("%q must be a region name (lowercase letters, digits and '-'), got %q", field, region)
	}
	return nil
}

// validBucketName reports whether name is a valid name for a bucket gos3
// creates: the rules of S3 CreateBucket (internal/s3 validBucketName):
// 3-63 lowercase letters, digits, '-' and '.', starting and ending with a
// letter or digit, not an IPv4 address, no "..", ".-" or "-.", and none of
// the prefixes/suffixes AWS reserves.
func validBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '.':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") || looksLikeIPv4(name) {
		return false
	}
	for _, p := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	for _, sfx := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(name, sfx) {
			return false
		}
	}
	return true
}

// looksLikeIPv4 reports whether s is formatted like a dotted-quad address.
func looksLikeIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

// validRemoteBucketName reports whether name can be an existing remote
// bucket (to adopt or to hold a container): the lenient shape the
// configuration accepts for an s3 backing's home bucket (older services
// allowed names that new buckets may not have).
func validRemoteBucketName(name string) bool {
	if len(name) < 2 || len(name) > 255 {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// validContainerID reports whether id has the shape of a container ID of
// an s3 backing: 1..maxContainerID lowercase letters, digits and '-',
// starting with a letter or digit ("home" names the backing's home). It
// matches the S3 API's x-gos3-container header.
func validContainerID(id string) bool {
	if id == "" || len(id) > maxContainerID {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}
