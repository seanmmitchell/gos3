package s3

// Bucket access configuration operations (owned by s3-bucketconfig):
// bucket ACL, object ownership controls, public access block and the
// object lock configuration.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// maxACLBody bounds AccessControlPolicy request bodies.
const maxACLBody = 64 << 10

// --- Bucket ACL ---

// handleGetBucketAcl implements GetBucketAcl: the bucket's canned ACL
// (storage.ConfigACL) rendered as an AccessControlPolicy owned by the
// bucket owner, whose FULL_CONTROL grant is always listed first. With
// ObjectOwnership BucketOwnerEnforced (ACLs disabled) only the owner grant
// is reported.
func (s *Server) handleGetBucketAcl(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	raw, err := s.bucketConfig(ctx, rc.bucket, storage.ConfigACL)
	if err != nil {
		return err
	}
	own, err := s.bucketConfig(ctx, rc.bucket, storage.ConfigOwnership)
	if err != nil {
		return err
	}
	canned := cannedACLFromConfig(raw)
	if aclsDisabledByOwnership(own) {
		canned = ""
	}
	return writeXML(w, http.StatusOK, aclPolicyForCanned(canned, ownerFor(bi.Owner)))
}

// handlePutBucketAcl implements PutBucketAcl with a canned ACL (x-amz-acl,
// no body) or an AccessControlPolicy body whose grants are equivalent to
// one of the bucket canned ACLs private, public-read, public-read-write,
// authenticated-read or log-delivery-write (anything else is
// NotImplemented: gos3 stores canned ACLs only). x-amz-grant-* headers are
// NotImplemented. With ObjectOwnership BucketOwnerEnforced only ACLs that
// grant nothing beyond the owner are accepted
// (AccessControlListNotSupported); with BlockPublicAcls public ACLs are
// rejected (AccessDenied).
func (s *Server) handlePutBucketAcl(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	h := r.Header
	if err := checkGrantHeaders(h); err != nil {
		return err
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	data, err := s.readConfigBody(r, rc, maxACLBody)
	if err != nil {
		return err
	}
	var canned string
	if v := h.Get("X-Amz-Acl"); v != "" {
		if canned, err = validateCannedACL(v, true); err != nil {
			return err
		}
		if len(bytes.TrimSpace(data)) > 0 {
			return s3err.ErrUnexpectedContent.WithMessage("This request does not support content when x-amz-acl is specified.")
		}
	} else {
		var p AccessControlPolicy
		if err := decodeXML(data, &p); err != nil {
			if errors.Is(err, s3err.ErrMalformedXML) {
				return s3err.ErrMalformedACL
			}
			return err
		}
		if canned, err = bucketCannedFromGrants(&p, ownerFor(bi.Owner).ID, ownerFor(rc.userName()).ID); err != nil {
			return err
		}
		if err := s.authorizeOpWith(ctx, r, rc, aclBodyConditions(canned, &p)); err != nil {
			return err
		}
	}

	unlock := lockBucketState(rc.bucket)
	defer unlock()
	own, err := s.bucketConfig(ctx, rc.bucket, storage.ConfigOwnership)
	if err != nil {
		return err
	}
	if aclsDisabledByOwnership(own) && bucketACLGrantsOthers(canned) {
		return s3err.ErrAccessControlListNotSupported
	}
	if isPublicCannedACL(canned) {
		pab, err := s.publicAccessBlock(ctx, rc.bucket)
		if err != nil {
			return err
		}
		if pab.BlockPublicAcls {
			return s3err.ErrAccessDenied
		}
	}
	if canned == "" {
		err = s.removeBucketConfig(ctx, rc, storage.ConfigACL)
	} else {
		err = s.storeBucketConfig(ctx, rc, storage.ConfigACL, []byte(canned))
	}
	if err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// bucketACLGrantsOthers reports whether a stored bucket canned ACL grants
// anything to someone other than the bucket owner.
func bucketACLGrantsOthers(canned string) bool {
	return isPublicCannedACL(canned) || strings.EqualFold(canned, aclLogDeliveryWrite)
}

// aclGroupGrant is a (group URI, permission) pair of an ACL.
type aclGroupGrant struct{ uri, perm string }

// bucketCannedGrantSets lists the non-owner grants of every bucket canned
// ACL that an AccessControlPolicy can be mapped to.
var bucketCannedGrantSets = []struct {
	canned string
	grants []aclGroupGrant
}{
	{aclPublicRead, []aclGroupGrant{{groupAllUsers, "READ"}}},
	{aclPublicReadWrite, []aclGroupGrant{{groupAllUsers, "READ"}, {groupAllUsers, "WRITE"}}},
	{aclAuthenticatedRead, []aclGroupGrant{{groupAuthenticatedUsers, "READ"}}},
	{aclLogDeliveryWrite, []aclGroupGrant{{groupLogDelivery, "WRITE"}, {groupLogDelivery, "READ_ACP"}}},
}

// bucketCannedFromGrants maps the grants of an AccessControlPolicy to the
// bucket canned ACL with exactly the same grants ("" = private). Grants to
// the owner (any of ownerIDs) are implied and accepted with any
// permission. Unknown permissions are MalformedACLError; grants to other
// users, e-mail grantees or combinations without a canned equivalent are
// NotImplemented.
func bucketCannedFromGrants(p *AccessControlPolicy, ownerIDs ...string) (string, error) {
	notImpl := s3err.ErrNotImplemented.WithMessage("Only ACLs equivalent to the private, public-read, public-read-write, authenticated-read or log-delivery-write canned ACLs are supported.")
	got := map[aclGroupGrant]bool{}
	for _, g := range p.Grants {
		perm := strings.ToUpper(strings.TrimSpace(g.Permission))
		switch perm {
		case "FULL_CONTROL", "READ", "WRITE", "READ_ACP", "WRITE_ACP":
		default:
			return "", s3err.ErrMalformedACL
		}
		gt := g.Grantee
		typ := gt.Type
		if typ == "" {
			switch {
			case gt.URI != "":
				typ = "Group"
			case gt.ID != "":
				typ = "CanonicalUser"
			case gt.EmailAddress != "":
				typ = "AmazonCustomerByEmail"
			}
		}
		switch typ {
		case "CanonicalUser":
			id := strings.TrimSpace(gt.ID)
			owner := false
			for _, o := range ownerIDs {
				owner = owner || id == o
			}
			if !owner {
				return "", notImpl
			}
		case "Group":
			uri := strings.TrimSpace(gt.URI)
			switch uri {
			case groupAllUsers, groupAuthenticatedUsers, groupLogDelivery:
			default:
				return "", s3err.ErrInvalidArgument.WithMessage("Invalid group uri").With("ArgumentName", "uri").With("ArgumentValue", uri)
			}
			got[aclGroupGrant{uri, perm}] = true
		case "AmazonCustomerByEmail":
			return "", notImpl
		default:
			return "", s3err.ErrMalformedACL
		}
	}
	if len(got) == 0 {
		return "", nil
	}
	for _, set := range bucketCannedGrantSets {
		if len(set.grants) != len(got) {
			continue
		}
		match := true
		for _, g := range set.grants {
			match = match && got[g]
		}
		if match {
			return set.canned, nil
		}
	}
	return "", notImpl
}

// --- Ownership controls ---

// Object ownership settings.
const (
	ownershipBucketOwnerEnforced  = "BucketOwnerEnforced"
	ownershipBucketOwnerPreferred = "BucketOwnerPreferred"
	ownershipObjectWriter         = "ObjectWriter"
)

// ownershipControls is the <OwnershipControls> document
// (storage.ConfigOwnership).
type ownershipControls struct {
	XMLName xml.Name        `xml:"OwnershipControls"`
	XMLNS   string          `xml:"xmlns,attr,omitempty"`
	Rules   []ownershipRule `xml:"Rule"`
}

// ownershipRule is one <Rule> of OwnershipControls.
type ownershipRule struct {
	ObjectOwnership string `xml:"ObjectOwnership"`
}

// handleGetBucketOwnershipControls implements GetBucketOwnershipControls
// (OwnershipControlsNotFoundError when unset).
func (s *Server) handleGetBucketOwnershipControls(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigOwnership, s3err.ErrOwnershipControlsNotFound)
	if err != nil {
		return err
	}
	var oc ownershipControls
	if err := s.decodeStoredConfig(ctx, rc, storage.ConfigOwnership, raw, &oc); err != nil {
		return err
	}
	oc.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, oc)
}

