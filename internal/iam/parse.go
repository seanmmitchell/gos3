package iam

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// MaxPolicySize is the maximum size in bytes of a policy document (identity
// or bucket policy), as in AWS (20 KiB).
const MaxPolicySize = 20 * 1024

// Policy versions accepted by the parsers ("" is treated as 2008-10-17,
// which in gos3 only differs in that policy variables are always expanded).
const (
	PolicyVersion2012 = "2012-10-17"
	PolicyVersion2008 = "2008-10-17"
)

// Fields allowed in a policy statement (case-sensitive, like AWS).
var statementFields = map[string]bool{
	"Sid": true, "Effect": true, "Principal": true, "NotPrincipal": true,
	"Action": true, "NotAction": true, "Resource": true, "NotResource": true,
	"Condition": true,
}

// decodePolicy strictly decodes a policy document. It returns a non-empty
// AWS-style reason on failure.
func decodePolicy(data []byte) (*PolicyDocument, string) {
	if len(data) > MaxPolicySize {
		return nil, fmt.Sprintf("Policy exceeds the maximum allowed document size of %d bytes.", MaxPolicySize)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, "Policies must be valid JSON and the first byte must be '{'"
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, "This policy contains invalid Json"
	}
	doc := &PolicyDocument{}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		v := top[k]
		switch k {
		case "Version":
			if err := json.Unmarshal(v, &doc.Version); err != nil {
				return nil, "The policy Version must be a string"
			}
		case "Id":
			if err := json.Unmarshal(v, &doc.ID); err != nil {
				return nil, "The policy Id must be a string"
			}
		case "Statement":
		default:
			return nil, "Unknown field " + k
		}
	}
	raw, ok := top["Statement"]
	if !ok {
		return nil, "Missing required field Statement"
	}
	raw = bytes.TrimSpace(raw)
	var rawStmts []json.RawMessage
	switch {
	case len(raw) > 0 && raw[0] == '{':
		rawStmts = []json.RawMessage{raw}
	case len(raw) > 0 && raw[0] == '[':
		if err := json.Unmarshal(raw, &rawStmts); err != nil {
			return nil, "This policy contains invalid Json"
		}
	default:
		return nil, "Statement must be an object or an array of objects"
	}
	doc.Statement = make([]Statement, 0, len(rawStmts))
	for _, rs := range rawStmts {
		st, reason := decodeStatement(rs)
		if reason != "" {
			return nil, reason
		}
		doc.Statement = append(doc.Statement, st)
	}
	return doc, ""
}

// decodeStatement strictly decodes one statement.
func decodeStatement(raw json.RawMessage) (Statement, string) {
	var st Statement
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return st, "Each Statement must be an object"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return st, "This policy contains invalid Json"
	}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		v := fields[k]
		if !statementFields[k] {
			return st, "Unknown field " + k
		}
		var err error
		switch k {
		case "Sid":
			err = json.Unmarshal(v, &st.Sid)
		case "Effect":
			err = json.Unmarshal(v, &st.Effect)
		case "Principal":
			st.Principal, err = decodePrincipal(v)
			if err != nil {
				return st, "Invalid principal in policy"
			}
		case "NotPrincipal":
			st.NotPrincipal, err = decodePrincipal(v)
			if err != nil {
				return st, "Invalid principal in policy"
			}
		case "Action":
			err = json.Unmarshal(v, &st.Action)
		case "NotAction":
			err = json.Unmarshal(v, &st.NotAction)
		case "Resource":
			err = json.Unmarshal(v, &st.Resource)
		case "NotResource":
			err = json.Unmarshal(v, &st.NotResource)
		case "Condition":
			err = json.Unmarshal(v, &st.Condition)
		}
		if err != nil {
			return st, "Invalid value for field " + k
		}
	}
	return st, ""
}

func decodePrincipal(v json.RawMessage) (*Principal, error) {
	if string(bytes.TrimSpace(v)) == "null" {
		return &Principal{}, nil // present but empty: rejected by validation
	}
	p := &Principal{}
	if err := p.UnmarshalJSON(v); err != nil {
		return nil, err
	}
	return p, nil
}

// validateIdentityPolicy validates an identity (user attached) policy.
func validateIdentityPolicy(doc *PolicyDocument) string {
	if reason := validateCommon(doc); reason != "" {
		return reason
	}
	for i := range doc.Statement {
		st := &doc.Statement[i]
		if st.Principal != nil {
			return "Has prohibited field Principal"
		}
		if st.NotPrincipal != nil {
			return "Has prohibited field NotPrincipal"
		}
		for _, a := range actionsOf(st) {
			if !validAction(a, true) {
				return "Policy has invalid action: " + a
			}
		}
		for _, r := range resourcesOf(st) {
			if !validIdentityResource(r) {
				return "Policy has invalid resource: " + r
			}
		}
	}
	return ""
}

// validateBucketPolicy validates a bucket policy for bucket.
func validateBucketPolicy(doc *PolicyDocument, bucket string) string {
	if reason := validateCommon(doc); reason != "" {
		return reason
	}
	for i := range doc.Statement {
		st := &doc.Statement[i]
		switch {
		case st.Principal != nil && st.NotPrincipal != nil:
			return "Statement cannot contain both Principal and NotPrincipal"
		case st.Principal == nil && st.NotPrincipal == nil:
			return "Missing required field Principal"
		case st.Principal != nil && !validPrincipal(st.Principal):
			return "Invalid principal in policy"
		case st.NotPrincipal != nil && !validPrincipal(st.NotPrincipal):
			return "Invalid principal in policy"
		}
		for _, a := range actionsOf(st) {
			if !validAction(a, false) {
				return "Policy has invalid action"
			}
		}
		for _, r := range resourcesOf(st) {
			if !validBucketResource(r, bucket) {
				return "Policy has invalid resource"
			}
		}
	}
	return ""
}

