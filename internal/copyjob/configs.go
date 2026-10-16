package copyjob

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// safeConfigs are the configuration documents "configs": "safe" copies.
// Encryption provider and object lock settings are copied only where the
// target supports them, the policy only with its ARNs rewritten, and the
// lifecycle only by copies (see configKeys).
var safeConfigs = []string{
	storage.ConfigCORS, storage.ConfigTagging, storage.ConfigEncryption, storage.ConfigWebsite,
	storage.ConfigOwnership, storage.ConfigPublicAccessBlock, storage.ConfigACL, storage.ConfigCompression,
	storage.ConfigEncryptionProvider, storage.ConfigObjectLock, storage.ConfigPolicy, storage.ConfigLifecycle,
}

// aclGuardConfigs are the configuration documents that can keep object
// ACLs from granting access: a public access block ignoring public ACLs,
// and ownership controls disabling ACLs. Copied versions keep their ACLs,
// so a source's guard is copied whatever "configs" says (see aclGuard).
var aclGuardConfigs = []string{storage.ConfigPublicAccessBlock, storage.ConfigOwnership}

// configKeys returns the configuration documents the job copies. Never
// copied: replication (it names the source's destinations) and the restic
// snapshot schedule (snapshots belong to the source), and for mirrors the
// lifecycle, whose expirations would be applied twice. The ACL guards are
// always considered (see syncConfigs).
func configKeys(sp *Spec) []string {
	var keys []string
	switch sp.Options.Configs {
	case ConfigsSafe:
		keys = slices.Clone(safeConfigs)
	case ConfigsAll:
		keys = slices.Clone(storage.AllConfigKeys)
	}
	keys = slices.DeleteFunc(keys, func(k string) bool {
		return k == storage.ConfigReplication || k == storage.ConfigResticSnapshots ||
			k == storage.ConfigLifecycle && sp.mirror()
	})
	for _, k := range aclGuardConfigs {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// selected reports whether the spec's "configs" option selects key (the
// ACL guards are copied regardless when they guard, see aclGuard).
func selected(sp *Spec, key string) bool {
	switch sp.Options.Configs {
	case ConfigsSafe:
		return slices.Contains(safeConfigs, key)
	case ConfigsAll:
		return true
	}
	return false
}

// aclGuard reports whether configuration document data (of key) keeps
// object ACLs from granting access, as the S3 layer applies it: a public
// access block with IgnorePublicAcls (an unreadable one counts, as it is
// applied as the most restrictive), ownership controls with
// BucketOwnerEnforced.
func aclGuard(key string, data []byte) bool {
	if len(data) == 0 {
		return false
	}
	switch key {
	case storage.ConfigPublicAccessBlock:
		var pab struct {
			IgnorePublicAcls bool `xml:"IgnorePublicAcls"`
		}
		if xml.Unmarshal(data, &pab) != nil {
			return true
		}
		return pab.IgnorePublicAcls
	case storage.ConfigOwnership:
		var oc struct {
			Rules []struct {
				ObjectOwnership string `xml:"ObjectOwnership"`
			} `xml:"Rule"`
		}
		if xml.Unmarshal(data, &oc) != nil {
			return bytes.Contains(data, []byte("BucketOwnerEnforced"))
		}
		for _, r := range oc.Rules {
			if strings.TrimSpace(r.ObjectOwnership) == "BucketOwnerEnforced" {
				return true
			}
		}
	}
	return false
}

// syncConfigs copies the source's configuration documents to the target
// (configKeys). A mirror also removes from the target the documents it
// copied earlier that the source no longer has; documents the job did not
// copy are never deleted. copied lists the documents the job copied
// before (kept in the checkpoint); syncConfigs returns warnings for the
// documents it skipped and the updated list.
//
// The ACL guards (aclGuardConfigs) of the source are copied even when
// "configs" does not select them, unless the target is guarded the same
// way already, since the copied versions keep their ACLs; a target that
// cannot store them fails the job.
func (r *run) syncConfigs(ctx context.Context, copied []string) ([]string, []string, error) {
	src, dst := r.sp.Source.Bucket, r.sp.Target.Bucket
	var warnings []string
	forget := func(key string) { copied = slices.DeleteFunc(copied, func(k string) bool { return k == key }) }
	// drop deletes a document the job copied before from the target.
	drop := func(key string, cur []byte) error {
		if cur != nil && slices.Contains(copied, key) {
			if err := r.s.router.DeleteBucketConfig(ctx, dst, key); err != nil {
				return fmt.Errorf("deleting the target's %s configuration: %w", key, err)
			}
		}
		forget(key)
		return nil
	}
	for _, key := range configKeys(&r.sp) {
		data, err := getConfig(ctx, r.s.router, src, key)
		if err != nil {
			return warnings, copied, fmt.Errorf("reading the source's %s configuration: %w", key, err)
		}
		cur, err := getConfig(ctx, r.s.router, dst, key)
		if err != nil {
			return warnings, copied, fmt.Errorf("reading the target's %s configuration: %w", key, err)
		}
		guard := aclGuard(key, data)
		if !selected(&r.sp, key) {
			// An ACL guard that configs does not select.
			if !guard || aclGuard(key, cur) {
				if r.sp.mirror() && !guard {
					if err := drop(key, cur); err != nil {
						return warnings, copied, err
					}
				}
				continue
			}
		}
		if data == nil {
			if r.sp.mirror() {
				if err := drop(key, cur); err != nil {
					return warnings, copied, err
				}
			}
			forget(key)
			continue
		}
		switch key {
		case storage.ConfigPolicy:
			var reason string
			if data, reason = rewritePolicy(data, &r.sp); reason != "" {
				warning := "the bucket policy is not copied: " + reason
				if cur != nil && slices.Contains(copied, key) {
					// Never leave a policy copied from an older source
					// policy in place.
					warning += "; the policy copied earlier was deleted from the target"
					if err := drop(key, cur); err != nil {
						return warnings, copied, err
					}
				}
				warnings = append(warnings, warning)
				continue
			}
		case storage.ConfigEncryptionProvider:
			set, err := storage.ParseEncryptionSettings(data)
			if err != nil {
				warnings = append(warnings, "the encryption provider setting is not copied: "+err.Error())
				continue
			}
			if err := r.s.router.ValidateEncryptionSettings(ctx, dst, set); err != nil {
				if !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, storage.ErrNotSupported) {
					return warnings, copied, fmt.Errorf("checking the encryption provider %s on the target: %w", set.Provider, err)
				}
				warnings = append(warnings, fmt.Sprintf("the encryption provider setting (%s) is not copied: the target cannot use it", set.Provider))
				continue
			}
		case storage.ConfigObjectLock:
			if !r.in.dstLock() {
				warnings = append(warnings, "the object lock configuration is not copied: the target has no object lock")
				continue
			}
		}
		if !bytes.Equal(cur, data) {
			if err := r.s.router.PutBucketConfig(ctx, dst, key, data); err != nil {
				unsupported := errors.Is(err, storage.ErrNotSupported) || errors.Is(err, errors.ErrUnsupported)
				switch {
				case unsupported && guard:
					return warnings, copied, jobs.Permanent(fmt.Errorf("the target cannot store the source's %s configuration, "+
						"which keeps the object ACLs the copies carry from granting access: %w", key, err))
				case unsupported:
					warnings = append(warnings, fmt.Sprintf("the %s configuration is not copied: the target does not support it", key))
					continue
				}
				return warnings, copied, fmt.Errorf("storing the target's %s configuration: %w", key, err)
			}
		}
		if !slices.Contains(copied, key) {
			copied = append(copied, key)
		}
	}
	slices.Sort(copied)
	return warnings, copied, nil
}

// getConfig returns a bucket's configuration document (nil when unset).
func getConfig(ctx context.Context, b storage.Backend, bucket, key string) ([]byte, error) {
	data, err := b.GetBucketConfig(ctx, bucket, key)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return nil, nil
	}
	return data, err
}

