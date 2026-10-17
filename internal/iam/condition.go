package iam

import (
	"bytes"
	"encoding/base64"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// opKind is the comparison family of a condition operator.
type opKind uint8

const (
	opStringEquals opKind = iota + 1
	opStringEqualsIgnoreCase
	opStringLike
	opNumericEquals
	opNumericLessThan
	opNumericLessThanEquals
	opNumericGreaterThan
	opNumericGreaterThanEquals
	opDateEquals
	opDateLessThan
	opDateLessThanEquals
	opDateGreaterThan
	opDateGreaterThanEquals
	opBool
	opIPAddress
	opNull
	opArnLike
	opBinaryEquals
)

// setKind is the multi-value set operator prefix.
type setKind uint8

const (
	setNone setKind = iota
	setForAnyValue
	setForAllValues
)

type opDef struct {
	kind    opKind
	negated bool
}

// condOps maps lower-cased base operator names to their definition.
var condOps = map[string]opDef{
	"stringequals":              {opStringEquals, false},
	"stringnotequals":           {opStringEquals, true},
	"stringequalsignorecase":    {opStringEqualsIgnoreCase, false},
	"stringnotequalsignorecase": {opStringEqualsIgnoreCase, true},
	"stringlike":                {opStringLike, false},
	"stringnotlike":             {opStringLike, true},
	"numericequals":             {opNumericEquals, false},
	"numericnotequals":          {opNumericEquals, true},
	"numericlessthan":           {opNumericLessThan, false},
	"numericlessthanequals":     {opNumericLessThanEquals, false},
	"numericgreaterthan":        {opNumericGreaterThan, false},
	"numericgreaterthanequals":  {opNumericGreaterThanEquals, false},
	"dateequals":                {opDateEquals, false},
	"datenotequals":             {opDateEquals, true},
	"datelessthan":              {opDateLessThan, false},
	"datelessthanequals":        {opDateLessThanEquals, false},
	"dategreaterthan":           {opDateGreaterThan, false},
	"dategreaterthanequals":     {opDateGreaterThanEquals, false},
	"bool":                      {opBool, false},
	"ipaddress":                 {opIPAddress, false},
	"notipaddress":              {opIPAddress, true},
	"null":                      {opNull, false},
	"arnequals":                 {opArnLike, false},
	"arnlike":                   {opArnLike, false},
	"arnnotequals":              {opArnLike, true},
	"arnnotlike":                {opArnLike, true},
	"binaryequals":              {opBinaryEquals, false},
}

// condOp is a parsed condition operator such as "ForAnyValue:StringLikeIfExists".
type condOp struct {
	opDef
	set      setKind
	ifExists bool
}

// parseCondOp parses an operator name (case-insensitively).
func parseCondOp(name string) (condOp, bool) {
	var op condOp
	l := strings.ToLower(name)
	if rest, ok := strings.CutPrefix(l, "foranyvalue:"); ok {
		op.set, l = setForAnyValue, rest
	} else if rest, ok := strings.CutPrefix(l, "forallvalues:"); ok {
		op.set, l = setForAllValues, rest
	}
	def, ok := condOps[l]
	if !ok {
		base, found := strings.CutSuffix(l, "ifexists")
		if !found {
			return op, false
		}
		def, ok = condOps[base]
		if !ok || def.kind == opNull {
			return op, false
		}
		op.ifExists = true
	}
	op.opDef = def
	return op, true
}

// validateConditions validates a Condition block.
func validateConditions(cond map[string]map[string]StringOrSlice) string {
	for opName, kv := range cond {
		op, ok := parseCondOp(opName)
		if !ok {
			return "Invalid Condition type : " + opName
		}
		if len(kv) == 0 {
			return "Condition " + opName + " must contain at least one condition key"
		}
		for key, vals := range kv {
			if !validConditionKey(key) {
				return "Policy has an invalid condition key: " + key
			}
			if len(vals) == 0 {
				return "Condition " + opName + " for key " + key + " must have at least one value"
			}
			for _, v := range vals {
				if !validConditionValue(op.kind, v) {
					return "Invalid Condition value for " + opName + " " + key + ": " + v
				}
			}
		}
	}
	return ""
}

// validConditionKey requires "<prefix>:<name>" with a printable name.
func validConditionKey(k string) bool {
	if len(k) > 512 {
		return false
	}
	prefix, name, ok := strings.Cut(k, ":")
	if !ok || prefix == "" || name == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] == 0x7f {
			return false
		}
	}
	return true
}