// handlePutBucketOwnershipControls implements PutBucketOwnershipControls:
// exactly one rule with ObjectOwnership BucketOwnerEnforced,
// BucketOwnerPreferred or ObjectWriter (MalformedXML otherwise).
// BucketOwnerEnforced is refused while the bucket ACL grants access to
// others (InvalidBucketAclWithObjectOwnership).
func (s *Server) handlePutBucketOwnershipControls(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var oc ownershipControls
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &oc); err != nil {
		return err
	}
	if len(oc.Rules) != 1 {
		return s3err.ErrMalformedXML
	}
	v := strings.TrimSpace(oc.Rules[0].ObjectOwnership)
	switch v {
	case ownershipBucketOwnerEnforced, ownershipBucketOwnerPreferred, ownershipObjectWriter:
	default:
		return s3err.ErrMalformedXML
	}
	oc.Rules[0].ObjectOwnership = v

	unlock := lockBucketState(rc.bucket)
	defer unlock()
	if v == ownershipBucketOwnerEnforced {
		raw, err := s.bucketConfig(ctx, rc.bucket, storage.ConfigACL)
		if err != nil {
			return err
		}
		if bucketACLGrantsOthers(cannedACLFromConfig(raw)) {
			return s3err.ErrInvalidBucketAclWithObjectOwnership
		}
	}
	oc.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigOwnership, oc); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeleteBucketOwnershipControls implements
