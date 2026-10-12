package auth

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// Limits for browser POST policies.
const (
	// maxPolicySize bounds the base64 policy form field.
	maxPolicySize = 64 << 10
	// maxPolicyConditions bounds the number of policy conditions.
	maxPolicyConditions = 512
)

// Form fields that never need a policy condition.
var postPolicyExempt = map[string]bool{
	"x-amz-signature": true,
	"file":            true,
	"policy":          true,
	// The bucket is part of the request URL, not a user supplied field; the
	// object handler adds it to the form so {"bucket": ...} can be checked.
	"bucket": true,
}

func missingPostField(name string) error {
	return s3err.ErrInvalidArgument.
		WithMessage("Bucket POST must contain a field named '%s'. If it is specified, please check the order of the fields.", name).
		With("ArgumentName", name)
}

// lowerForm returns form with lowercased keys (defensive; the contract says
// keys are already lowercase).
func lowerForm(form PostPolicyForm) PostPolicyForm {
	out := make(PostPolicyForm, len(form))
	for k, v := range form {
		out[strings.ToLower(k)] = v
	}
	return out
}

// verifyPostPolicy implements Verifier.VerifyPostPolicy.
func (v *Verifier) verifyPostPolicy(form PostPolicyForm) (Identity, *PostPolicyConditions, error) {
	form = lowerForm(form)
	policy := form["policy"]
	_, hasSig := form["x-amz-signature"]
	if !hasSig {
		_, v2sig := form["signature"]
		_, v2key := form["awsaccesskeyid"]
		if v2sig || v2key {
			return Identity{}, nil, errSigV2
		}
	}
	if policy == "" && !hasSig {
		if _, ok := form["x-amz-credential"]; !ok {
			return Anonymous, nil, nil
		}
	}
	if policy == "" {
		return Identity{}, nil, missingPostField("policy")
	}
	for _, f := range []string{"x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-signature"} {
		if form[f] == "" {
			return Identity{}, nil, missingPostField(f)
		}
	}
	if form["x-amz-algorithm"] != signV4Algorithm {
		return Identity{}, nil, s3err.ErrInvalidArgument.
			WithMessage(`Only the "AWS4-HMAC-SHA256" algorithm is supported for POST uploads.`).
			With("ArgumentName", "x-amz-algorithm").
			With("ArgumentValue", truncate(form["x-amz-algorithm"], 64))
	}
	t, err := time.Parse(iso8601Format, form["x-amz-date"])
	if err != nil {
		return Identity{}, nil, s3err.ErrInvalidArgument.
			WithMessage(`X-Amz-Date must be in the ISO8601 Long Format "yyyyMMdd'T'HHmmss'Z'"`).
			With("ArgumentName", "x-amz-date")
	}
	cred, err := v.parseCredential(form["x-amz-credential"], kindPost)
	if err != nil {
		return Identity{}, nil, err
	}
	if err := checkCredentialDate(cred, t, kindPost); err != nil {
		return Identity{}, nil, err
	}
	if _, ok := form["x-amz-security-token"]; ok {
		return Identity{}, nil, s3err.ErrInvalidToken
	}
	if len(policy) > maxPolicySize {
		return Identity{}, nil, s3err.ErrInvalidPolicyDocument.WithMessage("Invalid Policy: Policy document is too large.")
	}
	secret, id, err := v.lookup(cred.accessKey)
	if err != nil {
		return Identity{}, nil, err
	}
	key := SigningKey(secret, cred.date, cred.region, cred.service)
	want := hex.EncodeToString(hmacSHA256(key, policy))
	if !hmac.Equal([]byte(want), []byte(form["x-amz-signature"])) {
		return Identity{}, nil, s3err.ErrSignatureDoesNotMatch.
			With("AWSAccessKeyId", cred.accessKey).
			With("StringToSign", policy).
			With("SignatureProvided", truncate(form["x-amz-signature"], 128))
	}
	p, err := decodePostPolicy(policy)
	if err != nil {
		return Identity{}, nil, err
	}
	if now := v.opts.Now(); !now.Before(p.Expiration) {
		return Identity{}, nil, s3err.ErrAccessDenied.WithMessage("Invalid according to Policy: Policy expired.")
	}
	return id, p, nil
}