func validConditionValue(kind opKind, v string) bool {
	if strings.Contains(v, "${") {
		return true // resolved at evaluation time
	}
	switch kind {
	case opNumericEquals, opNumericLessThan, opNumericLessThanEquals, opNumericGreaterThan, opNumericGreaterThanEquals:
		_, ok := parseNumber(v)
		return ok
	case opDateEquals, opDateLessThan, opDateLessThanEquals, opDateGreaterThan, opDateGreaterThanEquals:
		_, ok := parseDate(v)
		return ok
	case opBool, opNull:
		return strings.EqualFold(v, "true") || strings.EqualFold(v, "false")
	case opIPAddress:
		_, ok := parsePrefix(v)
		return ok
	case opBinaryEquals:
		_, err := base64.StdEncoding.DecodeString(v)
		return err == nil
	}
	return true
}

// evalConditions reports whether all operators of a Condition block are
// satisfied. Unknown operators (only possible for documents that bypassed
// validation) are treated as satisfied for Deny statements and unsatisfied
// for Allow statements, so they always fail closed.
func (c *evalCtx) evalConditions(cond map[string]map[string]StringOrSlice, deny bool) bool {
	for opName, kv := range cond {
		op, ok := parseCondOp(opName)
		if !ok {
			if deny {
				continue
			}
			return false
		}
		for key, vals := range kv {
			ctxVals, present := c.lookup(key)
			if !c.evalKey(op, vals, ctxVals, present) {
				return false
			}
		}
	}
	return true
}

// evalKey evaluates one operator/key pair following AWS semantics.
func (c *evalCtx) evalKey(op condOp, policyVals []string, ctxVals []string, present bool) bool {
	if op.kind == opNull {
		// "true": the key must be absent; "false": it must be present.
		for _, pv := range policyVals {
			if strings.EqualFold(pv, "true") != present {
				return true
			}
		}
		return false
	}
	if !present {
		switch {
		case op.ifExists:
			return true
		case op.set == setForAllValues:
			return true // vacuously true for an empty set
		case op.set == setForAnyValue:
			return false
		default:
			return op.negated
		}
	}
	pvs := c.expandPolicyValues(op.kind, policyVals)
	matchOne := func(cv string) bool {
		for _, pv := range pvs {
			if c.matchCondValue(op.kind, pv, cv) {
				return true
			}
		}
		return false
	}
	switch op.set {
	case setForAllValues:
		for _, cv := range ctxVals {
			if matchOne(cv) == op.negated {
				return false
			}
		}
		return true
	case setForAnyValue:
		for _, cv := range ctxVals {
			if matchOne(cv) != op.negated {
				return true
			}
		}
		return false
	default:
		matched := false
		for _, cv := range ctxVals {
			if matchOne(cv) {
				matched = true
				break
			}
		}
		return matched != op.negated
	}
}

// condValue is a condition value from a policy, after variable expansion.
// escaped marks backslash-escaped glob patterns (see expandVars).
type condValue struct {
	s       string
	escaped bool
}

// expandPolicyValues substitutes policy variables in condition values.
// Values whose variables cannot be resolved (and have no ${key, 'default'})
// are dropped: as on AWS such a value is effectively null, so it equals or
// is like no request value. Positive operators therefore do not match it,
// while negated operators (StringNotEquals, StringNotLike, ArnNotLike, ...)
// do, because the request value is not equal to or like null. This is
// AWS's documented behaviour ("Policy variables with no value",
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_variables.html#policy-vars-no-value):
// it makes a Deny with StringNotEquals against ${aws:PrincipalTag/x} deny
// callers without the tag, but also makes an Allow with a negated operator
// against such a variable grant callers for whom it is unresolved (an
// anonymous caller has no aws:username). Write such Allow statements with
// a positive operator, or guard them with a Null condition on the variable.
func (c *evalCtx) expandPolicyValues(kind opKind, vals []string) []condValue {
	pattern := kind == opStringLike || kind == opArnLike
	out := make([]condValue, 0, len(vals))
	for _, v := range vals {
		if !strings.Contains(v, "${") {
			out = append(out, condValue{s: v})
			continue
		}
		if e, ok := expandVars(v, c.lookupVar, pattern, c.budget); ok {
			out = append(out, condValue{s: e, escaped: pattern})
		}
	}
	return out
}

