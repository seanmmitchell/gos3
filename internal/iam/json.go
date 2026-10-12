package iam

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// errPrincipal is returned by Principal.UnmarshalJSON for unsupported shapes.
// The text is AWS's MalformedPolicy message and reaches clients verbatim.
//
//lint:ignore ST1005 matches the AWS error message clients see
var errPrincipal = errors.New("Invalid principal in policy")

// UnmarshalJSON accepts a JSON string, an array of strings, or null. Number
// and boolean scalars (common in Condition blocks, e.g.
// "aws:SecureTransport": false) are converted to their literal text.
func (s *StringOrSlice) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("iam: empty JSON value")
	}
	switch data[0] {
	case 'n':
		if string(data) == "null" {
			*s = nil
			return nil
		}
	case '[':
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		out := make(StringOrSlice, 0, len(raw))
		for _, r := range raw {
			v, err := scalarString(r)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		*s = out
		return nil
	}
	v, err := scalarString(data)
	if err != nil {
		return err
	}
	*s = StringOrSlice{v}
	return nil
}

// MarshalJSON renders a single value as a JSON string and anything else as
// an array (the AWS convention).
func (s StringOrSlice) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(s))
}

// scalarString converts a JSON string/number/bool literal to a Go string.
func scalarString(r []byte) (string, error) {
	r = bytes.TrimSpace(r)
	if len(r) == 0 {
		return "", errors.New("iam: empty JSON value")
	}
	switch c := r[0]; {
	case c == '"':
		var v string
		if err := json.Unmarshal(r, &v); err != nil {
			return "", err
		}
		return v, nil
	case c == 't' || c == 'f':
		var v bool
		if err := json.Unmarshal(r, &v); err != nil {
			return "", err
		}
		if v {
			return "true", nil
		}
		return "false", nil
	case c == '-' || (c >= '0' && c <= '9'):
		var n json.Number
		if err := json.Unmarshal(r, &n); err != nil {
			return "", err
		}
		return n.String(), nil
	}
	return "", fmt.Errorf("iam: expected a string or an array of strings, got %.32s", r)
}

// UnmarshalJSON accepts "*", {"AWS": "*"|"id"|[...]} and tolerates
// {"CanonicalUser": ...}, whose values are matched like AWS principals,
// and {"Service": ...} / {"Federated": ...}, which are kept but never
// match (gos3 has no service or federated identities). Other principal
// types are rejected.
func (p *Principal) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errPrincipal
	}
	switch data[0] {
	case '"':
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		if v != "*" {
			return errPrincipal
		}
		*p = Principal{Any: true}
		return nil
	case '{':
		var m map[string]StringOrSlice
		if err := json.Unmarshal(data, &m); err != nil {
			return errPrincipal
		}
		out := Principal{}
		for k, vals := range m {
			if typ := otherPrincipalType(k); typ != "" {
				for _, v := range vals {
					if v == "" {
						return errPrincipal
					}
				}
				if len(vals) == 0 {
					return errPrincipal
				}
				if out.other == nil {
					out.other = map[string]StringOrSlice{}
				}
				out.other[typ] = append(out.other[typ], vals...)
				continue
			}
			if !strings.EqualFold(k, "AWS") && !strings.EqualFold(k, "CanonicalUser") {
				return errPrincipal
			}
			for _, v := range vals {
				if v == "*" {
					out.Any = true
					continue
				}
				out.AWS = append(out.AWS, v)
			}
		}
		if out.Any {
			out.AWS = nil // "*" subsumes everything else
			out.other = nil
		}
		*p = out
		return nil
	}
	return errPrincipal
}

// otherPrincipalType returns the canonical name of a principal type that
// is accepted but never matches ("Service", "Federated"), or "".
func otherPrincipalType(k string) string {
	switch {
	case strings.EqualFold(k, "Service"):
		return "Service"
	case strings.EqualFold(k, "Federated"):
		return "Federated"
	}
	return ""
}

// MarshalJSON renders "*" for Any and {"AWS": ..., "Service": ...,
// "Federated": ...} otherwise.
func (p Principal) MarshalJSON() ([]byte, error) {
	if p.Any {
		return []byte(`"*"`), nil
	}
	m := make(map[string]StringOrSlice, 1+len(p.other))
	if len(p.AWS) > 0 {
		m["AWS"] = p.AWS
	}
	for k, v := range p.other {
		m[k] = v
	}
	return json.Marshal(m)
}

// UnmarshalJSON decodes a policy document, accepting "Statement" as either
// an array of statements or a single statement object (both are valid IAM
// policy syntax). Decoding is lenient (unknown fields are ignored);
// ParsePolicy / ParseBucketPolicy perform strict validation.
func (d *PolicyDocument) UnmarshalJSON(data []byte) error {
	var raw struct {
		Version   string          `json:"Version"`
		ID        string          `json:"Id"`
		Statement json.RawMessage `json:"Statement"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	stmts, err := decodeStatements(raw.Statement)
	if err != nil {
		return err
	}
	*d = PolicyDocument{Version: raw.Version, ID: raw.ID, Statement: stmts}
	return nil
}

// decodeStatements decodes a Statement value (array, single object or null).
func decodeStatements(raw json.RawMessage) ([]Statement, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	switch raw[0] {
	case '{':
		var st Statement
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, err
		}
		return []Statement{st}, nil
	case '[':
		var sts []Statement
		if err := json.Unmarshal(raw, &sts); err != nil {
			return nil, err
		}
		return sts, nil
	}
	return nil, errors.New("iam: Statement must be an object or an array")
}

// cloneDocument returns a deep copy of d.
func cloneDocument(d PolicyDocument) PolicyDocument {
	out := PolicyDocument{Version: d.Version, ID: d.ID}
	if d.Statement != nil {
		out.Statement = make([]Statement, len(d.Statement))
		for i := range d.Statement {
			out.Statement[i] = cloneStatement(&d.Statement[i])
		}
	}
	return out
}

func cloneStatement(s *Statement) Statement {
	out := Statement{
		Sid:          s.Sid,
		Effect:       s.Effect,
		Principal:    clonePrincipal(s.Principal),
		NotPrincipal: clonePrincipal(s.NotPrincipal),
		Action:       cloneSlice(s.Action),
		NotAction:    cloneSlice(s.NotAction),
		Resource:     cloneSlice(s.Resource),
		NotResource:  cloneSlice(s.NotResource),
	}
	if s.Condition != nil {
		out.Condition = make(map[string]map[string]StringOrSlice, len(s.Condition))
		for op, kv := range s.Condition {
			var m map[string]StringOrSlice
			if kv != nil {
				m = make(map[string]StringOrSlice, len(kv))
				for k, v := range kv {
					m[k] = cloneSlice(v)
				}
			}
			out.Condition[op] = m
		}
	}
	return out
}

func clonePrincipal(p *Principal) *Principal {
	if p == nil {
		return nil
	}
	out := &Principal{Any: p.Any, AWS: cloneSlice(p.AWS)}
	if p.other != nil {
		out.other = make(map[string]StringOrSlice, len(p.other))
		for k, v := range p.other {
			out.other[k] = cloneSlice(v)
		}
	}
	return out
}

func cloneSlice(s StringOrSlice) StringOrSlice {
	if s == nil {
		return nil
	}
	return append(StringOrSlice(make([]string, 0, len(s))), s...)
}
