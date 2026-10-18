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
	// Backings lets every caller pick a backing when creating a bucket.
	Backings backingChoices `json:"backings"`
}

// capabilities are derived by evaluating admin actions for the caller at
// service level: a grant scoped to some users, policies or buckets
// (arn:gos3:admin:::user/team-*) does not show up here.
type capabilities struct {
	Proxy          bool `json:"proxy"`          // admin:ProxyRequest
	Admin          bool `json:"admin"`          // any admin:* allowed
	ViewInfo       bool `json:"viewInfo"`       // admin:ServerInfo
	ViewStats      bool `json:"viewStats"`      // admin:GetStats
	ManageConfig   bool `json:"manageConfig"`   // admin:UpdateConfig
	ManageUsers    bool `json:"manageUsers"`    // admin:CreateUser
	ListUsers      bool `json:"listUsers"`      // admin:ListUsers (also granted to delegated user managers)
	ManagePolicies bool `json:"managePolicies"` // admin:PutPolicy
	ManageBuckets  bool `json:"manageBuckets"`  // admin:ForceDeleteBucket
	ViewStorage    bool `json:"viewStorage"`    // admin:ListBackings
	ManageStorage  bool `json:"manageStorage"`  // admin:UpdateBucketPlacement
	Discovery      bool `json:"discovery"`      // admin:GetDiscovery
	Scan           bool `json:"scan"`           // admin:ScanNetwork
	Probe          bool `json:"probe"`          // admin:ProbeNetwork
	CreateOwnKeys  bool `json:"createOwnKeys"`  // admin:CreateOwnAccessKey (never root)
	// ManageCompression: admin:PutBucketCompression (bucket compression
	// settings).
	ManageCompression bool `json:"manageCompression"`
	// Remote buckets and containers of s3 backings (granted on every
	// backing: a grant on arn:gos3:admin:::backing/<name> does not show).
	BrowseRemote     bool `json:"browseRemote"`     // admin:ListRemoteBuckets
	CreateRemote     bool `json:"createRemote"`     // admin:CreateRemoteBucket
	AdoptRemote      bool `json:"adoptRemote"`      // admin:AdoptRemoteBucket
	ManageContainers bool `json:"manageContainers"` // admin:ManageContainers
	// Background jobs and restic (granted on every bucket: a grant on
	// arn:gos3:admin:::bucket/<name> does not show).
	ViewJobs                bool `json:"viewJobs"`                // admin:ListJobs
	ManageJobs              bool `json:"manageJobs"`              // admin:CreateCopyJob
	ManageSnapshots         bool `json:"manageSnapshots"`         // admin:PutBucketSnapshots
	RevealResticCredentials bool `json:"revealResticCredentials"` // admin:GetResticCredentials
	RunSnapshots            bool `json:"runSnapshots"`            // admin:RunBucketSnapshot
	RestoreSnapshots        bool `json:"restoreSnapshots"`        // admin:RestoreBucketSnapshot
	UpdateJobs              bool `json:"updateJobs"`              // admin:UpdateJob
}

// adminActions are every admin:* action used by the API; "admin" in
// capabilities is true when any of them is allowed.
var adminActions = []string{
	"ServerInfo", "GetStats", "GetConfig", "UpdateConfig",
	"ListUsers", "CreateUser", "GetUser", "UpdateUser", "DeleteUser",
	"CreateAccessKey", "UpdateAccessKey", "DeleteAccessKey",
	"ListPolicies", "GetPolicy", "PutPolicy", "DeletePolicy", "SimulatePolicy",
	"ListBuckets", "GetBucket", "CreateBucket", "ForceDeleteBucket", "ReleaseBucket",
	"GetBucketCompression", "PutBucketCompression", "GetBucketEncryption", "PutBucketEncryption",
	"ListBackings", "GetBucketPlacement", "UpdateBucketPlacement",
	"ListRemoteBuckets", "CreateRemoteBucket", "AdoptRemoteBucket", "ManageContainers",
	"ListJobs", "GetJob", "UpdateJob", "CreateCopyJob",
	"GetResticCredentials", "GetBucketSnapshots", "PutBucketSnapshots", "RunBucketSnapshot", "RestoreBucketSnapshot",
	"GetDiscovery", "ScanNetwork", "ProbeNetwork", "ProxyRequest",
}