// decodePostPolicy decodes a base64 policy document and validates its
// structure.
func decodePostPolicy(policy string) (*PostPolicyConditions, error) {
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, policy)
	raw, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(compact); err != nil {
			return nil, s3err.ErrInvalidPolicyDocument.WithMessage("Invalid Policy: Invalid base64 encoding of the policy document.")
		}
	}
	var doc struct {
		Expiration *string `json:"expiration"`
		Conditions []any   `json:"conditions"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, s3err.ErrInvalidPolicyDocument.WithMessage("Invalid Policy: Invalid JSON.")
	}
	if dec.More() {
		return nil, s3err.ErrInvalidPolicyDocument.WithMessage("Invalid Policy: Invalid JSON.")
	}
	if doc.Expiration == nil {
		return nil, s3err.ErrInvalidPolicyDocument.WithMessage("Invalid Policy: Policy missing expiration.")
	}
	exp, err := time.Parse(time.RFC3339Nano, *doc.Expiration)
	if err != nil {
		return nil, s3err.ErrInvalidPolicyDocument.WithMessage("%s", "Invalid Policy: Invalid 'expiration' value: "+strconv.Quote(truncate(*doc.Expiration, 64)))
	}
	if doc.Conditions == nil {
		return nil, s3err.ErrInvalidPolicyDocument.WithMessage("Invalid Policy: Policy missing conditions.")
	}
	p := &PostPolicyConditions{Expiration: exp, Conditions: doc.Conditions}
	if _, err := parsePolicyConditions(p.Conditions); err != nil {
		return nil, err
	}
	return p, nil
}

// Policy condition operators.
const (
	condEq         = "eq"
	condStartsWith = "starts-with"
	condRange      = "content-length-range"
)

// policyCond is one normalised policy condition.
type policyCond struct {
	op       string
	field    string // lowercase, without '$'
	value    string
	min, max int64
	raw      string // for error messages
}

func invalidCondition(c any) error {
	b, _ := json.Marshal(c)
	return s3err.ErrInvalidPolicyDocument.WithMessage("%s", "Invalid Policy: Invalid Simple-Condition: "+truncate(string(b), 256))
}

// condString converts a JSON scalar condition value to a string.
func condString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}

// condInt converts a JSON number (or numeric string) to a non-negative int64.
func condInt(v any) (int64, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	case float64:
		if x < 0 || x > math.MaxInt64 || x != math.Trunc(x) {
			return 0, false
		}
		return int64(x), true
	default:
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// parsePolicyConditions normalises raw policy conditions.
func parsePolicyConditions(raw []any) ([]policyCond, error) {
	if len(raw) > maxPolicyConditions {
		return nil, s3err.ErrInvalidPolicyDocument.WithMessage("Invalid Policy: Too many conditions.")
	}
	var out []policyCond
	for _, c := range raw {
		rawJSON, _ := json.Marshal(c)
		switch x := c.(type) {
		case map[string]any:
			if len(x) == 0 {
				return nil, invalidCondition(c)
			}
			for k, val := range x {
				s, ok := condString(val)
				if !ok {
					return nil, invalidCondition(c)
				}
				field := strings.ToLower(strings.TrimPrefix(k, "$"))
				if field == "" {
					return nil, invalidCondition(c)
				}
				out = append(out, policyCond{op: condEq, field: field, value: s, raw: string(rawJSON)})
			}
		case []any:
			if len(x) != 3 {
				return nil, invalidCondition(c)
			}
			op, ok := x[0].(string)
			if !ok {
				return nil, invalidCondition(c)
			}
			switch strings.ToLower(op) {
			case condEq, condStartsWith:
				f, ok := x[1].(string)
				if !ok || !strings.HasPrefix(f, "$") || len(f) < 2 {
					return nil, invalidCondition(c)
				}
				s, ok := condString(x[2])
				if !ok {
					return nil, invalidCondition(c)
				}
				out = append(out, policyCond{op: strings.ToLower(op), field: strings.ToLower(f[1:]), value: s, raw: string(rawJSON)})
			case condRange:
				lo, ok1 := condInt(x[1])
				hi, ok2 := condInt(x[2])
				if !ok1 || !ok2 || lo > hi {
					return nil, invalidCondition(c)
				}
				out = append(out, policyCond{op: condRange, min: lo, max: hi, raw: string(rawJSON)})
			default:
				return nil, invalidCondition(c)
			}
		default:
			return nil, invalidCondition(c)
		}
	}
	return out, nil
}

func conditionFailed(c policyCond) error {
	return s3err.ErrAccessDenied.WithMessage("%s", "Invalid according to Policy: Policy Condition failed: "+truncate(c.raw, 256))
}

// checkPostPolicy implements CheckPostPolicy.
func checkPostPolicy(p *PostPolicyConditions, form PostPolicyForm, fileSize int64) error {
	if p == nil {
		return nil
	}
	conds, err := parsePolicyConditions(p.Conditions)
	if err != nil {
		return err
	}
	form = lowerForm(form)
	covered := map[string]bool{}
	for _, c := range conds {
		switch c.op {
		case condEq:
			covered[c.field] = true
			if form[c.field] != c.value {
				return conditionFailed(c)
			}
		case condStartsWith:
			covered[c.field] = true
			val := form[c.field]
			if c.field == "content-type" {
				// AWS allows a comma separated list of content types; each
				// one must match the prefix.
				for _, ct := range strings.Split(val, ",") {
					if !strings.HasPrefix(strings.TrimSpace(ct), c.value) {
						return conditionFailed(c)
					}
				}
			} else if !strings.HasPrefix(val, c.value) {
				return conditionFailed(c)
			}
		case condRange:
			if fileSize < 0 {
				continue
			}
			if fileSize > c.max {
				return s3err.ErrEntityTooLarge.
					WithMessage("Your proposed upload exceeds the maximum allowed size").
					With("ProposedSize", strconv.FormatInt(fileSize, 10)).
					With("MaxSizeAllowed", strconv.FormatInt(c.max, 10))
			}
			if fileSize < c.min {
				return s3err.ErrEntityTooSmall.
					WithMessage("Your proposed upload is smaller than the minimum allowed size").
					With("ProposedSize", strconv.FormatInt(fileSize, 10)).
					With("MinSizeAllowed", strconv.FormatInt(c.min, 10))
			}
		}
	}
	var extra []string
	for k := range form {
		if postPolicyExempt[k] || strings.HasPrefix(k, "x-ignore-") || covered[k] {
			continue
		}
		extra = append(extra, k)
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return s3err.ErrAccessDenied.WithMessage("%s", "Invalid according to Policy: Extra input fields: "+truncate(strings.Join(extra, ", "), 256))
	}
	return nil
}

// SignPostPolicy builds and signs a browser POST policy (client helper for
// tests and UIs). The policy contains expiration, the given conditions and
// the x-amz-algorithm / x-amz-credential / x-amz-date conditions that AWS
// requires; the returned form holds the policy, x-amz-algorithm,
// x-amz-credential, x-amz-date and x-amz-signature fields to send alongside
// key, file and any other conditioned fields.
func SignPostPolicy(c Credentials, region string, t time.Time, expiration time.Time, conditions []any) (PostPolicyForm, error) {
	t = t.UTC()
	amz := t.Format(iso8601Format)
	credStr := c.AccessKey + "/" + t.Format(yyyymmdd) + "/" + region + "/" + serviceS3 + "/" + scopeTerminator
	conds := make([]any, 0, len(conditions)+3)
	conds = append(conds, conditions...)
	conds = append(conds,
		map[string]string{"x-amz-algorithm": signV4Algorithm},
		map[string]string{"x-amz-credential": credStr},
		map[string]string{"x-amz-date": amz},
	)
	doc, err := json.Marshal(map[string]any{
		"expiration": expiration.UTC().Format("2006-01-02T15:04:05.000Z"),
		"conditions": conds,
	})
	if err != nil {
		return nil, err
	}
	policy := base64.StdEncoding.EncodeToString(doc)
	return PostPolicyForm{
		"policy":           policy,
		"x-amz-algorithm":  signV4Algorithm,
		"x-amz-credential": credStr,
		"x-amz-date":       amz,
		"x-amz-signature":  PostPolicySignature(c.SecretKey, t.Format(yyyymmdd), region, policy),
	}, nil
}

// PostPolicySignature returns the SigV4 signature of a base64 POST policy:
// hex(HMAC-SHA256(SigningKey(secretKey, date, region, "s3"), policyBase64)),
// where date is YYYYMMDD.
func PostPolicySignature(secretKey, date, region, policyBase64 string) string {
	return hex.EncodeToString(hmacSHA256(SigningKey(secretKey, date, region, serviceS3), policyBase64))
}
