package s3

// Bucket configuration operations that gos3 validates and stores so that
// clients (SDKs, Terraform, consoles) can round-trip them, but whose
// features are not executed (owned by s3-bucketconfig):
//
//   - website: no website endpoint serves index/error documents or
//     redirects;
//   - notification: no events are delivered;
//   - logging: no server access log objects are written;
//   - replication: nothing is replicated;
//   - accelerate: there is no accelerate endpoint;
//   - request payment: only BucketOwner is supported.

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- Website ---

// Website configuration limits.
const (
	maxWebsiteRoutingRules = 50
	maxWebsiteFieldLength  = 1024
)

// websiteConfiguration is the <WebsiteConfiguration> document
// (storage.ConfigWebsite).
type websiteConfiguration struct {
	XMLName               xml.Name             `xml:"WebsiteConfiguration"`
	XMLNS                 string               `xml:"xmlns,attr,omitempty"`
	RedirectAllRequestsTo *websiteRedirectAll  `xml:"RedirectAllRequestsTo,omitempty"`
	IndexDocument         *websiteIndexDoc     `xml:"IndexDocument,omitempty"`
	ErrorDocument         *websiteErrorDoc     `xml:"ErrorDocument,omitempty"`
	RoutingRules          *websiteRoutingRules `xml:"RoutingRules,omitempty"`
}

// websiteRedirectAll is <RedirectAllRequestsTo>.
type websiteRedirectAll struct {
	HostName string `xml:"HostName"`
	Protocol string `xml:"Protocol,omitempty"`
}

// websiteIndexDoc is <IndexDocument>.
type websiteIndexDoc struct {
	Suffix string `xml:"Suffix"`
}

// websiteErrorDoc is <ErrorDocument>.
type websiteErrorDoc struct {
	Key string `xml:"Key"`
}

// websiteRoutingRules is <RoutingRules>.
type websiteRoutingRules struct {
	Rules []websiteRoutingRule `xml:"RoutingRule"`
}

// websiteRoutingRule is one <RoutingRule>.
type websiteRoutingRule struct {
	Condition *websiteCondition `xml:"Condition,omitempty"`
	Redirect  *websiteRedirect  `xml:"Redirect"`
}

// websiteCondition is the <Condition> of a routing rule.
type websiteCondition struct {
	HTTPErrorCodeReturnedEquals string `xml:"HttpErrorCodeReturnedEquals,omitempty"`
	KeyPrefixEquals             string `xml:"KeyPrefixEquals,omitempty"`
}

// websiteRedirect is the <Redirect> of a routing rule.
type websiteRedirect struct {
	HostName             string `xml:"HostName,omitempty"`
	HTTPRedirectCode     string `xml:"HttpRedirectCode,omitempty"`
	Protocol             string `xml:"Protocol,omitempty"`
	ReplaceKeyPrefixWith string `xml:"ReplaceKeyPrefixWith,omitempty"`
	ReplaceKeyWith       string `xml:"ReplaceKeyWith,omitempty"`
}

// websiteArgError is an InvalidArgument error of a website configuration.
func websiteArgError(msg string) error {
	return s3err.ErrInvalidArgument.WithMessage("%s", msg)
}

// validWebsiteProtocol reports whether p is an allowed redirect protocol.
func validWebsiteProtocol(p string) bool { return p == "" || p == "http" || p == "https" }

