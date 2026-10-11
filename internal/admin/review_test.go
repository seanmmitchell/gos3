package admin

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/clientip"
)

func withTrustProxy(proxies ...string) envOption {
	return func(d *Deps) { d.TrustProxyHeaders = clientip.New(true, mustParsePrefixes(proxies)) }
}

// putPolicy stores a policy document through the API as root.
func (e *testEnv) putPolicy(name string, statements ...map[string]any) {
	e.t.Helper()
	e.root("PUT", "/policies/"+name, map[string]any{
		"document": map[string]any{"Version": "2012-10-17", "Statement": statements},
	}).expectStatus(e.t, 200)
}

// TestTrustProxyHeadersConditions checks that the admin API evaluates
// aws:SourceIp and aws:SecureTransport like the S3 API: from
// X-Forwarded-For / X-Forwarded-Proto when server.trust_proxy_headers is
// on, from the connection otherwise. Previously the admin API always used
// the connection, so behind a reverse proxy every admin request appeared to
// come from the proxy.
func TestTrustProxyHeadersConditions(t *testing.T) {
	policy := func(e *testEnv) {
		e.putPolicy("office-only", map[string]any{
			"Effect": "Allow", "Action": "admin:ServerInfo", "Resource": "*",
			"Condition": map[string]any{"IpAddress": map[string]any{"aws:SourceIp": "203.0.113.0/24"}},
		}, map[string]any{
			"Effect": "Allow", "Action": "admin:GetStats", "Resource": "*",
			"Condition": map[string]any{"Bool": map[string]any{"aws:SecureTransport": "true"}},
		})
	}
	t.Run("trusted", func(t *testing.T) {
		e := newEnv(t, withTrustProxy())
		policy(e)
		ops := e.addUser("ops", "office-only")
		e.do(&ops, "GET", Prefix+"/info", nil, withHeader("X-Forwarded-For", "10.0.0.2, 203.0.113.7")).expectStatus(t, 200)
		e.do(&ops, "GET", Prefix+"/info", nil, withHeader("X-Forwarded-For", "198.51.100.1")).expectError(t, 403, codeAccessDenied)
		e.do(&ops, "GET", Prefix+"/info", nil).expectError(t, 403, codeAccessDenied)
		e.do(&ops, "GET", Prefix+"/stats", nil, withHeader("X-Forwarded-Proto", "https")).expectStatus(t, 200)
		e.do(&ops, "GET", Prefix+"/stats", nil, withHeader("X-Forwarded-Proto", "http")).expectError(t, 403, codeAccessDenied)
	})
	t.Run("untrusted", func(t *testing.T) {
		e := newEnv(t)
		policy(e)
		ops := e.addUser("ops", "office-only")
		e.do(&ops, "GET", Prefix+"/info", nil, withHeader("X-Forwarded-For", "203.0.113.7")).expectError(t, 403, codeAccessDenied)
		e.do(&ops, "GET", Prefix+"/stats", nil, withHeader("X-Forwarded-Proto", "https")).expectError(t, 403, codeAccessDenied)
	})
}

// TestOwnKeysExplicitDeny checks that the self-service key endpoints, open
// to every user by default, honour an explicit Deny: previously a user (or
// a stolen key) under "Deny admin:*" could still mint fresh access keys
// that survived deactivating the original one.
func TestOwnKeysExplicitDeny(t *testing.T) {
	e := newEnv(t)
	e.putPolicy("deny-admin", map[string]any{"Effect": "Deny", "Action": "admin:*", "Resource": "*"})
	e.putPolicy("deny-own-keys", map[string]any{"Effect": "Deny", "Action": "admin:CreateOwnAccessKey", "Resource": "*"})

	plain := e.addUser("plain")
	r := e.do(&plain, "POST", Prefix+"/whoami/keys", map[string]any{"description": "second"})
	r.expectStatus(t, http.StatusCreated)
	var k struct {
		AccessKey string `json:"accessKey"`
	}
	r.json(t, &k)
	e.do(&plain, "DELETE", Prefix+"/whoami/keys/"+k.AccessKey, nil).expectStatus(t, http.StatusNoContent)

	locked := e.addUser("locked", "deny-admin")
	e.do(&locked, "GET", Prefix+"/whoami/keys", nil).expectStatus(t, 200)
	e.do(&locked, "POST", Prefix+"/whoami/keys", map[string]any{}).expectError(t, 403, codeAccessDenied)
	// A second key added by an administrator cannot be deleted either.
	k2, err := e.iam.CreateAccessKey(t.Context(), "locked", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.do(&locked, "DELETE", Prefix+"/whoami/keys/"+k2.AccessKey, nil).expectError(t, 403, codeAccessDenied)

	nokeys := e.addUser("nokeys", "deny-own-keys")
	e.do(&nokeys, "POST", Prefix+"/whoami/keys", map[string]any{}).expectError(t, 403, codeAccessDenied)
}

// TestScanConcurrencyLimit checks that at most maxConcurrentScans network
// scans run at once; further requests get 429 instead of starting hundreds
// more probe workers each.
func TestScanConcurrencyLimit(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	hits := make(chan struct{}, 16)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.NotFound(w, r)
	}))
	defer slow.Close()
	defer unblock() // before slow.Close waits for its handlers

	body := map[string]any{"targets": []string{slow.URL}, "timeout": int64(30 * time.Second)}
	var wg sync.WaitGroup
	statuses := make(chan int, maxConcurrentScans)
	for range maxConcurrentScans {
		wg.Go(func() { statuses <- e.root("POST", "/discovery/scan", body).status })
	}
	for range maxConcurrentScans {
		select {
		case <-hits:
		case <-time.After(10 * time.Second):
			t.Fatal("scans did not start")
		}
	}
	e.root("POST", "/discovery/scan", body).expectError(t, http.StatusTooManyRequests, codeTooManyRequests)
	unblock()
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != 200 {
			t.Errorf("blocked scan finished with %d", s)
		}
	}
	// Capacity is released afterwards.
	e.root("POST", "/discovery/scan", body).expectStatus(t, 200)
}