// DeleteBucketOwnershipControls (204).
func (s *Server) handleDeleteBucketOwnershipControls(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigOwnership)
}

// --- Public access block ---

// handleGetPublicAccessBlock implements GetPublicAccessBlock
// (NoSuchPublicAccessBlockConfiguration when unset).
func (s *Server) handleGetPublicAccessBlock(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigPublicAccessBlock, s3err.ErrNoSuchPublicAccessBlockConfiguration)
	if err != nil {
		return err
	}
	var c PublicAccessBlockConfiguration
	if err := s.decodeStoredConfig(ctx, rc, storage.ConfigPublicAccessBlock, raw, &c); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutPublicAccessBlock implements PutPublicAccessBlock. Omitted
// settings are false. The settings apply immediately to authorization
// (IgnorePublicAcls, RestrictPublicBuckets) and to later ACL and policy
// changes (BlockPublicAcls, BlockPublicPolicy); like AWS, they do not
// change already stored ACLs or policies.
func (s *Server) handlePutPublicAccessBlock(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c PublicAccessBlockConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	unlock := lockBucketState(rc.bucket)
	defer unlock()
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigPublicAccessBlock, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeletePublicAccessBlock implements DeletePublicAccessBlock (204).
func (s *Server) handleDeletePublicAccessBlock(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	unlock := lockBucketState(rc.bucket)
	defer unlock()
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigPublicAccessBlock)
}

// --- Object lock configuration ---

// Object lock default retention limits (as AWS).
const (
	maxDefaultRetentionDays  = 36500
	maxDefaultRetentionYears = 100
)