// s3ARN is the prefix of the ARNs of S3 buckets and objects.
const s3ARN = "arn:aws:s3:::"

// rewritePolicy returns the source's bucket policy data made for the
// target, or the reason it cannot be copied ("" when it can). The result
// must pass iam.ParseBucketPolicy for the target bucket, and never grants
// more on the target than the source's policy grants on the source keys
// the target keys are copies of:
//
//   - without prefixes, the source bucket's ARNs (arn:aws:s3:::<src> and
//     arn:aws:s3:::<src>/…) become the target's;
//   - with a source or target prefix (rewriteScoped), only object ARNs
//     that cover copied keys are kept, mapped from the source prefix to the
//     target prefix; an Allow keeps nothing broader (bucket ARNs and
//     patterns that partly cover the copied keys are dropped, statements
//     left without resources too), a Deny may get broader (it then covers
//     every copied key). Policies whose meaning cannot be mapped (s3:prefix
//     conditions, NotResource) are not copied.
func rewritePolicy(data []byte, sp *Spec) ([]byte, string) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, "it is not a JSON object"
	}
	var stmts []any
	single := false
	switch v := doc["Statement"].(type) {
	case []any:
		stmts = v
	case map[string]any:
		stmts, single = []any{v}, true
	}
	if sp.Source.Prefix == "" && sp.Target.Prefix == "" {
		rewrite := func(s string) (string, bool) {
			if s == s3ARN+sp.Source.Bucket {
				return s3ARN + sp.Target.Bucket, true
			}
			if rest, ok := strings.CutPrefix(s, s3ARN+sp.Source.Bucket+"/"); ok {
				return s3ARN + sp.Target.Bucket + "/" + rest, true
			}
			return s, true
		}
		for _, s := range stmts {
			if st, ok := s.(map[string]any); ok {
				mapResources(st, "Resource", rewrite)
				mapResources(st, "NotResource", rewrite)
			}
		}
	} else {
		var reason string
		if stmts, reason = rewriteScoped(stmts, sp); reason != "" {
			return nil, reason
		}
		if len(stmts) == 0 {
			return nil, "none of its statements applies to the copied keys alone"
		}
		single = false
	}
	if single {
		doc["Statement"] = stmts[0]
	} else {
		doc["Statement"] = stmts
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err.Error()
	}
	if _, err := iam.ParseBucketPolicy(out, sp.Target.Bucket); err != nil {
		return nil, err.Error()
	}
	return out, ""
}