// validate checks the configuration as AWS does: either
// RedirectAllRequestsTo (with a HostName, alone) or an IndexDocument with a
// non-empty Suffix without '/'; an optional ErrorDocument Key; at most 50
// routing rules, each with a Redirect, a non-empty Condition when present,
// a 4xx/5xx HttpErrorCodeReturnedEquals, a 3xx HttpRedirectCode and not
// both ReplaceKeyPrefixWith and ReplaceKeyWith.
func (c *websiteConfiguration) validate() error {
	if ra := c.RedirectAllRequestsTo; ra != nil {
		if c.IndexDocument != nil || c.ErrorDocument != nil || c.RoutingRules != nil {
			return s3err.ErrInvalidRequest.WithMessage("RedirectAllRequestsTo cannot be provided in conjunction with other Routing Rules.")
		}
		ra.HostName = strings.TrimSpace(ra.HostName)
		if ra.HostName == "" || len(ra.HostName) > maxWebsiteFieldLength {
			return websiteArgError("The HostName in RedirectAllRequestsTo is not valid.")
		}
		if !validWebsiteProtocol(ra.Protocol) {
			return websiteArgError("Invalid protocol, protocol can be http or https. If not defined the protocol will be selected automatically.")
		}
		return nil
	}
	if c.IndexDocument == nil {
		return websiteArgError("A value for IndexDocument Suffix must be provided if RedirectAllRequestsTo is empty")
	}
	if sfx := c.IndexDocument.Suffix; sfx == "" || strings.Contains(sfx, "/") || len(sfx) > maxWebsiteFieldLength {
		return websiteArgError("The IndexDocument Suffix is not well formed")
	}
	if ed := c.ErrorDocument; ed != nil && (ed.Key == "" || len(ed.Key) > maxWebsiteFieldLength) {
		return websiteArgError("The ErrorDocument Key is not well formed")
	}
	if c.RoutingRules == nil {
		return nil
	}
	rules := c.RoutingRules.Rules
	if len(rules) == 0 {
		return s3err.ErrMalformedXML
	}
	if len(rules) > maxWebsiteRoutingRules {
		return websiteArgError("The number of routing rules must not exceed 50.")
	}
	for i := range rules {
		if err := rules[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

// validate checks one routing rule (see websiteConfiguration.validate).
func (rr *websiteRoutingRule) validate() error {
	if cond := rr.Condition; cond != nil {
		if cond.HTTPErrorCodeReturnedEquals == "" && cond.KeyPrefixEquals == "" {
			return websiteArgError("Condition cannot be empty. To redirect all requests without a condition, the condition element shouldn't be present.")
		}
		if v := cond.HTTPErrorCodeReturnedEquals; v != "" {
			if n, err := strconv.Atoi(v); err != nil || n < 400 || n > 599 {
				return websiteArgError("The provided HTTP error code (" + v + ") is not valid. Valid codes are 4XX or 5XX.")
			}
		}
		if len(cond.KeyPrefixEquals) > maxWebsiteFieldLength {
			return websiteArgError("The KeyPrefixEquals is too long.")
		}
	}
	rd := rr.Redirect
	if rd == nil {
		return s3err.ErrMalformedXML
	}
	if *rd == (websiteRedirect{}) {
		return websiteArgError("Redirect must contain at least one of the following: HostName, HttpRedirectCode, Protocol, ReplaceKeyPrefixWith, ReplaceKeyWith.")
	}
	if rd.ReplaceKeyPrefixWith != "" && rd.ReplaceKeyWith != "" {
		return websiteArgError("You can only define ReplaceKeyPrefix or ReplaceKey but not both.")
	}
	if !validWebsiteProtocol(rd.Protocol) {
		return websiteArgError("Invalid protocol, protocol can be http or https. If not defined the protocol will be selected automatically.")
	}
	if v := rd.HTTPRedirectCode; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 300 || n > 399 {
			return websiteArgError("The provided HTTP redirect code (" + v + ") is not valid. Valid codes are 3XX except 300.")
		}
	}
	for _, f := range []string{rd.HostName, rd.ReplaceKeyPrefixWith, rd.ReplaceKeyWith} {
		if len(f) > maxWebsiteFieldLength {
			return websiteArgError("A Redirect field is too long.")
		}
	}
	return nil
}

// handleGetBucketWebsite implements GetBucketWebsite
// (NoSuchWebsiteConfiguration when unset).
func (s *Server) handleGetBucketWebsite(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigWebsite, s3err.ErrNoSuchWebsiteConfiguration)
	if err != nil {
		return err
	}
	var c websiteConfiguration
	if err := s.decodeStoredConfig(ctx, rc, storage.ConfigWebsite, raw, &c); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutBucketWebsite implements PutBucketWebsite (see
// websiteConfiguration.validate). The configuration is stored only; gos3
// has no website endpoint.
func (s *Server) handlePutBucketWebsite(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c websiteConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigWebsite, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeleteBucketWebsite implements DeleteBucketWebsite (204).
func (s *Server) handleDeleteBucketWebsite(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigWebsite)
}

// --- Notification ---

// maxNotificationConfigs bounds the number of destination configurations.
const maxNotificationConfigs = 1000

// notificationConfiguration is the <NotificationConfiguration> document
// (storage.ConfigNotification).
type notificationConfiguration struct {
	XMLName     xml.Name             `xml:"NotificationConfiguration"`
	XMLNS       string               `xml:"xmlns,attr,omitempty"`
	Topics      []notificationTarget `xml:"TopicConfiguration"`
	Queues      []notificationTarget `xml:"QueueConfiguration"`
	Functions   []notificationTarget `xml:"CloudFunctionConfiguration"`
	EventBridge *struct{}            `xml:"EventBridgeConfiguration"`
}

// notificationTarget is a Topic/Queue/CloudFunctionConfiguration; exactly
// the destination element matching its kind is set.
type notificationTarget struct {
	ID            string              `xml:"Id,omitempty"`
	Topic         string              `xml:"Topic,omitempty"`
	Queue         string              `xml:"Queue,omitempty"`
	CloudFunction string              `xml:"CloudFunction,omitempty"`
	Events        []string            `xml:"Event"`
	Filter        *notificationFilter `xml:"Filter,omitempty"`
}

// notificationFilter is <Filter><S3Key><FilterRule>...</FilterRule></S3Key></Filter>.
type notificationFilter struct {
	Rules []notificationFilterRule `xml:"S3Key>FilterRule"`
}

// notificationFilterRule is one key name filter rule.
type notificationFilterRule struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

// notificationEvents are the event types S3 accepts in notification
// configurations.
var notificationEvents = map[string]bool{
	"s3:TestEvent":                                     true,
	"s3:ObjectCreated:*":                               true,
	"s3:ObjectCreated:Put":                             true,
	"s3:ObjectCreated:Post":                            true,
	"s3:ObjectCreated:Copy":                            true,
	"s3:ObjectCreated:CompleteMultipartUpload":         true,
	"s3:ObjectRemoved:*":                               true,
	"s3:ObjectRemoved:Delete":                          true,
	"s3:ObjectRemoved:DeleteMarkerCreated":             true,
	"s3:ObjectRestore:*":                               true,
	"s3:ObjectRestore:Post":                            true,
	"s3:ObjectRestore:Completed":                       true,
	"s3:ObjectRestore:Delete":                          true,
	"s3:ReducedRedundancyLostObject":                   true,
	"s3:Replication:*":                                 true,
	"s3:Replication:OperationFailedReplication":        true,
	"s3:Replication:OperationNotTracked":               true,
	"s3:Replication:OperationMissedThreshold":          true,
	"s3:Replication:OperationReplicatedAfterThreshold": true,
	"s3:LifecycleExpiration:*":                         true,
	"s3:LifecycleExpiration:Delete":                    true,
	"s3:LifecycleExpiration:DeleteMarkerCreated":       true,
	"s3:LifecycleTransition":                           true,
	"s3:IntelligentTiering":                            true,
	"s3:ObjectTagging:*":                               true,
	"s3:ObjectTagging:Put":                             true,
	"s3:ObjectTagging:Delete":                          true,
	"s3:ObjectAcl:Put":                                 true,
	"s3:ObjectCreated:PutRetention":                    true,
	"s3:ObjectCreated:PutLegalHold":                    true,
}

// empty reports whether the configuration has no destinations.
func (c *notificationConfiguration) empty() bool {
	return len(c.Topics)+len(c.Queues)+len(c.Functions) == 0 && c.EventBridge == nil
}

// validate checks every destination: exactly its own ARN element
// (non-empty), at least one supported event, unique IDs of at most 255
// characters and key filters with at most one prefix and one suffix rule
// (InvalidArgument).
func (c *notificationConfiguration) validate() error {
	if len(c.Topics)+len(c.Queues)+len(c.Functions) > maxNotificationConfigs {
		return s3err.ErrInvalidArgument.WithMessage("Too many notification configurations.")
	}
	ids := map[string]bool{}
	check := func(list []notificationTarget, kind string) error {
		for i := range list {
			t := &list[i]
			arns := map[string]string{"Topic": t.Topic, "Queue": t.Queue, "CloudFunction": t.CloudFunction}
			for k, v := range arns {
				if (k == kind) != (strings.TrimSpace(v) != "") {
					return s3err.ErrInvalidArgument.WithMessage("A %sConfiguration must specify exactly one %s ARN.", kind, kind).
						With("ArgumentName", kind)
				}
			}
			if len(t.ID) > 255 {
				return s3err.ErrInvalidArgument.WithMessage("The notification configuration ID is too long.").With("ArgumentName", "Id")
			}
			if t.ID != "" {
				if ids[t.ID] {
					return s3err.ErrInvalidArgument.WithMessage("Configurations must have unique IDs.").With("ArgumentName", "Id").With("ArgumentValue", t.ID)
				}
				ids[t.ID] = true
			}
			if len(t.Events) == 0 {
				return s3err.ErrInvalidArgument.WithMessage("At least one event must be specified.").With("ArgumentName", "Event")
			}
			for j, ev := range t.Events {
				ev = strings.TrimSpace(ev)
				if !notificationEvents[ev] {
					return s3err.ErrInvalidArgument.WithMessage("The event is not supported for notifications").
						With("ArgumentName", "Event").With("ArgumentValue", ev)
				}
				t.Events[j] = ev
			}
			if t.Filter != nil {
				seen := map[string]bool{}
				for j := range t.Filter.Rules {
					fr := &t.Filter.Rules[j]
					name := strings.ToLower(strings.TrimSpace(fr.Name))
					if name != "prefix" && name != "suffix" {
						return s3err.ErrInvalidArgument.WithMessage("filter rule name must be either prefix or suffix").
							With("ArgumentName", "FilterRule.Name").With("ArgumentValue", fr.Name)
					}
					if seen[name] {
						return s3err.ErrInvalidArgument.WithMessage("Cannot specify more than one %s rule in a filter.", name).
							With("ArgumentName", "FilterRule.Name").With("ArgumentValue", fr.Name)
					}
					if len(fr.Value) > maxKeyLength {
						return s3err.ErrInvalidArgument.WithMessage("The filter rule value is too long.").With("ArgumentName", "FilterRule.Value")
					}
					seen[name] = true
					fr.Name = name
				}
			}
		}
		return nil
	}
	if err := check(c.Topics, "Topic"); err != nil {
		return err
	}
	if err := check(c.Queues, "Queue"); err != nil {
		return err
	}
	return check(c.Functions, "CloudFunction")
}

// handleGetBucketNotification implements
// GetBucketNotificationConfiguration: the stored configuration, or an
// empty NotificationConfiguration when none is set.
func (s *Server) handleGetBucketNotification(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.loadBucketConfig(ctx, rc, storage.ConfigNotification)
	if err != nil {
		return err
	}
	var c notificationConfiguration
	if raw != nil {
		if err := s.decodeStoredConfig(ctx, rc, storage.ConfigNotification, raw, &c); err != nil {
			return err
		}
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutBucketNotification implements
// PutBucketNotificationConfiguration (see
// notificationConfiguration.validate). An empty configuration removes it.
// Destinations are not contacted and no events are delivered.
func (s *Server) handlePutBucketNotification(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c notificationConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	if c.empty() {
		if err := s.removeBucketConfig(ctx, rc, storage.ConfigNotification); err != nil {
			return err
		}
	} else {
		c.XMLNS = s3Namespace
		if err := s.storeBucketConfig(ctx, rc, storage.ConfigNotification, c); err != nil {
			return err
		}
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- Logging ---

// bucketLoggingStatus is the <BucketLoggingStatus> document
// (storage.ConfigLogging).
type bucketLoggingStatus struct {
	XMLName        xml.Name        `xml:"BucketLoggingStatus"`
	XMLNS          string          `xml:"xmlns,attr,omitempty"`
	LoggingEnabled *loggingEnabled `xml:"LoggingEnabled,omitempty"`
}

// loggingEnabled is <LoggingEnabled>.
type loggingEnabled struct {
	TargetBucket          string               `xml:"TargetBucket"`
	TargetPrefix          string               `xml:"TargetPrefix"`
	TargetGrants          *loggingTargetGrants `xml:"TargetGrants,omitempty"`
	TargetObjectKeyFormat *loggingKeyFormat    `xml:"TargetObjectKeyFormat,omitempty"`
}

// loggingTargetGrants is <TargetGrants>.
type loggingTargetGrants struct {
	Grants []Grant `xml:"Grant"`
}

// loggingKeyFormat is <TargetObjectKeyFormat>.
type loggingKeyFormat struct {
	SimplePrefix      *struct{}                 `xml:"SimplePrefix,omitempty"`
	PartitionedPrefix *loggingPartitionedPrefix `xml:"PartitionedPrefix,omitempty"`
}

// loggingPartitionedPrefix is <PartitionedPrefix>.
type loggingPartitionedPrefix struct {
	PartitionDateSource string `xml:"PartitionDateSource,omitempty"`
}

// maxLoggingGrants bounds TargetGrants.
const maxLoggingGrants = 100

// validate checks LoggingEnabled: a TargetBucket (MalformedXML when
// missing), a TargetPrefix of at most 1024 bytes, grants with
// FULL_CONTROL/READ/WRITE permissions and a grantee, and at most one key
// format (MalformedXML).
func (l *loggingEnabled) validate() error {
	l.TargetBucket = strings.TrimSpace(l.TargetBucket)
	if l.TargetBucket == "" {
		return s3err.ErrMalformedXML
	}
	if len(l.TargetPrefix) > maxKeyLength {
		return s3err.ErrInvalidArgument.WithMessage("The target prefix is too long.").With("ArgumentName", "TargetPrefix")
	}
	if tg := l.TargetGrants; tg != nil {
		if len(tg.Grants) > maxLoggingGrants {
			return s3err.ErrInvalidArgument.WithMessage("Too many target grants.").With("ArgumentName", "TargetGrants")
		}
		for i := range tg.Grants {
			g := &tg.Grants[i]
			g.Permission = strings.ToUpper(strings.TrimSpace(g.Permission))
			switch g.Permission {
			case "FULL_CONTROL", "READ", "WRITE":
			default:
				return s3err.ErrMalformedXML
			}
			gt := &g.Grantee
			if gt.Type == "" {
				switch {
				case gt.URI != "":
					gt.Type = "Group"
				case gt.ID != "":
					gt.Type = "CanonicalUser"
				case gt.EmailAddress != "":
					gt.Type = "AmazonCustomerByEmail"
				}
			}
			switch gt.Type {
			case "Group", "CanonicalUser", "AmazonCustomerByEmail":
			default:
				return s3err.ErrMalformedXML
			}
		}
	}
	if kf := l.TargetObjectKeyFormat; kf != nil {
		if (kf.SimplePrefix == nil) == (kf.PartitionedPrefix == nil) {
			return s3err.ErrMalformedXML
		}
		if pp := kf.PartitionedPrefix; pp != nil {
			switch pp.PartitionDateSource {
			case "", "EventTime", "DeliveryTime":
			default:
				return s3err.ErrMalformedXML
			}
		}
	}
	return nil
}

// handleGetBucketLogging implements GetBucketLogging: the stored status, or
// an empty BucketLoggingStatus (logging disabled) when none is set.
func (s *Server) handleGetBucketLogging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.loadBucketConfig(ctx, rc, storage.ConfigLogging)
	if err != nil {
		return err
	}
	var c bucketLoggingStatus
	if raw != nil {
		if err := s.decodeStoredConfig(ctx, rc, storage.ConfigLogging, raw, &c); err != nil {
			return err
		}
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutBucketLogging implements PutBucketLogging (see
// loggingEnabled.validate). The target bucket must exist
// (InvalidTargetBucketForLogging). A status without LoggingEnabled
// disables logging. No access log objects are written.
func (s *Server) handlePutBucketLogging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c bucketLoggingStatus
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	if c.LoggingEnabled == nil {
		if err := s.removeBucketConfig(ctx, rc, storage.ConfigLogging); err != nil {
			return err
		}
		writeEmpty(w, http.StatusOK)
		return nil
	}
	if err := c.LoggingEnabled.validate(); err != nil {
		return err
	}
	if _, err := s.lookupBucket(ctx, c.LoggingEnabled.TargetBucket); err != nil {
		if s3err.As(err).Code == s3err.ErrNoSuchBucket.Code {
			return s3err.ErrInvalidTargetBucketForLogging.With("TargetBucket", c.LoggingEnabled.TargetBucket)
		}
		return err
	}
	c.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigLogging, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- Replication ---

// Replication configuration limits.
const (
	maxReplicationRules  = 1000
	maxReplicationRuleID = 255
	maxReplicationRole   = 2048
)

// replicationConfiguration is the <ReplicationConfiguration> document
// (storage.ConfigReplication).
type replicationConfiguration struct {
	XMLName xml.Name          `xml:"ReplicationConfiguration"`
	XMLNS   string            `xml:"xmlns,attr,omitempty"`
	Role    string            `xml:"Role"`
	Rules   []replicationRule `xml:"Rule"`
}

// replicationRule is one replication <Rule>.
type replicationRule struct {
	ID                        string                   `xml:"ID,omitempty"`
	Priority                  *int                     `xml:"Priority,omitempty"`
	Prefix                    *string                  `xml:"Prefix,omitempty"`
	Filter                    *replicationFilter       `xml:"Filter,omitempty"`
	Status                    string                   `xml:"Status"`
	SourceSelectionCriteria   *replicationSourceSelect `xml:"SourceSelectionCriteria,omitempty"`
	ExistingObjectReplication *replicationStatus       `xml:"ExistingObjectReplication,omitempty"`
	Destination               *replicationDestination  `xml:"Destination"`
	DeleteMarkerReplication   *replicationStatus       `xml:"DeleteMarkerReplication,omitempty"`
}

// replicationStatus is an element holding just a <Status>.
type replicationStatus struct {
	Status string `xml:"Status"`
}

// replicationFilter is the <Filter> of a replication rule.
type replicationFilter struct {
	Prefix *string         `xml:"Prefix,omitempty"`
	Tag    *Tag            `xml:"Tag,omitempty"`
	And    *replicationAnd `xml:"And,omitempty"`
}

// replicationAnd is the <And> of a replication filter.
type replicationAnd struct {
	Prefix *string `xml:"Prefix,omitempty"`
	Tags   []Tag   `xml:"Tag"`
}

// replicationSourceSelect is <SourceSelectionCriteria>.
type replicationSourceSelect struct {
	SseKmsEncryptedObjects *replicationStatus `xml:"SseKmsEncryptedObjects,omitempty"`
	ReplicaModifications   *replicationStatus `xml:"ReplicaModifications,omitempty"`
}

// replicationDestination is <Destination>.
type replicationDestination struct {
	Bucket                   string                     `xml:"Bucket"`
	Account                  string                     `xml:"Account,omitempty"`
	StorageClass             string                     `xml:"StorageClass,omitempty"`
	AccessControlTranslation *replicationACLTranslation `xml:"AccessControlTranslation,omitempty"`
	EncryptionConfiguration  *replicationEncryption     `xml:"EncryptionConfiguration,omitempty"`
	ReplicationTime          *replicationTime           `xml:"ReplicationTime,omitempty"`
	Metrics                  *replicationMetrics        `xml:"Metrics,omitempty"`
}

// replicationACLTranslation is <AccessControlTranslation>.
type replicationACLTranslation struct {
	Owner string `xml:"Owner"`
}

// replicationEncryption is <EncryptionConfiguration>.
type replicationEncryption struct {
	ReplicaKmsKeyID string `xml:"ReplicaKmsKeyID,omitempty"`
}

// replicationTime is <ReplicationTime>.
type replicationTime struct {
	Status string              `xml:"Status"`
	Time   *replicationMinutes `xml:"Time,omitempty"`
}

// replicationMetrics is <Metrics>.
type replicationMetrics struct {
	Status         string              `xml:"Status"`
	EventThreshold *replicationMinutes `xml:"EventThreshold,omitempty"`
}

// replicationMinutes is an element holding <Minutes>.
type replicationMinutes struct {
	Minutes int `xml:"Minutes"`
}

// replArgError is an InvalidArgument error of a replication configuration.
func replArgError(name, msg string) error {
	return s3err.ErrInvalidArgument.WithMessage("%s", msg).With("ArgumentName", name)
}

// validate checks the configuration: 1..1000 rules (MalformedXML), a Role
// of at most 2048 characters, and for each rule a unique ID of at most 255
// characters, Status Enabled|Disabled, not both Prefix and Filter, a
// filter with at most one of Prefix/Tag/And and valid tags, unique
// priorities, Enabled|Disabled sub-statuses, a Destination with an
// arn:aws:s3::: bucket ARN and a supported storage class, and an
// AccessControlTranslation owner of "Destination".
func (c *replicationConfiguration) validate() error {
	if len(c.Rules) == 0 || len(c.Rules) > maxReplicationRules {
		return s3err.ErrMalformedXML
	}
	c.Role = strings.TrimSpace(c.Role)
	if len(c.Role) > maxReplicationRole {
		return replArgError("Role", "The IAM role ARN is too long.")
	}
	ids := map[string]bool{}
	priorities := map[int]bool{}
	for i := range c.Rules {
		rule := &c.Rules[i]
		rule.ID = strings.TrimSpace(rule.ID)
		if len(rule.ID) > maxReplicationRuleID {
			return replArgError("ID", "The ID of a replication rule must be at most 255 characters.")
		}
		if rule.ID != "" {
			if ids[rule.ID] {
				return replArgError("ID", "Rule Id must be unique.")
			}
			ids[rule.ID] = true
		}
		if !enabledOrDisabled(rule.Status) {
			return s3err.ErrMalformedXML
		}
		if rule.Prefix != nil && rule.Filter != nil {
			return s3err.ErrMalformedXML
		}
		if f := rule.Filter; f != nil {
			n := 0
			for _, set := range []bool{f.Prefix != nil, f.Tag != nil, f.And != nil} {
				if set {
					n++
				}
			}
			if n > 1 {
				return s3err.ErrMalformedXML
			}
			if f.Tag != nil {
				if err := validateTag(f.Tag.Key, f.Tag.Value); err != nil {
					return err
				}
			}
			if f.And != nil {
				if _, err := validateTags(f.And.Tags, maxObjectTags, "Replication filter tags cannot be greater than 10"); err != nil {
					return err
				}
			}
		}
		if rule.Priority != nil {
			if *rule.Priority < 0 {
				return replArgError("Priority", "Priority must be a non-negative integer.")
			}
			if priorities[*rule.Priority] {
				return replArgError("Priority", "Found two rules with same priority "+strconv.Itoa(*rule.Priority))
			}
			priorities[*rule.Priority] = true
		}
		for _, st := range []*replicationStatus{rule.ExistingObjectReplication, rule.DeleteMarkerReplication} {
			if st != nil && !enabledOrDisabled(st.Status) {
				return s3err.ErrMalformedXML
			}
		}
		if ss := rule.SourceSelectionCriteria; ss != nil {
			for _, st := range []*replicationStatus{ss.SseKmsEncryptedObjects, ss.ReplicaModifications} {
				if st != nil && !enabledOrDisabled(st.Status) {
					return s3err.ErrMalformedXML
				}
			}
		}
		if err := rule.Destination.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validate checks a replication destination (see
// replicationConfiguration.validate).
func (d *replicationDestination) validate() error {
	if d == nil {
		return s3err.ErrMalformedXML
	}
	d.Bucket = strings.TrimSpace(d.Bucket)
	name, ok := strings.CutPrefix(d.Bucket, "arn:aws:s3:::")
	if !ok || !validBucketName(name) {
		return replArgError("Destination", "Invalid bucket ARN.")
	}
	if d.StorageClass != "" {
		if _, err := normalizeStorageClass(d.StorageClass); err != nil {
			return err
		}
	}
	if t := d.AccessControlTranslation; t != nil && t.Owner != "Destination" {
		return s3err.ErrMalformedXML
	}
	if t := d.ReplicationTime; t != nil && (!enabledOrDisabled(t.Status) || t.Time == nil) {
		return s3err.ErrMalformedXML
	}
	if m := d.Metrics; m != nil && !enabledOrDisabled(m.Status) {
		return s3err.ErrMalformedXML
	}
	return nil
}

// handleGetBucketReplication implements GetBucketReplication
// (ReplicationConfigurationNotFoundError when unset).
func (s *Server) handleGetBucketReplication(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigReplication, s3err.ErrReplicationConfigurationNotFound)
	if err != nil {
		return err
	}
	var c replicationConfiguration
	if err := s.decodeStoredConfig(ctx, rc, storage.ConfigReplication, raw, &c); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutBucketReplication implements PutBucketReplication (see
// replicationConfiguration.validate). Versioning must be Enabled on the
// bucket (InvalidRequest). The configuration is stored only: gos3 does not
// replicate objects.
func (s *Server) handlePutBucketReplication(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c replicationConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	unlock := s.lockBucketState(rc.bucket)
	defer unlock()
	bi, err := s.lookupBucket(ctx, rc.bucket)
	if err != nil {
		return err
	}
	if bi.Versioning != storage.VersioningEnabled {
		return s3err.ErrInvalidRequest.WithMessage("Versioning must be 'Enabled' on the bucket to apply a replication configuration")
	}
	c.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigReplication, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeleteBucketReplication implements DeleteBucketReplication (204).
func (s *Server) handleDeleteBucketReplication(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	unlock := s.lockBucketState(rc.bucket)
	defer unlock()
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigReplication)
}

// --- Accelerate ---

// accelerateConfiguration is the <AccelerateConfiguration> document
// (storage.ConfigAccelerate).
type accelerateConfiguration struct {
	XMLName xml.Name `xml:"AccelerateConfiguration"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

// handleGetBucketAccelerate implements GetBucketAccelerateConfiguration:
// the stored Status, or an empty AccelerateConfiguration when never set.
func (s *Server) handleGetBucketAccelerate(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.loadBucketConfig(ctx, rc, storage.ConfigAccelerate)
	if err != nil {
		return err
	}
	var c accelerateConfiguration
	if raw != nil {
		if err := s.decodeStoredConfig(ctx, rc, storage.ConfigAccelerate, raw, &c); err != nil {
			return err
		}
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutBucketAccelerate implements PutBucketAccelerateConfiguration:
// Status Enabled or Suspended (MalformedXML otherwise); buckets with '.'
// in their name cannot be accelerated (InvalidRequest). The setting is
// stored only.
func (s *Server) handlePutBucketAccelerate(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c accelerateConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	c.Status = strings.TrimSpace(c.Status)
	if c.Status != "Enabled" && c.Status != "Suspended" {
		return s3err.ErrMalformedXML
	}
	if strings.Contains(rc.bucket, ".") {
		return s3err.ErrInvalidRequest.WithMessage("S3 Transfer Acceleration is not supported for buckets with periods (.) in their names")
	}
	c.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigAccelerate, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- Request payment ---

// requestPaymentConfiguration is the <RequestPaymentConfiguration>
// document.
type requestPaymentConfiguration struct {
	XMLName xml.Name `xml:"RequestPaymentConfiguration"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Payer   string   `xml:"Payer"`
}

// payerBucketOwner is the only supported request payer.
const payerBucketOwner = "BucketOwner"

// handleGetBucketRequestPayment implements GetBucketRequestPayment: the
// payer is always BucketOwner.
func (s *Server) handleGetBucketRequestPayment(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	if _, err := s.bucketInfo(r.Context(), rc); err != nil {
		return err
	}
	return writeXML(w, http.StatusOK, requestPaymentConfiguration{XMLNS: s3Namespace, Payer: payerBucketOwner})
}

// handlePutBucketRequestPayment implements PutBucketRequestPayment: Payer
// BucketOwner is accepted (it is the default, so nothing is stored),
// Requester is NotImplemented and anything else MalformedXML.
func (s *Server) handlePutBucketRequestPayment(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c requestPaymentConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	switch strings.TrimSpace(c.Payer) {
	case payerBucketOwner:
	case "Requester":
		return s3err.ErrNotImplemented.WithMessage("Requester Pays buckets are not supported.")
	default:
		return s3err.ErrMalformedXML
	}
	if err := s.removeBucketConfig(ctx, rc, storage.ConfigRequestPayment); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}
