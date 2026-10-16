package iam

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

// rawBucketPolicy decodes a bucket policy without validating it, the way a
// document stored before a validation rule existed would be evaluated.
func rawBucketPolicy(t *testing.T, s string) *PolicyDocument {
	t.Helper()
	var d PolicyDocument
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

// TestVarRefLimitValidation: documents with more than maxVarRefs variable
// references in one value are refused (H-02).
func TestVarRefLimitValidation(t *testing.T) {
	many := strings.Repeat("${aws:UserAgent}", maxVarRefs+1)
	ok := strings.Repeat("${aws:UserAgent}", maxVarRefs)
	bp := func(v string) string {
		return `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*",` +
			`"Condition":{"StringEquals":{"aws:UserAgent":"` + v + `"}}}]}`
	}
	if _, err := ParseBucketPolicy([]byte(bp(many)), "b"); err == nil {
		t.Error("bucket policy with too many variables in a condition value accepted")
	}
	if _, err := ParseBucketPolicy([]byte(bp(ok)), "b"); err != nil {
		t.Errorf("bucket policy with %d variables refused: %v", maxVarRefs, err)
	}
	res := `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/` +
		strings.Repeat("${aws:username}", maxVarRefs+1) + `"}]}`
	if _, err := ParsePolicy([]byte(res)); err == nil {
		t.Error("identity policy with too many variables in a resource accepted")
	}
}

// TestExpansionLimitFailsClosed reproduces H-02: a condition value repeating
// ${aws:UserAgent} with a huge User-Agent must neither allocate without
// bound nor let an Allow match or a Deny be skipped.
func TestExpansionLimitFailsClosed(t *testing.T) {
	m := newEvalManager(t)
	ua := strings.Repeat("a", 1<<20)
	refs := strings.Repeat("${aws:UserAgent}", 1200)
	deny := rawBucketPolicy(t, `{"Statement":[
		{"Sid":"Read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"},
		{"Sid":"Big","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*",
		 "Condition":{"StringNotEquals":{"aws:UserAgent":"`+refs+`"}}}]}`)
	allow := rawBucketPolicy(t, `{"Statement":[
		{"Sid":"Big","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*",
		 "Condition":{"StringNotEquals":{"aws:UserAgent":"${aws:UserAgent}${aws:UserAgent}x"}}}]}`)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	for _, bp := range []*PolicyDocument{deny, allow} {
		d := m.Authorize(context.Background(), AuthzRequest{
			Identity: anon, Action: "s3:GetObject", Bucket: "b", Key: "k",
			BucketPolicy: bp,
			Conditions:   map[string][]string{"aws:UserAgent": {ua}},
		})
		if d.Allowed || !d.ExplicitDeny || !strings.Contains(d.Reason, "limit exceeded") {
			t.Errorf("decision = %+v, want a limit deny", d)
		}
	}
	runtime.ReadMemStats(&after)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Authorize took %v", d)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("Authorize allocated %d bytes", alloc)
	}

	// Small values still expand and match as before.
	d := m.Authorize(context.Background(), AuthzRequest{
		Identity: anon, Action: "s3:GetObject", Bucket: "b", Key: "k",
		BucketPolicy: allow,
		Conditions:   map[string][]string{"aws:UserAgent": {"curl"}},
	})
	if !d.Allowed {
		t.Errorf("small User-Agent: decision = %+v, want allowed", d)
	}
}

// TestGlobWorkLimitFailsClosed reproduces H-03: a StringLike pattern that
// makes globMatch quadratic against a large subject is cut off by the work
// budget and denies, instead of taking minutes of CPU.
func TestGlobWorkLimitFailsClosed(t *testing.T) {
	m := newEvalManager(t)
	ua := strings.Repeat("a", 1<<20)
	bp := mustBucketPolicy(t, "b", `{"Statement":[
		{"Sid":"Read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"},
		{"Sid":"Glob","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*",
		 "Condition":{"StringLike":{"aws:UserAgent":"*`+strings.Repeat("a", 19000)+`b"}}}]}`)
	start := time.Now()
	d := m.Authorize(context.Background(), AuthzRequest{
		Identity: anon, Action: "s3:GetObject", Bucket: "b", Key: "k",
		BucketPolicy: bp,
		Conditions:   map[string][]string{"aws:UserAgent": {ua}},
	})
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("Authorize took %v", el)
	}
	if d.Allowed || !d.ExplicitDeny || !strings.Contains(d.Reason, "limit exceeded") {
		t.Errorf("decision = %+v, want a limit deny", d)
	}

	// An ordinary User-Agent is evaluated normally.
	d = m.Authorize(context.Background(), AuthzRequest{
		Identity: anon, Action: "s3:GetObject", Bucket: "b", Key: "k",
		BucketPolicy: bp,
		Conditions:   map[string][]string{"aws:UserAgent": {"aws-sdk-go/1.0"}},
	})
	if !d.Allowed {
		t.Errorf("ordinary User-Agent: decision = %+v, want allowed", d)
	}
}

func TestGlobMatchBudget(t *testing.T) {
	b := &budget{glob: 10}
	if globMatchBudget("*b", strings.Repeat("a", 100), false, false, b) || !b.exceeded {
		t.Fatalf("budget not enforced: %+v", b)
	}
	// An exceeded budget stays exceeded.
	if globMatchBudget("a", "a", false, false, b) {
		t.Fatal("match after the budget was exceeded")
	}
	b = &budget{glob: 1000}
	if !globMatchBudget("a*c", "abbbc", false, false, b) || b.exceeded {
		t.Fatalf("small match: %+v", b)
	}
}