// rewriteScoped maps a policy's statements for a copy with prefixes (see
// rewritePolicy) and returns those that remain.
func rewriteScoped(stmts []any, sp *Spec) ([]any, string) {
	srcObj := s3ARN + sp.Source.Bucket + "/"
	dstAll := s3ARN + sp.Target.Bucket + "/" + sp.Target.Prefix + "*"
	var out []any
	for _, s := range stmts {
		st, ok := s.(map[string]any)
		if !ok {
			return nil, "a statement is not a JSON object"
		}
		if _, ok := st["NotResource"]; ok {
			return nil, "a statement uses NotResource, which cannot be limited to the copied keys"
		}
		if usesPrefixCondition(st["Condition"]) {
			return nil, "a statement has an s3:prefix condition, which cannot be mapped to the target prefix"
		}
		allow := !strings.EqualFold(fmt.Sprint(st["Effect"]), "Deny")
		keep := mapResources(st, "Resource", func(arn string) (string, bool) {
			path, isObj := strings.CutPrefix(arn, srcObj)
			switch {
			case arn == s3ARN+sp.Source.Bucket:
				// The whole bucket: the target bucket holds other keys.
				if allow {
					return "", false
				}
				return s3ARN + sp.Target.Bucket, true
			case !isObj:
				return arn, true // another bucket's: refused by validation
			}
			lit := path
			if i := strings.IndexAny(path, "*?"); i >= 0 {
				lit = path[:i]
			}
			switch {
			case strings.HasPrefix(path, sp.Source.Prefix) && strings.HasPrefix(lit, sp.Source.Prefix):
				// Within the copied keys: exactly their copies.
				return s3ARN + sp.Target.Bucket + "/" + sp.Target.Prefix + path[len(sp.Source.Prefix):], true
			case lit == path || !strings.HasPrefix(sp.Source.Prefix, lit):
				// Disjoint from the copied keys (one key outside them, or a
				// pattern whose fixed start excludes them).
				return "", false
			case path == lit+"*":
				// Every copied key (and others the copy does not have).
				return dstAll, true
			case allow:
				// Some copied keys only: an Allow may not grow.
				return "", false
			}
			return dstAll, true // a Deny may
		})
		if keep {
			out = append(out, st)
		}
	}
	return out, ""
}

// mapResources rewrites the ARNs of a statement's field (a string or a
// list) with f, dropping those for which f returns false. It reports
// whether the field is left with an ARN (or was absent).
func mapResources(st map[string]any, field string, f func(string) (string, bool)) bool {
	var list []any
	switch v := st[field].(type) {
	case nil:
		return true
	case string:
		list = []any{v}
	case []any:
		list = v
	default:
		return true
	}
	var out []any
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			out = append(out, e)
			continue
		}
		if m, keep := f(s); keep && !slices.Contains(out, any(m)) {
			out = append(out, m)
		}
	}
	switch len(out) {
	case 0:
		delete(st, field)
		return false
	case 1:
		if _, isString := st[field].(string); isString {
			st[field] = out[0]
			return true
		}
	}
	st[field] = out
	return true
}

// usesPrefixCondition reports whether a statement's Condition tests the
// s3:prefix key (condition keys are case-insensitive).
func usesPrefixCondition(cond any) bool {
	ops, ok := cond.(map[string]any)
	if !ok {
		return false
	}
	for _, kv := range ops {
		keys, ok := kv.(map[string]any)
		if !ok {
			continue
		}
		for k := range keys {
			if strings.EqualFold(k, "s3:prefix") {
				return true
			}
		}
	}
	return false
}

// syncVersioning makes the target keep versions as the copy needs: a
// target that imports versions follows the source's versioning (never
// back to unversioned; an existing target the job did not create only
// leaves "unversioned", unless the job is a mirror), and a target written
// with plain writes has versioning enabled when every version is copied.
func (r *run) syncVersioning(ctx context.Context) error {
	if !r.sp.all() {
		return nil
	}
	have := r.in.dstInfo.Versioning
	want := have
	switch src := r.in.srcInfo.Versioning; {
	case !r.in.dstCaps.Import:
		want = storage.VersioningEnabled
	case src == storage.VersioningUnversioned:
	case have == storage.VersioningUnversioned || r.created() || r.sp.mirror():
		want = src
	}
	if want == have || want == storage.VersioningSuspended && r.in.dstInfo.ObjectLockEnabled {
		// Object lock keeps versioning enabled.
		return nil
	}
	if err := r.s.router.SetBucketVersioning(ctx, r.sp.Target.Bucket, want); err != nil {
		return fmt.Errorf("setting the versioning of the target bucket %q to %s: %w", r.sp.Target.Bucket, want, err)
	}
	r.in.dstInfo.Versioning = want
	return nil
}