// matchCondValue applies the positive form of an operator to a policy value
// pv and a request context value cv.
func (c *evalCtx) matchCondValue(kind opKind, v condValue, cv string) bool {
	pv := v.s
	switch kind {
	case opStringEquals:
		return pv == cv
	case opStringEqualsIgnoreCase:
		return strings.EqualFold(pv, cv)
	case opStringLike:
		return globMatchBudget(pv, cv, false, v.escaped, c.budget)
	case opNumericEquals, opNumericLessThan, opNumericLessThanEquals, opNumericGreaterThan, opNumericGreaterThanEquals:
		p, ok1 := parseNumber(pv)
		v, ok2 := parseNumber(cv)
		if !ok1 || !ok2 {
			return false
		}
		switch kind {
		case opNumericEquals:
			return v == p
		case opNumericLessThan:
			return v < p
		case opNumericLessThanEquals:
			return v <= p
		case opNumericGreaterThan:
			return v > p
		default:
			return v >= p
		}
	case opDateEquals, opDateLessThan, opDateLessThanEquals, opDateGreaterThan, opDateGreaterThanEquals:
		p, ok1 := parseDate(pv)
		v, ok2 := parseDate(cv)
		if !ok1 || !ok2 {
			return false
		}
		switch kind {
		case opDateEquals:
			return v.Equal(p)
		case opDateLessThan:
			return v.Before(p)
		case opDateLessThanEquals:
			return !v.After(p)
		case opDateGreaterThan:
			return v.After(p)
		default:
			return !v.Before(p)
		}
	case opBool:
		return strings.EqualFold(pv, cv)
	case opIPAddress:
		pfx, ok := parsePrefix(pv)
		if !ok {
			return false
		}
		addr, ok := parseAddr(cv)
		return ok && pfx.Contains(addr)
	case opArnLike:
		return arnMatch(pv, cv, v.escaped, c.budget)
	case opBinaryEquals:
		want, err := base64.StdEncoding.DecodeString(pv)
		if err != nil {
			return false
		}
		if got, err := base64.StdEncoding.DecodeString(cv); err == nil {
			return bytes.Equal(want, got)
		}
		return bytes.Equal(want, []byte(cv))
	}
	return false
}

// arnMatch matches ARNs component-wise: wildcards in one of the first five
// components never cross a ':' boundary.
func arnMatch(pattern, arn string, escaped bool, b *budget) bool {
	pp := strings.SplitN(pattern, ":", 6)
	ap := strings.SplitN(arn, ":", 6)
	if len(pp) != 6 || len(ap) != 6 {
		return false
	}
	for i := range pp {
		if !globMatchBudget(pp[i], ap[i], false, escaped, b) {
			return false
		}
	}
	return true
}

func parseNumber(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

var dateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05.999999999Z0700",
	"20060102T150405Z", // ISO 8601 basic format (x-amz-date)
	"2006-01-02T15:04:05",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02",
	time.RFC1123,
	time.RFC1123Z,
}

// parseDate accepts ISO 8601 / RFC 3339 variants, HTTP dates and epoch
// seconds (as AWS does for Date operators).
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < -1e11 || n > 1e11 {
			return time.Time{}, false
		}
		return time.Unix(n, 0).UTC(), true
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parsePrefix parses an IpAddress condition value: a CIDR or a single address.
func parsePrefix(s string) (netip.Prefix, bool) {
	s = strings.TrimSpace(s)
	if strings.IndexByte(s, '/') >= 0 {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, false
		}
		p = p.Masked()
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return p, true
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, false
	}
	a = a.Unmap().WithZone("")
	return netip.PrefixFrom(a, a.BitLen()), true
}

// parseAddr parses a request source address ("ip" or "ip:port").
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	a, err := netip.ParseAddr(s)
	if err != nil {
		ap, err2 := netip.ParseAddrPort(s)
		if err2 != nil {
			return netip.Addr{}, false
		}
		a = ap.Addr()
	}
	return a.Unmap().WithZone(""), true
}
