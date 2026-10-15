package storage

import "context"

// governanceBypassKey is the context key of WithGovernanceBypass.
type governanceBypassKey struct{}

// WithGovernanceBypass returns a context telling the backend that the S3
// layer approved bypassing GOVERNANCE-mode retention for the operation (the
// request carried x-amz-bypass-governance-retention: true and the caller
// holds s3:BypassGovernanceRetention). Backends that enforce retention
// themselves (a native bucket's remote service) pass the bypass on; the
// others ignore it, as the S3 layer has already enforced retention.
//
// Only the S3 layer sets it, and only after its own checks: the lifecycle
// worker and other internal callers never do.
func WithGovernanceBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, governanceBypassKey{}, true)
}

// GovernanceBypass reports whether ctx carries an approved GOVERNANCE
// bypass (see WithGovernanceBypass).
func GovernanceBypass(ctx context.Context) bool {
	v, _ := ctx.Value(governanceBypassKey{}).(bool)
	return v
}
