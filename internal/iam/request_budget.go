package iam

import (
	"context"
	"sync"
)

type requestBudgetKey struct{}

type requestBudget struct {
	mu sync.Mutex
	b  *budget
}

// WithAuthorizationBudget shares the authorization work limit across all
// decisions in one HTTP request, including bulk operations and ACL retries.
// Calling it again preserves the existing budget. Concurrent decisions are
// serialized so they cannot race or spend the same remaining work twice.
func WithAuthorizationBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(requestBudgetKey{}).(*requestBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, requestBudgetKey{}, &requestBudget{b: newBudget()})
}