// validateCommon checks version, statements, effects, required fields and
// conditions.
func validateCommon(doc *PolicyDocument) string {
	if doc == nil {
		return "Missing required field Statement"
	}
	switch doc.Version {
	case "", PolicyVersion2012, PolicyVersion2008:
	default:
		return "The policy must contain a valid version string"
	}
	if len(doc.Statement) == 0 {
		return "Missing required field Statement"
	}
	sids := make(map[string]bool, len(doc.Statement))
	for i := range doc.Statement {
		st := &doc.Statement[i]
		if st.Sid != "" {
			if sids[st.Sid] {
				return "Statement IDs (SID) in a single policy must be unique"
			}
			sids[st.Sid] = true
		}
		switch st.Effect {
		case "Allow", "Deny":
		case "":
			return "Missing required field Effect"
		default:
			return "Invalid effect: " + st.Effect
		}
		switch {
		case len(st.Action) > 0 && len(st.NotAction) > 0:
			return "Statement cannot contain both Action and NotAction"
		case len(st.Action) == 0 && len(st.NotAction) == 0:
			return "Missing required field Action"
		case len(st.Resource) > 0 && len(st.NotResource) > 0:
			return "Statement cannot contain both Resource and NotResource"
		case len(st.Resource) == 0 && len(st.NotResource) == 0:
			return "Missing required field Resource"
		}
		if reason := validateConditions(st.Condition); reason != "" {
			return reason
		}
	}
	return ""
}

func actionsOf(st *Statement) StringOrSlice {
	if len(st.Action) > 0 {
		return st.Action
	}
	return st.NotAction
}

func resourcesOf(st *Statement) StringOrSlice {
	if len(st.Resource) > 0 {
		return st.Resource
	}
	return st.NotResource
}

// validAction reports whether a looks like "*", "s3:<name>" or (when
// allowAdmin) "admin:<name>", where <name> consists of letters, digits and
// the wildcards * and ?.
func validAction(a string, allowAdmin bool) bool {
	if a == "*" {
		return true
	}
	svc, name, ok := strings.Cut(a, ":")
	if !ok || name == "" || len(a) > 128 {
		return false
	}
	switch strings.ToLower(svc) {
	case "s3":
	case "admin":
		if !allowAdmin {
			return false
		}
	default:
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isAlnum(c) && c != '*' && c != '?' {
			return false
		}
	}
	return true
}

// validIdentityResource accepts "*" and ARNs (arn:partition:service:region:account:resource).
func validIdentityResource(r string) bool {
	if r == "*" {
		return true
	}
	if len(r) > 2048 || !strings.HasPrefix(r, "arn:") {
		return false
	}
	parts := strings.SplitN(r, ":", 6)
	return len(parts) == 6 && parts[1] != "" && parts[2] != "" && parts[5] != ""
}

const s3ARNPrefix = "arn:aws:s3:::"

// validBucketResource accepts arn:aws:s3:::<bucket>[/...] where the bucket
// part (wildcards allowed) matches bucket.
func validBucketResource(r, bucket string) bool {
	if len(r) > 2048 || !strings.HasPrefix(r, s3ARNPrefix) {
		return false
	}
	rest := r[len(s3ARNPrefix):]
	bpat, _, _ := strings.Cut(rest, "/")
	if bpat == "" {
		return false
	}
	return globMatch(bpat, bucket, false, false)
}

// validPrincipal checks a bucket policy principal.
func validPrincipal(p *Principal) bool {
	if p.Any {
		return true
	}
	if len(p.AWS) == 0 {
		return false
	}
	for _, v := range p.AWS {
		if !validPrincipalEntry(v) {
			return false
		}
	}
	return true
}

// validPrincipalEntry accepts "*", arn:<partition>:iam::<account>:root,
// arn:<partition>:iam::<account>:user/[path/]<name>, account ids, user names
// and access key ids.
func validPrincipalEntry(v string) bool {
	if v == "*" {
		return true
	}
	if strings.HasPrefix(v, "arn:") {
		parts := strings.SplitN(v, ":", 6)
		if len(parts) != 6 || parts[2] != "iam" || parts[4] == "" {
			return false
		}
		if parts[5] == "root" {
			return true
		}
		name, ok := strings.CutPrefix(parts[5], "user/")
		if !ok {
			return false
		}
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		return validName(name, 64)
	}
	return validName(v, 128)
}

// validName reports whether s matches [A-Za-z0-9+=,.@_-]{1,max}.
func validName(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAlnum(c) {
			continue
		}
		switch c {
		case '+', '=', ',', '.', '@', '_', '-':
		default:
			return false
		}
	}
	return true
}

// isDotSegment reports whether s is "." or "..". Such names are valid
// characters-wise but cannot be addressed in a URL path (clients resolve
// the dot segment), so a user or policy with that name could never be
// read, changed or deleted through the admin API. They are refused when
// creating users and policies (stored ones still load).
func isDotSegment(s string) bool { return s == "." || s == ".." }

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