func (a *api) whoami(rc *reqCtx) (int, any, error) {
	allowed := make(map[string]bool, len(adminActions))
	anyAllowed := false
	for _, act := range adminActions {
		ok := a.authorize(rc, "admin:"+act, "", nil).Allowed
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
			Admin:             anyAllowed,
			Proxy:             allowed["ProxyRequest"],
			ViewInfo:          allowed["ServerInfo"],
			ViewStats:         allowed["GetStats"],
			ManageConfig:      allowed["UpdateConfig"],
			ManageUsers:       allowed["CreateUser"],
			ListUsers:         allowed["ListUsers"],
			ManagePolicies:    allowed["PutPolicy"],
			ManageBuckets:     allowed["ForceDeleteBucket"],
			ViewStorage:       allowed["ListBackings"],
			ManageStorage:     allowed["UpdateBucketPlacement"],
			Discovery:         allowed["GetDiscovery"],
			Scan:              allowed["ScanNetwork"],
			Probe:             allowed["ProbeNetwork"],
			ManageCompression: allowed["PutBucketCompression"],
			BrowseRemote:      allowed["ListRemoteBuckets"],
			CreateRemote:      allowed["CreateRemoteBucket"],
			AdoptRemote:       allowed["AdoptRemoteBucket"],
			ManageContainers:  allowed["ManageContainers"],
			ViewJobs:          allowed["ListJobs"],
			ManageJobs:        allowed["CreateCopyJob"],
			ManageSnapshots:   allowed["PutBucketSnapshots"],
			// The repository passwords: the UI offers "reveal" only then.
			RevealResticCredentials: allowed["GetResticCredentials"],
			RunSnapshots:            allowed["RunBucketSnapshot"],
			RestoreSnapshots:        allowed["RestoreBucketSnapshot"],
			UpdateJobs:              allowed["UpdateJob"],
			// Root has no IAM keys to manage (POST /whoami/keys answers 400).
			CreateOwnKeys: !rc.id.IsRoot && a.authorize(rc, actionCreateOwnKey, "", nil).Allowed,
		},
		Server:   a.info(),
		Backings: a.backingChoices(rc.ctx),
	}, nil
}

// Self-service access key actions (not in adminActions: they do not make
// the caller an administrator).
//
// Creating a key for oneself needs an explicit Allow of
// admin:CreateOwnAccessKey (the route's action; only the built-in "admin"
// policy grants it): a stolen key must not be able to mint fresh keys that
// survive revoking it. Deleting one's own keys only reduces access, so it
// is allowed to every authenticated user unless admin:DeleteOwnAccessKey
// is explicitly denied (e.g. by a Deny of "admin:*").
const (
	actionCreateOwnKey = "admin:CreateOwnAccessKey"
	actionDeleteOwnKey = "admin:DeleteOwnAccessKey"
)

// authorizeSelfService refuses a self-service action that a policy
// explicitly denies to the caller.
func (a *api) authorizeSelfService(rc *reqCtx, action string) error {
	if rc.id.IsRoot {
		return nil
	}
	if d := a.authorize(rc, action, "", nil); !d.Allowed && d.ExplicitDeny {
		return errorf(http.StatusForbidden, codeAccessDenied,
			"%s is explicitly denied %s", describeIdentity(rc.id), action)
	}
	return nil
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
	// The route already required an explicit Allow of actionCreateOwnKey
	// (root passes that and is refused here).
	if _, err := a.selfUser(rc); err != nil {
		return 0, nil, err
	}
	k, err := a.d.IAM.CreateAccessKey(rc.ctx, rc.id.User, body.Description, "", "")
	if err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: access key created (self-service)", "user", rc.id.User,
		"accessKey", k.AccessKey, "signedBy", rc.id.AccessKey)
	return http.StatusCreated, k, nil
}

func (a *api) deleteOwnKey(rc *reqCtx) (int, any, error) {
	ak := rc.r.PathValue("accessKey")
	u, err := a.selfUser(rc)
	if err != nil {
		return 0, nil, err
	}
	if err := a.authorizeSelfService(rc, actionDeleteOwnKey); err != nil {
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
	// Ownership is checked again atomically with the deletion.
	if err := a.d.IAM.DeleteUserAccessKey(rc.ctx, rc.id.User, ak); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}
