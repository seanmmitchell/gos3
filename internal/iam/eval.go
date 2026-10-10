package iam

import (
	"strconv"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// AdminResourceARN is the resource that admin:* actions are evaluated
// against (besides "*").
const AdminResourceARN = "arn:gos3:admin:::*"

// s3ServiceARN is the resource that service-level s3 actions (Bucket == "",
// e.g. s3:ListAllMyBuckets) are evaluated against besides "*".
const s3ServiceARN = s3ARNPrefix + "*"

// BucketARN returns the resource ARN of a bucket (arn:aws:s3:::bucket).
func BucketARN(bucket string) string { return s3ARNPrefix + bucket }

// ObjectARN returns the resource ARN of an object (arn:aws:s3:::bucket/key).
func ObjectARN(bucket, key string) string { return s3ARNPrefix + bucket + "/" + key }

// evalCtx holds the per-request state of one authorization decision.
type evalCtx struct {
	id       auth.Identity
	action   string
	isAdmin  bool
	bucket   string
	resource string // resource ARN; "" for service-level requests
	raw      map[string][]string
	lower    map[string][]string // lazily built, lower-cased keys + derived keys
	now      time.Time
}

func newEvalCtx(req *AuthzRequest, now time.Time) *evalCtx {
	c := &evalCtx{
		id:      req.Identity,
		action:  req.Action,
		isAdmin: hasPrefixFold(req.Action, "admin:"),
		bucket:  req.Bucket,
		raw:     req.Conditions,
		now:     now,
	}
	switch {
	case req.Bucket == "":
	case req.Key == "":
		c.resource = BucketARN(req.Bucket)
	default:
		c.resource = ObjectARN(req.Bucket, req.Key)
	}
	return c
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// lookup returns the values of a condition key (exact match first, then
// case-insensitive). present is false when the key is missing or empty.
func (c *evalCtx) lookup(key string) ([]string, bool) {
	if v, ok := c.raw[key]; ok && len(v) > 0 {
		return v, true
	}
	if c.lower == nil {
		c.buildLower()
	}
	v := c.lower[strings.ToLower(key)]
	return v, len(v) > 0
}

// buildLower indexes the request context case-insensitively and adds the
// keys gos3 can derive itself when the caller did not supply them.
func (c *evalCtx) buildLower() {
	c.lower = make(map[string][]string, len(c.raw)+5)
	for k, v := range c.raw {
		if len(v) == 0 {
			continue
		}
		lk := strings.ToLower(k)
		c.lower[lk] = append(c.lower[lk], v...)
	}
	setDefault := func(k string, v ...string) {
		if _, ok := c.lower[k]; !ok {
			c.lower[k] = v
		}
	}
	switch {
	case c.id.IsAnonymous:
		setDefault("aws:principaltype", "Anonymous")
	case c.id.IsRoot:
		setDefault("aws:principaltype", "Account")
		setDefault("aws:userid", auth.RootUser)
	default:
		setDefault("aws:principaltype", "User")
		if c.id.User != "" {
			setDefault("aws:username", c.id.User)
			setDefault("aws:userid", c.id.User)
		}
	}
	setDefault("aws:currenttime", c.now.UTC().Format(time.RFC3339))
	setDefault("aws:epochtime", strconv.FormatInt(c.now.Unix(), 10))
}

// lookupVar resolves a policy variable (single-valued keys only).
func (c *evalCtx) lookupVar(name string) (string, bool) {
	v, ok := c.lookup(name)
	if !ok || len(v) != 1 {
		return "", false
	}
	return v[0], true
}

// actionMatches reports whether the request action matches any pattern
// (case-insensitive, with wildcards).
func (c *evalCtx) actionMatches(patterns []string) bool {
	for _, p := range patterns {
		if globMatch(p, c.action, true, false) {
			return true
		}
	}
	return false
}

// resourceMatches reports whether the request resource matches any pattern.
// Service-level requests (Bucket == "") only match "*", plus
// "arn:aws:s3:::*" for s3 actions and "arn:gos3:admin:::*" for admin
// actions.
func (c *evalCtx) resourceMatches(patterns []string) bool {
	for _, p := range patterns {
		if c.resource == "" {
			if p == "*" || (c.isAdmin && p == AdminResourceARN) || (!c.isAdmin && p == s3ServiceARN) {
				return true
			}
			continue
		}
		if matchPattern(p, c.resource, false, c.lookupVar) {
			return true
		}
	}
	return false
}

// principalMatches reports whether p names the request identity. broad
// selects how account principals (arn:<p>:iam::<acct>:root or a 12-digit
// account ID) are read: gos3 has a single account, so in the broad reading
// they name every authenticated identity (as the account principal does on
// AWS), in the narrow one only the root identity. Callers pick whichever
// makes the statement grant less: broad for Deny statements' Principal
// and Allow statements' NotPrincipal, narrow for Allow statements'
// Principal (an Allow to the account delegates to identity policies, as
// on AWS) and Deny statements' NotPrincipal (an IAM user is only exempted
// by an entry naming that user).
func (c *evalCtx) principalMatches(p *Principal, broad bool) bool {
	if p == nil {
		return false
	}
	if p.Any {
		return true
	}
	for _, v := range p.AWS {
		if principalEntryMatches(v, c.id, broad) {
			return true
		}
	}
	return false
}

// principalEntryMatches matches one principal entry: "*", a user name, an
// access key ID, arn:<p>:iam::<any>:user/[path/]<name>, or an account
// principal arn:<p>:iam::<any>:root / 12-digit account ID (the root
// identity, or with broad every authenticated identity; see
// principalMatches).
func principalEntryMatches(v string, id auth.Identity, broad bool) bool {
	if v == "*" {
		return true
	}
	if id.IsAnonymous || v == "" {
		return false
	}
	if strings.HasPrefix(v, "arn:") {
		parts := strings.SplitN(v, ":", 6)
		if len(parts) != 6 || parts[2] != "iam" {
			return false
		}
		if parts[5] == "root" {
			return id.IsRoot || broad
		}
		name, ok := strings.CutPrefix(parts[5], "user/")
		if !ok || id.IsRoot {
			return false
		}
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		return name != "" && name == id.User
	}
	if isAccountID(v) && (id.IsRoot || broad) {
		return true
	}
	return v == id.User || v == id.AccessKey
}

func isAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// statementApplies reports whether st applies to the request. Principal
// elements are only checked for resource (bucket) policies.
func (c *evalCtx) statementApplies(st *Statement, resourcePolicy bool) bool {
	if resourcePolicy {
		deny := st.Effect == "Deny"
		switch {
		case st.Principal != nil:
			if !c.principalMatches(st.Principal, deny) {
				return false
			}
		case st.NotPrincipal != nil:
			if c.principalMatches(st.NotPrincipal, !deny) {
				return false
			}
		default:
			return false
		}
	}
	switch {
	case len(st.Action) > 0:
		if !c.actionMatches(st.Action) {
			return false
		}
	case len(st.NotAction) > 0:
		if c.actionMatches(st.NotAction) {
			return false
		}
	default:
		return false
	}
	switch {
	case len(st.Resource) > 0:
		if !c.resourceMatches(st.Resource) {
			return false
		}
	case len(st.NotResource) > 0:
		if c.resourceMatches(st.NotResource) {
			return false
		}
	default:
		return false
	}
	if len(st.Condition) > 0 && !c.evalConditions(st.Condition, st.Effect == "Deny") {
		return false
	}
	return true
}

// stmtLabel names a statement for decision reasons.
func stmtLabel(st *Statement, i int) string {
	if st.Sid != "" {
		return "Sid=" + st.Sid
	}
	return "#" + strconv.Itoa(i+1)
}

// Canned ACL grants (lower-cased actions).
var (
	aclReadActions = map[string]bool{
		"s3:getobject":          true,
		"s3:getobjectversion":   true,
		"s3:listbucket":         true,
		"s3:listbucketversions": true,
		"s3:getbucketlocation":  true,
	}
	// aclWriteActions are granted by bucket WRITE (public-read-write). As
	// on AWS, WRITE lets grantees create, overwrite and delete objects
	// (a delete marker in a versioned bucket) but not permanently delete
	// versions, which needs s3:DeleteObjectVersion from a policy.
	// Aborting or listing the parts of an upload (reserved on AWS for its
	// initiator and the bucket owner) is not granted either: gos3 does not
	// track initiators, and granting it to everyone would let anyone
	// abort or read other clients' uploads.
	aclWriteActions = map[string]bool{
		"s3:putobject":                  true,
		"s3:deleteobject":               true,
		"s3:listbucketmultipartuploads": true,
	}
)

// Canned ACL names that grant access beyond the owner.
const (
	ACLPublicRead        = "public-read"
	ACLPublicReadWrite   = "public-read-write"
	ACLAuthenticatedRead = "authenticated-read"
)

// cannedACLAllows reports whether a canned ACL grants action to id.
func cannedACLAllows(acl string, id auth.Identity, action string) bool {
	if acl == "" || action == "" {
		return false
	}
	a := strings.ToLower(action)
	switch strings.ToLower(acl) {
	case ACLPublicRead:
		return aclReadActions[a]
	case ACLPublicReadWrite:
		return aclReadActions[a] || aclWriteActions[a]
	case ACLAuthenticatedRead:
		return !id.IsAnonymous && aclReadActions[a]
	}
	return false
}

// IsPublicACL reports whether a canned ACL grants access to everyone
// (public-read, public-read-write) — used for "public" flags.
func IsPublicACL(acl string) bool {
	switch strings.ToLower(acl) {
	case ACLPublicRead, ACLPublicReadWrite:
		return true
	}
	return false
}

// authorize implements Manager.Authorize.
func (m *Manager) authorize(req AuthzRequest) Decision {
	id := req.Identity
	if id.IsRoot && !id.IsAnonymous {
		return Decision{Allowed: true, Reason: "root user"}
	}
	if req.Action == "" {
		return Decision{Reason: "missing action"}
	}

	var policies []*NamedPolicy
	if !id.IsAnonymous {
		st := m.snapshot()
		u := st.resolveUser(id)
		if u != nil {
			if u.Disabled {
				return Decision{Reason: "user " + u.Name + " is disabled"}
			}
			policies = make([]*NamedPolicy, 0, len(u.Policies))
			for _, name := range u.Policies {
				if p := st.policy(name); p != nil {
					policies = append(policies, p)
				}
			}
		}
	}

	c := newEvalCtx(&req, m.now())
	allowReason := ""
	for _, p := range policies {
		for i := range p.Document.Statement {
			st := &p.Document.Statement[i]
			if st.Effect == "Allow" && allowReason != "" {
				continue // already allowed; only Deny statements matter now
			}
			if (st.Effect != "Allow" && st.Effect != "Deny") || !c.statementApplies(st, false) {
				continue
			}
			if st.Effect == "Deny" {
				return Decision{ExplicitDeny: true, Reason: "explicit deny in policy " + p.Name + " statement " + stmtLabel(st, i)}
			}
			allowReason = "allowed by policy " + p.Name
		}
	}
	if bp := req.BucketPolicy; bp != nil {
		for i := range bp.Statement {
			st := &bp.Statement[i]
			if st.Effect == "Allow" && allowReason != "" {
				continue
			}
			if (st.Effect != "Allow" && st.Effect != "Deny") || !c.statementApplies(st, true) {
				continue
			}
			if st.Effect == "Deny" {
				return Decision{ExplicitDeny: true, Reason: "explicit deny in bucket policy statement " + stmtLabel(st, i)}
			}
			allowReason = "allowed by bucket policy statement " + stmtLabel(st, i)
		}
	}
	if allowReason != "" {
		return Decision{Allowed: true, Reason: allowReason}
	}
	if cannedACLAllows(req.ACL, id, req.Action) {
		return Decision{Allowed: true, Reason: "allowed by canned ACL " + strings.ToLower(req.ACL)}
	}
	return Decision{Reason: "no matching allow"}
}
