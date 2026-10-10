package iam

import (
	"math"
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
// non-negated condition on aws:SourceIp (whose ranges together cover at
// most four IPv4 /8 and four IPv6 /32 networks),
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
			if restricting && lk == "aws:sourceip" {
				restricting = narrowSourceIPs(vals)
			}
			if restricting {
				return true
			}
		}
	}
	return false
}

// Limits on the address space an aws:SourceIp condition may cover and still
// count as restricting: four IPv4 /8 networks and four IPv6 /32 networks.
// These are generous for real allow-lists (all RFC 1918 ranges together
// are about one /8) but keep a policy that lists many wide ranges, which
// together admit (nearly) every client, public.
const (
	maxRestrictingIPv4 = float64(1 << 26)
	maxRestrictingIPv6 = float64(1<<62) * float64(1<<36) // 2^98
)

// narrowSourceIPs reports whether the aws:SourceIp values together cover a
// bounded part of the address space. Values are normalised exactly as the
// evaluator does (parsePrefix maps ::ffff:a.b.c.d/96+n to the IPv4 /n), and
// overlapping ranges are counted twice, which errs towards "public".
func narrowSourceIPs(vals []string) bool {
	var v4, v6 float64
	for _, v := range vals {
		p, ok := parsePrefix(v)
		if !ok {
			return false
		}
		size := math.Ldexp(1, p.Addr().BitLen()-p.Bits())
		if p.Addr().Is4() {
			v4 += size
		} else {
			v6 += size
		}
	}
	return v4 <= maxRestrictingIPv4 && v6 <= maxRestrictingIPv6
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
