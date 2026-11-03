package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ReplicationState is saved alongside a canonical rule document. A worker
// only executes rules with the matching hash, so an interrupted rule update
// cannot run an old configuration under the new activation time.
type ReplicationState struct {
	Hash        string    `json:"hash"`
	ActivatedAt time.Time `json:"activatedAt"`
}

func ReplicationRuleHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ExecutableReplicationRule is the subset of S3 bucket replication rules
// gos3 can execute. A stored rule that cannot be compiled must not be run.
type ExecutableReplicationRule struct {
	ID            string
	Priority      int
	Enabled       bool
	Prefix        string
	Tags          map[string]string
	Destination   string
	DeleteMarkers bool
	Backfill      bool
}

// Matches checks a source version against this rule. Delete markers have no
// object tags and therefore cannot match a tag-filtered rule.
func (r ExecutableReplicationRule) Matches(key string, tags map[string]string, deleteMarker bool) bool {
	if !r.Enabled || !strings.HasPrefix(key, r.Prefix) || (deleteMarker && !r.DeleteMarkers) {
		return false
	}
	if deleteMarker && len(r.Tags) > 0 {
		return false
	}
	for key, value := range r.Tags {
		if tags[key] != value {
			return false
		}
	}
	return true
}

// MatchingReplicationRules returns at most one matching rule for each
// destination. The highest numeric priority wins for overlapping rules.
func MatchingReplicationRules(rules []ExecutableReplicationRule, key string, tags map[string]string, deleteMarker bool) []ExecutableReplicationRule {
	ordered := slices.Clone(rules)
	slices.SortFunc(ordered, func(a, b ExecutableReplicationRule) int {
		if a.Priority != b.Priority {
			if a.Priority > b.Priority {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	seen := make(map[string]bool)
	var result []ExecutableReplicationRule
	for _, rule := range ordered {
		if !seen[rule.Destination] && rule.Matches(key, tags, deleteMarker) {
			seen[rule.Destination] = true
			result = append(result, rule)
		}
	}
	return result
}

var replicationRoleARN = regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_-]{1,64}$`)

// CompileReplicationRules parses a stored ?replication document into rules
// the worker can honor. PUT must call it before saving; workers call it again
// when loading older documents and leave unsupported ones inactive.
func CompileReplicationRules(raw []byte) (string, []ExecutableReplicationRule, error) {
	if err := strictReplicationElements(raw); err != nil {
		return "", nil, err
	}
	var config replicationConfiguration
	if err := xml.Unmarshal(raw, &config); err != nil {
		return "", nil, fmt.Errorf("replication XML: %w", err)
	}
	if err := config.validate(); err != nil {
		return "", nil, err
	}
	if !replicationRoleARN.MatchString(config.Role) {
		return "", nil, replArgError("Role", "A local replication role ARN is required.")
	}
	if len(config.Rules) > 1 {
		for _, rule := range config.Rules {
			if rule.Priority == nil {
				return "", nil, replArgError("Priority", "Every rule needs a priority when multiple rules are configured.")
			}
		}
	}
	compiled := make([]ExecutableReplicationRule, 0, len(config.Rules))
	for _, rule := range config.Rules {
		if rule.SourceSelectionCriteria != nil || rule.Destination.Account != "" || rule.Destination.StorageClass != "" || rule.Destination.AccessControlTranslation != nil || rule.Destination.EncryptionConfiguration != nil || rule.Destination.ReplicationTime != nil || rule.Destination.Metrics != nil {
			return "", nil, replArgError("Rule", "This replication rule contains a field gos3 cannot execute.")
		}
		out := ExecutableReplicationRule{ID: rule.ID, Enabled: rule.Status == "Enabled", Destination: strings.TrimPrefix(rule.Destination.Bucket, "arn:aws:s3:::"), DeleteMarkers: rule.DeleteMarkerReplication != nil && rule.DeleteMarkerReplication.Status == "Enabled", Backfill: rule.ExistingObjectReplication != nil && rule.ExistingObjectReplication.Status == "Enabled"}
		if rule.Priority != nil {
			out.Priority = *rule.Priority
		}
		if rule.Prefix != nil {
			out.Prefix = *rule.Prefix
		}
		if f := rule.Filter; f != nil {
			switch {
			case f.Prefix != nil:
				out.Prefix = *f.Prefix
			case f.Tag != nil:
				out.Tags = map[string]string{f.Tag.Key: f.Tag.Value}
			case f.And != nil:
				if f.And.Prefix != nil {
					out.Prefix = *f.And.Prefix
				}
				out.Tags = make(map[string]string, len(f.And.Tags))
				for _, tag := range f.And.Tags {
					out.Tags[tag.Key] = tag.Value
				}
			}
		}
		if out.DeleteMarkers && len(out.Tags) > 0 {
			return "", nil, replArgError("DeleteMarkerReplication", "Tag-filtered rules cannot replicate delete markers.")
		}
		compiled = append(compiled, out)
	}
	return config.Role, compiled, nil
}

var replicationChildren = map[string]map[string]bool{
	"":                                     {"ReplicationConfiguration": true},
	"ReplicationConfiguration":             {"Role": true, "Rule": true},
	"ReplicationConfiguration/Rule":        {"ID": true, "Priority": true, "Status": true, "Prefix": true, "Filter": true, "ExistingObjectReplication": true, "Destination": true, "DeleteMarkerReplication": true},
	"ReplicationConfiguration/Rule/Filter": {"Prefix": true, "Tag": true, "And": true},
	"ReplicationConfiguration/Rule/Filter/And":                {"Prefix": true, "Tag": true},
	"ReplicationConfiguration/Rule/Filter/Tag":                {"Key": true, "Value": true},
	"ReplicationConfiguration/Rule/Filter/And/Tag":            {"Key": true, "Value": true},
	"ReplicationConfiguration/Rule/Destination":               {"Bucket": true},
	"ReplicationConfiguration/Rule/DeleteMarkerReplication":   {"Status": true},
	"ReplicationConfiguration/Rule/ExistingObjectReplication": {"Status": true},
}

func strictReplicationElements(raw []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var stack []string
	var root bool
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("replication XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			parent := strings.Join(stack, "/")
			if !replicationChildren[parent][value.Name.Local] || value.Name.Space != "" && value.Name.Space != s3Namespace || (len(stack) == 0 && root) {
				return replArgError("Rule", "Unsupported replication XML element: "+value.Name.Local)
			}
			for _, attr := range value.Attr {
				if len(stack) != 0 || attr.Name.Local != "xmlns" || attr.Name.Space != "" || attr.Value != s3Namespace {
					return replArgError("Rule", "Unsupported replication XML attribute: "+attr.Name.Local)
				}
			}
			if len(stack) == 0 {
				root = true
			}
			stack = append(stack, value.Name.Local)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 || replicationChildren[strings.Join(stack, "/")] != nil {
				if len(bytes.TrimSpace(value)) > 0 {
					return replArgError("Rule", "Unexpected text in replication XML.")
				}
			}
		case xml.Directive:
			return replArgError("Rule", "XML directives are not supported in replication rules.")
		case xml.ProcInst:
			if value.Target != "xml" || root {
				return replArgError("Rule", "XML processing instructions are not supported in replication rules.")
			}
		}
	}
	if !root || len(stack) != 0 {
		return replArgError("Rule", "A ReplicationConfiguration XML document is required.")
	}
	return nil
}
