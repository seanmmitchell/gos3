package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func (a *api) listUsers(rc *reqCtx) (int, any, error) {
	users, err := a.d.IAM.ListUsers(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	if users == nil {
		users = []iam.User{}
	}
	return http.StatusOK, map[string]any{"users": users}, nil
}

// createUserRequest is the POST /users body: iam.UserSpec plus createKey.
type createUserRequest struct {
	iam.UserSpec
	CreateKey bool `json:"createKey"`
}

type createUserResponse struct {
	User iam.User       `json:"user"`
	Key  *iam.AccessKey `json:"key,omitempty"`
}

func (a *api) createUser(rc *reqCtx) (int, any, error) {
	var req createUserRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	u, err := a.d.IAM.CreateUser(rc.ctx, req.UserSpec)
	if err != nil {
		return 0, nil, err
	}
	resp := createUserResponse{User: u}
	if req.CreateKey {
		k, err := a.d.IAM.CreateAccessKey(rc.ctx, u.Name, "", "", "")
		if err != nil {
			// Don't leave a half-created user behind.
			if derr := a.d.IAM.DeleteUser(rc.ctx, u.Name); derr != nil {
				a.log.Warn("admin: rolling back user creation failed", "user", u.Name, "err", derr)
			}
			return 0, nil, err
		}
		resp.Key = &k
		if u, err = a.d.IAM.GetUser(rc.ctx, u.Name); err == nil {
			resp.User = u
		}
	}
	a.log.Info("admin: user created", "user", u.Name, "by", rc.id.User)
	return http.StatusCreated, resp, nil
}

func (a *api) getUser(rc *reqCtx) (int, any, error) {
	u, err := a.d.IAM.GetUser(rc.ctx, rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, u, nil
}

func (a *api) updateUser(rc *reqCtx) (int, any, error) {
	var upd iam.UserUpdate
	if err := rc.decodeBody(&upd, false); err != nil {
		return 0, nil, err
	}
	u, err := a.d.IAM.UpdateUser(rc.ctx, rc.r.PathValue("name"), upd)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, u, nil
}

func (a *api) deleteUser(rc *reqCtx) (int, any, error) {
	name := rc.r.PathValue("name")
	if err := a.d.IAM.DeleteUser(rc.ctx, name); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: user deleted", "user", name, "by", rc.id.User)
	return http.StatusNoContent, nil, nil
}

func (a *api) createAccessKey(rc *reqCtx) (int, any, error) {
	var req struct {
		Description string `json:"description"`
		AccessKey   string `json:"accessKey"`
		SecretKey   string `json:"secretKey"`
	}
	if err := rc.decodeBody(&req, true); err != nil {
		return 0, nil, err
	}
	k, err := a.d.IAM.CreateAccessKey(rc.ctx, rc.r.PathValue("name"), req.Description, req.AccessKey, req.SecretKey)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, k, nil
}

func (a *api) updateAccessKey(rc *reqCtx) (int, any, error) {
	var req struct {
		Status string `json:"status"`
	}
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	if req.Status != iam.KeyActive && req.Status != iam.KeyInactive {
		return 0, nil, badRequest("status must be %q or %q", iam.KeyActive, iam.KeyInactive)
	}
	if err := a.d.IAM.UpdateAccessKey(rc.ctx, rc.r.PathValue("accessKey"), req.Status); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

func (a *api) deleteAccessKey(rc *reqCtx) (int, any, error) {
	if err := a.d.IAM.DeleteAccessKey(rc.ctx, rc.r.PathValue("accessKey")); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

func (a *api) listPolicies(rc *reqCtx) (int, any, error) {
	pols, err := a.d.IAM.ListPolicies(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	if pols == nil {
		pols = []iam.NamedPolicy{}
	}
	return http.StatusOK, map[string]any{"policies": pols}, nil
}

func (a *api) getPolicy(rc *reqCtx) (int, any, error) {
	p, err := a.d.IAM.GetPolicy(rc.ctx, rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, p, nil
}

func (a *api) putPolicy(rc *reqCtx) (int, any, error) {
	var req struct {
		Description string          `json:"description"`
		Document    json.RawMessage `json:"document"`
	}
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	raw := bytes.TrimSpace(req.Document)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, nil, errorf(http.StatusBadRequest, codeInvalidPolicy, "document is required")
	}
	// Accept the document as a JSON object or as a JSON string holding it.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, nil, errorf(http.StatusBadRequest, codeInvalidPolicy, "document: invalid JSON string")
		}
		raw = []byte(s)
	}
	doc, err := iam.ParsePolicy(raw)
	if err != nil {
		return 0, nil, err
	}
	p, err := a.d.IAM.PutPolicy(rc.ctx, rc.r.PathValue("name"), req.Description, *doc)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, p, nil
}

func (a *api) deletePolicy(rc *reqCtx) (int, any, error) {
	if err := a.d.IAM.DeletePolicy(rc.ctx, rc.r.PathValue("name")); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// simulateRequest is the POST /authz/simulate body.
type simulateRequest struct {
	User       string                       `json:"user"`
	Action     string                       `json:"action"`
	Bucket     string                       `json:"bucket"`
	Key        string                       `json:"key"`
	Conditions map[string]iam.StringOrSlice `json:"conditions"`
}

// decisionJSON is iam.Decision with the API's JSON names.
type decisionJSON struct {
	Allowed      bool   `json:"allowed"`
	ExplicitDeny bool   `json:"explicitDeny"`
	Reason       string `json:"reason"`
}

// maxSimulateConditions bounds the condition context of a simulation.
const maxSimulateConditions = 128

func (a *api) simulate(rc *reqCtx) (int, any, error) {
	var req simulateRequest
	if err := rc.decodeBody(&req, false); err != nil {
		return 0, nil, err
	}
	req.Action = strings.TrimSpace(req.Action)
	if req.Action == "" || len(req.Action) > 256 || strings.ContainsAny(req.Action, " \t\r\n") {
		return 0, nil, badRequest("action is required (e.g. \"s3:GetObject\")")
	}
	if req.Key != "" && req.Bucket == "" {
		return 0, nil, badRequest("key requires bucket")
	}
	if len(req.Conditions) > maxSimulateConditions {
		return 0, nil, badRequest("at most %d condition keys allowed", maxSimulateConditions)
	}

	var id auth.Identity
	switch {
	case req.User == "":
		id = auth.Anonymous
	case req.User == auth.RootUser:
		id = auth.Identity{User: auth.RootUser, IsRoot: true}
	default:
		u, err := a.d.IAM.GetUser(rc.ctx, req.User)
		if err != nil {
			return 0, nil, err
		}
		id = auth.Identity{User: u.Name}
		for _, k := range u.Keys {
			if k.Status == iam.KeyActive {
				id.AccessKey = k.AccessKey
				break
			}
		}
	}

	areq := iam.AuthzRequest{Identity: id, Action: req.Action, Bucket: req.Bucket, Key: req.Key}
	if len(req.Conditions) > 0 {
		areq.Conditions = make(map[string][]string, len(req.Conditions))
		for k, v := range req.Conditions {
			areq.Conditions[k] = []string(v)
		}
	}
	if req.Bucket != "" {
		if _, err := a.d.Backend.GetBucket(rc.ctx, req.Bucket); err != nil {
			return 0, nil, err
		}
		pol, err := a.bucketPolicy(rc, req.Bucket)
		if err != nil {
			return 0, nil, err
		}
		areq.BucketPolicy = pol
		acl, err := a.bucketACL(rc, req.Bucket)
		if err != nil {
			return 0, nil, err
		}
		if acl != aclPrivate {
			areq.ACL = acl
		}
	}
	d := a.d.IAM.Authorize(rc.ctx, areq)
	return http.StatusOK, decisionJSON{Allowed: d.Allowed, ExplicitDeny: d.ExplicitDeny, Reason: d.Reason}, nil
}

// bucketPolicy returns the parsed bucket policy (nil when unset or
// unparsable — an invalid stored policy grants nothing).
func (a *api) bucketPolicy(rc *reqCtx, bucket string) (*iam.PolicyDocument, error) {
	data, err := a.d.Backend.GetBucketConfig(rc.ctx, bucket, storage.ConfigPolicy)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	doc, perr := iam.ParseBucketPolicy(data, bucket)
	if perr != nil {
		a.log.Warn("admin: stored bucket policy is invalid", "bucket", bucket, "err", perr)
		return nil, nil
	}
	return doc, nil
}
