package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/s3account"
)

// Storage backings and bucket placement (docs/admin-api.md "Storage
// backings and bucket placement"). They need the multi-backing router as
// the backend (always the case in a gos3 server); with another backend the
// endpoints answer 404.

// router returns the multi-backing router, or a 404 error.
func (a *api) router() (*multi.Router, error) {
	if r, ok := a.d.Backend.(*multi.Router); ok {
		return r, nil
	}
	return nil, notFound("storage backings are not available on this server")
}

// backingJSON is the Backing document.
type backingJSON struct {
	multi.BackingStatus
	// Account summarises an s3 backing (nil for other backings).
	Account *accountJSON `json:"account,omitempty"`
	Disk    *diskStats   `json:"disk"`
}

// accountJSON is the AccountInfo document of an s3 backing: its
// configuration summary and its containers (the home first).
type accountJSON struct {
	s3account.AccountInfo
	Containers []s3account.ContainerInfo `json:"containers"`
}

func (a *api) getBackingConfig(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	out, err := r.BackingConfiguration()
	if err != nil {
		return 0, nil, notFound("backing configuration is not available on this server")
	}
	return http.StatusOK, out, nil
}

func (a *api) putBackingConfig(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	var in multi.BackingConfiguration
	if err := rc.decodeBody(&in, false); err != nil {
		return 0, nil, err
	}
	if err := a.checkBackingEndpoints(rc, r, in); err != nil {
		return 0, nil, err
	}
	out, err := r.ConfigureBackings(rc.ctx, in)
	if err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: backing configuration updated", "user", rc.id.User)
	return http.StatusOK, out, nil
}

// checkBackingEndpoints refuses (400) an s3 backing of in whose endpoint
// is new or changed and is this server (see checkNotSelf): the backing
// would store its data back into this server. Unchanged endpoints are not
// probed again.
func (a *api) checkBackingEndpoints(rc *reqCtx, r *multi.Router, in multi.BackingConfiguration) error {
	current := map[string]string{} // backing name -> configured s3 endpoint
	if cur, err := r.BackingConfiguration(); err == nil {
		for _, d := range cur.Backings {
			if d.Backend == config.BackendS3 && d.S3 != nil {
				current[d.Name] = d.S3.Endpoint
			}
		}
	}
	for _, d := range in.Backings {
		if !strings.EqualFold(strings.TrimSpace(d.Backend), config.BackendS3) || d.S3 == nil {
			continue
		}
		ep := strings.TrimRight(strings.TrimSpace(d.S3.Endpoint), "/")
		if ep == "" {
			continue // from the environment (BackingEnv): the operator's choice
		}
		if old, ok := current[strings.TrimSpace(d.Name)]; ok && old == ep {
			continue
		}
		if err := a.checkNotSelf(rc.ctx, ep); err != nil {
			return err
		}
	}
	return nil
}

func (a *api) listBackings(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	list := r.Backings()
	out := make([]backingJSON, len(list))
	for i, b := range list {
		out[i] = backingJSON{BackingStatus: b}
		// Disk statistics describe a local directory: fs backings only
		// (memory and s3 backings have none).
		if b.Kind == config.BackendFS && b.Path != "" {
			out[i].Disk = diskUsage(b.Path)
		}
		if rm, ok := remoteManagerOf(r, b.Name); ok {
			info := rm.Info()
			out[i].Account = &accountJSON{AccountInfo: info, Containers: containersOf(rc, rm)}
			if len(b.Kinds) == 0 {
				for _, k := range info.Kinds {
					out[i].Kinds = append(out[i].Kinds, string(k))
				}
				out[i].DefaultKind = string(info.DefaultKind)
			}
		}
	}
	return http.StatusOK, map[string]any{
		"defaultBacking": r.DefaultBacking(),
		"autoFailover":   r.AutoFailover(),
		"backings":       out,
	}, nil
}

func (a *api) getPlacement(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	st, err := r.Placement(rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, st, nil
}

// placementRequest is the body of PUT /buckets/{name}/placement.
type placementRequest struct {
	Mode     multi.Mode `json:"mode"`
	Replicas []string   `json:"replicas"`
}

func (a *api) putPlacement(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	var req placementRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	st, err := r.SetReplication(rc.ctx, rc.r.PathValue("name"), req.Mode, req.Replicas)
	if err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: bucket replication changed", "bucket", st.Bucket, "mode", st.Mode,
		"replicas", req.Replicas, "by", describeIdentity(rc.id))
	return http.StatusOK, st, nil
}

// moveRequest is the body of POST /buckets/{name}/placement/move.
type moveRequest struct {
	To   string `json:"to"`
	From string `json:"from"`
	// Container chooses the container of the copy on an s3 "to" backing
	// ("" = the backing's default; such copies are wrapped buckets).
	Container string `json:"container"`
}

func (a *api) startMove(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	var req moveRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	if req.To == "" {
		return 0, nil, badRequest("\"to\" (the target backing) is required")
	}
	var target *storage.BucketTarget
	if req.Container != "" {
		if !validContainerID(req.Container) {
			return 0, nil, badRequest("\"container\" must be a container ID (\"home\" or up to %d lowercase letters, digits and '-'), got %q",
				maxContainerID, req.Container)
		}
		target = &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: req.Container}
	}
	st, err := r.Move(rc.ctx, rc.r.PathValue("name"), req.From, req.To, target)
	if err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: bucket move started", "bucket", st.Bucket, "to", req.To, "container", req.Container,
		"by", describeIdentity(rc.id))
	return http.StatusAccepted, st, nil
}

func (a *api) cancelMove(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	st, err := r.CancelMove(rc.ctx, rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: bucket move canceled", "bucket", st.Bucket, "by", describeIdentity(rc.id))
	return http.StatusOK, st, nil
}

// promoteRequest is the body of POST /buckets/{name}/placement/promote.
type promoteRequest struct {
	Backing string `json:"backing"`
	Force   bool   `json:"force"`
}

func (a *api) promote(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	var req promoteRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	if req.Backing == "" {
		return 0, nil, badRequest("\"backing\" (the replica to promote) is required")
	}
	st, err := r.Promote(rc.ctx, rc.r.PathValue("name"), req.Backing, req.Force)
	if err != nil {
		return 0, nil, err
	}
	a.log.Warn("admin: bucket replica promoted to primary", "bucket", st.Bucket, "backing", req.Backing,
		"force", req.Force, "by", describeIdentity(rc.id))
	return http.StatusOK, st, nil
}

// resyncRequest is the body of POST /buckets/{name}/placement/resync.
type resyncRequest struct {
	Backing string `json:"backing"`
}

func (a *api) resync(rc *reqCtx) (int, any, error) {
	r, err := a.router()
	if err != nil {
		return 0, nil, err
	}
	var req resyncRequest
	if err := rc.decodeBody(&req, true); err != nil {
		return 0, nil, err
	}
	st, err := r.Resync(rc.ctx, rc.r.PathValue("name"), req.Backing)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, st, nil
}

// placementError maps the router's placement errors (nil otherwise).
func placementError(err error, msg string) *apiError {
	switch {
	case errors.Is(err, multi.ErrUnknownBacking), errors.Is(err, multi.ErrInvalidPlacement):
		return errorf(http.StatusBadRequest, codeInvalidRequest, "%s", msg)
	case errors.Is(err, multi.ErrConflict):
		return errorf(http.StatusConflict, codeConflict, "%s", msg)
	}
	return nil
}
