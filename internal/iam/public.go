package iam

import (
	"net/netip"
	"strings"
)

// restrictingKeys are condition keys that, with fixed values, limit an
// Allow statement to known callers/networks (lower-cased). This mirrors the
// AWS definition used by GetBucketPolicyStatus.
var restrictingKeys = map[string]bool{
	"aws:sourceip":         true,
	"aws:sourcevpc":        true,
	"aws:sourcevpce":       true,
	"aws:principalorgid":   true,
	"aws:principalaccount": true,
	"aws:principalarn":     true,
	"aws:sourcearn":        true,
	"aws:sourceaccount":    true,
	"aws:userid":           true,
	"aws:username":         true,
}

// IsPublicPolicy reports whether doc grants access to everyone: some Allow
// statement applies to "*" (Principal "*" / {"AWS":"*"}, or a NotPrincipal
// that does not exclude "*") and has no restricting condition, i.e. no
// non-negated condition on aws:SourceIp (other than 0.0.0.0/0 or ::/0),
// aws:SourceVpc, aws:SourceVpce, aws:PrincipalOrgID, aws:PrincipalAccount,
// aws:PrincipalArn, aws:SourceArn, aws:SourceAccount, aws:userid or
// aws:username with fixed (wildcard-free) values. Used for
// GetBucketPolicyStatus and the admin API's "public" flags.
func IsPublicPolicy(doc *PolicyDocument) bool {
	if doc == nil {
		return false
	}
	for i := range doc.Statement {
		st := &doc.Statement[i]
		if st.Effect != "Allow" {
			continue
		}
		if !statementGrantsEveryone(st) {
			continue
		}
		if !hasRestrictingCondition(st.Condition) {
			return true
		}
	}
	return false
}

func statementGrantsEveryone(st *Statement) bool {
	switch {
	case st.Principal != nil:
		return principalIsEveryone(st.Principal)
	case st.NotPrincipal != nil:
		// "Everyone except X" is public unless it excludes everyone.
		return !principalIsEveryone(st.NotPrincipal)
	}
	return false
}

func principalIsEveryone(p *Principal) bool {
	if p.Any {
		return true
	}
	for _, v := range p.AWS {
		if v == "*" {
			return true
		}
	}
	return false
}

func hasRestrictingCondition(cond map[string]map[string]StringOrSlice) bool {
	for opName, kv := range cond {
		op, ok := parseCondOp(opName)
		if !ok || op.negated || op.kind == opNull || op.ifExists {
			continue
		}
		for key, vals := range kv {
			lk := strings.ToLower(key)
			if !restrictingKeys[lk] || len(vals) == 0 {
				continue
			}
			restricting := true
			for _, v := range vals {
				if !fixedValue(lk, v) {
					restricting = false
					break
				}
			}
			if restricting {
				return true
			}
		}
	}
	return false
}

// fixedValue reports whether a condition value names a specific caller or
// network rather than "anything".
func fixedValue(key, v string) bool {
	if v == "" || strings.ContainsAny(v, "*?") || strings.Contains(v, "${") {
		return false
	}
	if key == "aws:sourceip" {
		if p, err := netip.ParsePrefix(strings.TrimSpace(v)); err == nil && p.Bits() == 0 {
			return false
		}
	}
	return true
}
