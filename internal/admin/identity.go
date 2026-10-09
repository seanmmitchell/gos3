package admin

import (
	"net/http"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
)

// whoAmI is the GET /whoami response.
type whoAmI struct {
	User         string         `json:"user"`
	AccessKey    string         `json:"accessKey"`
	IsRoot       bool           `json:"isRoot"`
	Policies     []string       `json:"policies"`
	Capabilities capabilities   `json:"capabilities"`
	Server       discovery.Info `json:"server"`
}

// capabilities are derived by evaluating admin actions for the caller.
type capabilities struct {
	Admin          bool `json:"admin"`          // any admin:* allowed
	ViewInfo       bool `json:"viewInfo"`       // admin:ServerInfo
	ViewStats      bool `json:"viewStats"`      // admin:GetStats
	ManageConfig   bool `json:"manageConfig"`   // admin:UpdateConfig
	ManageUsers    bool `json:"manageUsers"`    // admin:CreateUser
	ManagePolicies bool `json:"managePolicies"` // admin:PutPolicy
	ManageBuckets  bool `json:"manageBuckets"`  // admin:ForceDeleteBucket
	Discovery      bool `json:"discovery"`      // admin:GetDiscovery
	Scan           bool `json:"scan"`           // admin:ScanNetwork
}

// adminActions are every admin:* action used by the API; "admin" in
// capabilities is true when any of them is allowed.
var adminActions = []string{
	"ServerInfo", "GetStats", "GetConfig", "UpdateConfig",
	"ListUsers", "CreateUser", "GetUser", "UpdateUser", "DeleteUser",
	"CreateAccessKey", "UpdateAccessKey", "DeleteAccessKey",
	"ListPolicies", "GetPolicy", "PutPolicy", "DeletePolicy", "SimulatePolicy",
	"ListBuckets", "GetBucket", "ForceDeleteBucket",
	"GetDiscovery", "ScanNetwork",
}

func (a *api) whoami(rc *reqCtx) (int, any, error) {
	allowed := make(map[string]bool, len(adminActions))
	anyAllowed := false
	for _, act := range adminActions {
		ok := a.authorize(rc, "admin:"+act).Allowed
		allowed[act] = ok
		anyAllowed = anyAllowed || ok
	}
	pols, err := a.d.IAM.EffectivePolicies(rc.ctx, rc.id)
	if err != nil {
		return 0, nil, err
	}
	names := make([]string, 0, len(pols))
	for _, p := range pols {
		names = append(names, p.Name)
	}
	user := rc.id.User
	if rc.id.IsRoot && user == "" {
		user = auth.RootUser
	}
	return http.StatusOK, whoAmI{
		User:      user,
		AccessKey: rc.id.AccessKey,
		IsRoot:    rc.id.IsRoot,
		Policies:  names,
		Capabilities: capabilities{
			Admin:          anyAllowed,
			ViewInfo:       allowed["ServerInfo"],
			ViewStats:      allowed["GetStats"],
			ManageConfig:   allowed["UpdateConfig"],
			ManageUsers:    allowed["CreateUser"],
			ManagePolicies: allowed["PutPolicy"],
			ManageBuckets:  allowed["ForceDeleteBucket"],
			Discovery:      allowed["GetDiscovery"],
			Scan:           allowed["ScanNetwork"],
		},
		Server: a.info(),
	}, nil
}

// selfUser returns the IAM user record of the caller (root has none).
func (a *api) selfUser(rc *reqCtx) (iam.User, error) {
	if rc.id.IsRoot {
		return iam.User{}, badRequest("the root user has no IAM access keys; root credentials come from the server configuration")
	}
	return a.d.IAM.GetUser(rc.ctx, rc.id.User)
}

func (a *api) listOwnKeys(rc *reqCtx) (int, any, error) {
	keys := []iam.AccessKey{}
	if !rc.id.IsRoot {
		u, err := a.selfUser(rc)
		if err != nil {
			return 0, nil, err
		}
		for _, k := range u.Keys {
			k.SecretKey = ""
			keys = append(keys, k)
		}
	}
	return http.StatusOK, map[string]any{"keys": keys}, nil
}

func (a *api) createOwnKey(rc *reqCtx) (int, any, error) {
	var body struct {
		Description string `json:"description"`
	}
	if err := rc.decodeBody(&body, true); err != nil {
		return 0, nil, err
	}
	if _, err := a.selfUser(rc); err != nil {
		return 0, nil, err
	}
	k, err := a.d.IAM.CreateAccessKey(rc.ctx, rc.id.User, body.Description, "", "")
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, k, nil
}

func (a *api) deleteOwnKey(rc *reqCtx) (int, any, error) {
	ak := rc.r.PathValue("accessKey")
	u, err := a.selfUser(rc)
	if err != nil {
		return 0, nil, err
	}
	owned := false
	for _, k := range u.Keys {
		if k.AccessKey == ak {
			owned = true
			break
		}
	}
	if !owned {
		return 0, nil, notFound("access key %q not found among your keys", ak)
	}
	if ak == rc.id.AccessKey {
		return 0, nil, errorf(http.StatusConflict, codeConflict,
			"cannot delete access key %q: it was used to sign this request", ak)
	}
	if err := a.d.IAM.DeleteAccessKey(rc.ctx, ak); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}
