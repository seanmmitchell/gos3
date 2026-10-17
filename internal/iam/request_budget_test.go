package iam

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestRequestBudgetAcrossDecisions(t *testing.T) {
	m := newEvalManager(t)
	ctx := WithAuthorizationBudget(context.Background())
	if WithAuthorizationBudget(ctx) != ctx {
		t.Fatal("nested call replaced the request budget")
	}
	shared := ctx.Value(requestBudgetKey{}).(*requestBudget)
	shared.b.glob = 100
	req := AuthzRequest{Identity: anon, Action: "s3:GetObject", Bucket: "b", Key: "k",
		BucketPolicy: mustBucketPolicy(t, "b", `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)}
	denied := false
	for i := 0; i < 1000; i++ {
		d := m.Authorize(ctx, req)
		if !d.Allowed {
			if !d.ExplicitDeny || !strings.Contains(d.Reason, "limit exceeded") {
				t.Fatalf("unexpected denial: %+v", d)
			}
			denied = true
			break
		}
	}
	if !denied {
		t.Fatal("1000 decisions reset the aggregate work budget")
	}
	// Exhaustion cannot be bypassed through ACL fallback or an empty policy.
	req.BucketPolicy, req.ACL = nil, ACLPublicRead
	if d := m.Authorize(ctx, req); d.Allowed || !d.ExplicitDeny {
		t.Fatalf("ACL bypassed exhausted budget: %+v", d)
	}
	if d := m.Authorize(WithAuthorizationBudget(context.Background()), req); !d.Allowed {
		t.Fatalf("independent request inherited exhaustion: %+v", d)
	}
}

func TestRequestBudgetConcurrent(t *testing.T) {
	m := newEvalManager(t)
	ctx := WithAuthorizationBudget(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			m.Authorize(ctx, AuthzRequest{Identity: anon, Action: "s3:GetObject", ACL: ACLPublicRead})
		})
	}
	wg.Wait()
}
