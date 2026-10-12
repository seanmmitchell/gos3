package iam

import (
	"math"
	"net/netip"
	"slices"
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
// non-negated, non-IfExists, non-ForAllValues condition on aws:SourceIp,
// aws:SourceVpc, aws:SourceVpce, aws:PrincipalOrgID, aws:PrincipalAccount,
// aws:PrincipalArn, aws:SourceArn, aws:SourceAccount, aws:userid or
// aws:username with fixed (wildcard-free) values. ForAllValues conditions
// never restrict (they hold when the key is missing, as it is for
// anonymous callers). Statements restricted only by aws:SourceIp are judged
// together: the union of their ranges over the whole policy may cover at
// most four IPv4 /8 and four IPv6 /32 networks, so splitting a wide range
// across statements does not hide it. Used for GetBucketPolicyStatus,
// BlockPublicPolicy, RestrictPublicBuckets and the admin API's "public"
// flags.
func IsPublicPolicy(doc *PolicyDocument) bool {
	if doc == nil {
		return false
	}
	var ipOnly []netip.Prefix // ranges of statements restricted only by aws:SourceIp
	for i := range doc.Statement {
		st := &doc.Statement[i]
		if st.Effect != "Allow" {
			continue
		}
		if !statementGrantsEveryone(st) {
			continue
		}
		restricted, ips := restrictingCondition(st.Condition)
		switch {
		case restricted:
		case ips == nil:
			return true
		default:
			ipOnly = append(ipOnly, ips...)
		}
	}
	return !narrowPrefixes(ipOnly)
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

// restrictingCondition inspects a statement's Condition block. restricted
// is true when a key other than aws:SourceIp limits the statement to known
// callers. Otherwise ips is the narrowest set of aws:SourceIp ranges the
// statement is limited to (nil when it is not limited at all).
func restrictingCondition(cond map[string]map[string]StringOrSlice) (restricted bool, ips []netip.Prefix) {
	var best float64
	for opName, kv := range cond {
		op, ok := parseCondOp(opName)
		if !ok || op.negated || op.kind == opNull || op.ifExists || op.set == setForAllValues {
			continue
		}
		for key, vals := range kv {
			lk := strings.ToLower(key)
			if !restrictingKeys[lk] || len(vals) == 0 {
				continue
			}
			fixed := true
			for _, v := range vals {
				if !fixedValue(lk, v) {
					fixed = false
					break
				}
			}
			if !fixed {
				continue
			}
			if lk != "aws:sourceip" {
				return true, nil
			}
			ps, ok := sourceIPPrefixes(vals)
			if !ok {
				continue
			}
			if size := prefixesSize(ps); ips == nil || size < best {
				ips, best = ps, size
			}
		}
	}
	return false, ips
}

// Limits on the address space that aws:SourceIp conditions may cover and
// still count as restricting: four IPv4 /8 networks and four IPv6 /32
// networks. These are generous for real allow-lists (all RFC 1918 ranges
// together are about one /8) but keep a policy that lists many wide
// ranges, which together admit (nearly) every client, public.
const (
	maxRestrictingIPv4 = float64(1 << 26)
	maxRestrictingIPv6 = float64(1<<62) * float64(1<<36) // 2^98
)

// sourceIPPrefixes parses aws:SourceIp values exactly as the evaluator
// does (parsePrefix maps ::ffff:a.b.c.d/96+n to the IPv4 /n).
func sourceIPPrefixes(vals []string) ([]netip.Prefix, bool) {
	out := make([]netip.Prefix, 0, len(vals))
	for _, v := range vals {
		p, ok := parsePrefix(v)
		if !ok {
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}

// prefixesSize returns the number of addresses of ps (overlaps counted
// twice); used only to pick the narrowest condition of a statement.
func prefixesSize(ps []netip.Prefix) float64 {
	var n float64
	for _, p := range ps {
		n += math.Ldexp(1, p.Addr().BitLen()-p.Bits())
	}
	return n
}

// narrowPrefixes reports whether the union of ps covers a bounded part of
// the IPv4 and IPv6 address spaces (see maxRestrictingIPv4/IPv6).
func narrowPrefixes(ps []netip.Prefix) bool {
	if len(ps) == 0 {
		return true
	}
	ps = slices.Clone(ps)
	for i, p := range ps {
		ps[i] = p.Masked()
	}
	slices.SortFunc(ps, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	// CIDR prefixes either nest or are disjoint; after sorting, a prefix
	// contained in an earlier one directly follows its container (or
	// other prefixes it contains), so only the last kept prefix matters.
	var v4, v6 float64
	var last netip.Prefix
	for _, p := range ps {
		if last.IsValid() && last.Addr().BitLen() == p.Addr().BitLen() && last.Bits() <= p.Bits() && last.Contains(p.Addr()) {
			continue
		}
		last = p
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