// objectLockConfiguration is the <ObjectLockConfiguration> document of
// Get/PutObjectLockConfiguration (storage.ConfigObjectLock).
type objectLockConfiguration struct {
	XMLName           xml.Name        `xml:"ObjectLockConfiguration"`
	XMLNS             string          `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *objectLockRule `xml:"Rule,omitempty"`
}

// objectLockRule is the <Rule> of an ObjectLockConfiguration.
type objectLockRule struct {
	DefaultRetention *objectLockDefaultRetention `xml:"DefaultRetention"`
}

// objectLockDefaultRetention is <DefaultRetention>.
type objectLockDefaultRetention struct {
	Mode  string `xml:"Mode"`
	Days  *int   `xml:"Days,omitempty"`
	Years *int   `xml:"Years,omitempty"`
}

// validate checks the configuration: ObjectLockEnabled must be "Enabled";
// a Rule needs a DefaultRetention with Mode GOVERNANCE or COMPLIANCE and
// exactly one of Days or Years (MalformedXML), which must be positive and
// at most 36500 days / 100 years (InvalidRetentionPeriod).
func (c *objectLockConfiguration) validate() error {
	c.ObjectLockEnabled = strings.TrimSpace(c.ObjectLockEnabled)
	if c.ObjectLockEnabled != "Enabled" {
		return s3err.ErrMalformedXML
	}
	if c.Rule == nil {
		return nil
	}
	dr := c.Rule.DefaultRetention
	if dr == nil {
		return s3err.ErrMalformedXML
	}
	dr.Mode = strings.TrimSpace(dr.Mode)
	if dr.Mode != "GOVERNANCE" && dr.Mode != "COMPLIANCE" {
		return s3err.ErrMalformedXML
	}
	switch {
	case (dr.Days == nil) == (dr.Years == nil):
		return s3err.ErrMalformedXML
	case dr.Days != nil && *dr.Days <= 0, dr.Years != nil && *dr.Years <= 0:
		return s3err.ErrInvalidRetentionPeriod.WithMessage("Default retention period must be a positive integer value.")
	case dr.Days != nil && *dr.Days > maxDefaultRetentionDays, dr.Years != nil && *dr.Years > maxDefaultRetentionYears:
		return s3err.ErrInvalidRetentionPeriod.WithMessage("Default retention period is too large.")
	}
	return nil
}

// handleGetObjectLockConfiguration implements GetObjectLockConfiguration:
// ObjectLockEnabled plus the default retention rule if one is configured.
// ObjectLockConfigurationNotFoundError when object lock is not enabled on
// the bucket.
func (s *Server) handleGetObjectLockConfiguration(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	if !bi.ObjectLockEnabled {
		return s3err.ErrObjectLockConfigurationNotFound.With("BucketName", rc.bucket)
	}
	raw, err := s.bucketConfig(ctx, rc.bucket, storage.ConfigObjectLock)
	if err != nil {
		return err
	}
	var c objectLockConfiguration
	if len(raw) > 0 {
		if err := s.decodeStoredConfig(ctx, rc, storage.ConfigObjectLock, raw, &c); err != nil {
			return err
		}
	}
	c.XMLNS, c.ObjectLockEnabled = s3Namespace, "Enabled"
	return writeXML(w, http.StatusOK, c)
}

// handlePutObjectLockConfiguration implements PutObjectLockConfiguration
// (see objectLockConfiguration.validate). Enabling object lock on an
// existing bucket requires versioning to be Enabled (InvalidBucketState);
// object lock can never be disabled again. A configuration without a Rule
// removes the default retention. The default retention applies to objects
// created afterwards.
func (s *Server) handlePutObjectLockConfiguration(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c objectLockConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}

	unlock := lockBucketState(rc.bucket)
	defer unlock()
	bi, err := s.lookupBucket(ctx, rc.bucket)
	if err != nil {
		return err
	}
	if !bi.ObjectLockEnabled {
		if bi.Versioning != storage.VersioningEnabled {
			return s3err.ErrInvalidBucketState.WithMessage("Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration")
		}
		if err := s.backend.SetBucketObjectLock(ctx, rc.bucket, true); err != nil {
			return bucketError(err, rc.bucket)
		}
		rc.forgetBucket()
	}
	c.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigObjectLock, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}
