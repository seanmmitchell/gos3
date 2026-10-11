package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
)

// TestProbeNeedsProbeNetwork checks discovery-3: probes make outbound
// connections, so they need admin:ProbeNetwork, which the read-only
// diagnostics policy does not grant.
func TestProbeNeedsProbeNetwork(t *testing.T) {
	e := newEnv(t)
	diag := e.addUser("diag", iam.PolicyDiagnostics)
	body := map[string]any{"url": "http://127.0.0.1:1"}

	msg := e.do(&diag, "POST", Prefix+"/discovery/probe", body).expectError(t, http.StatusForbidden, codeAccessDenied)
	if !strings.Contains(msg, "admin:ProbeNetwork") {
		t.Errorf("message %q", msg)
	}
	// Reading discovery state is still fine.
	e.do(&diag, "GET", Prefix+"/discovery", nil).expectStatus(t, http.StatusOK)

	e.putPolicy("prober", map[string]any{"Effect": "Allow", "Action": "admin:ProbeNetwork", "Resource": "*"})
	p := e.addUser("prober", "prober")
	e.do(&p, "POST", Prefix+"/discovery/probe", body).expectError(t, http.StatusBadGateway, codeProbeFailed)
	boss := e.addUser("boss", iam.PolicyAdmin)
	e.do(&boss, "POST", Prefix+"/discovery/probe", body).expectError(t, http.StatusBadGateway, codeProbeFailed)

	t.Run("whoami probe capability", func(t *testing.T) {
		for _, tc := range []struct {
			c        string
			want     bool
			wantDisc bool
		}{{"diag", false, true}, {"prober", true, false}, {"boss", true, true}} {
			var r response
			switch tc.c {
			case "diag":
				r = e.do(&diag, "GET", Prefix+"/whoami", nil)
			case "prober":
				r = e.do(&p, "GET", Prefix+"/whoami", nil)
			default:
				r = e.do(&boss, "GET", Prefix+"/whoami", nil)
			}
			var w whoAmI
			r.json(t, &w)
			if w.Capabilities.Probe != tc.want || w.Capabilities.Discovery != tc.wantDisc {
				t.Errorf("%s: capabilities %+v", tc.c, w.Capabilities)
			}
		}
		var w whoAmI
		e.root("GET", "/whoami", nil).json(t, &w)
		if !w.Capabilities.Probe {
			t.Error("root: probe = false")
		}
	})
}

// TestProbeErrorsAreCoarse checks that probe failures no longer echo the
// raw Go error, which leaked banners of non-HTTP services and told closed,
// filtered and TLS ports apart.
func TestProbeErrorsAreCoarse(t *testing.T) {
	e := newEnv(t)

	// A raw TCP service that answers with a banner.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6 SECRET-BANNER\r\n"))
			_ = c.Close()
		}
	}()
	statusSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "secret-internal-thing")
		http.Error(w, "SECRET-BODY", http.StatusTeapot)
	}))
	defer statusSrv.Close()
	notGos3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"SECRET-SERVICE","id":"x"}`))
	}))
	defer notGos3.Close()

	cases := []struct {
		url  string
		want string
	}{
		{"http://127.0.0.1:1", "unreachable"},
		{"http://" + ln.Addr().String(), "unreachable"},
		{statusSrv.URL, "HTTP 418"},
		{notGos3.URL, "not a gos3 server"},
	}
	for _, tc := range cases {
		msg := e.root("POST", "/discovery/probe", map[string]any{"url": tc.url}).expectError(t, http.StatusBadGateway, codeProbeFailed)
		if !strings.HasSuffix(msg, ": "+tc.want) {
			t.Errorf("%s: message %q, want suffix %q", tc.url, msg, tc.want)
		}
		for _, leak := range []string{"SECRET", "SSH", "malformed", "refused", "dial", "teapot", "Teapot"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: message %q leaks %q", tc.url, msg, leak)
			}
		}
	}
}

// TestProbeRefusesBlockedAddresses checks that probes of cloud metadata,
// unspecified and multicast addresses are refused with 400 before any
// request is sent, and never reported as a reachable or unreachable host.
func TestProbeRefusesBlockedAddresses(t *testing.T) {
	e := newEnv(t)
	for _, u := range []string{
		"http://169.254.169.254",
		"http://169.254.169.254:80/latest/meta-data",
		"http://[fd00:ec2::254]",
		"http://100.100.100.200",
		"http://0.0.0.0:9000",
		"http://[::ffff:169.254.169.254]",
		"http://224.0.0.251:9000",
	} {
		msg := e.root("POST", "/discovery/probe", map[string]any{"url": u}).expectError(t, http.StatusBadRequest, codeInvalidRequest)
		if !strings.HasSuffix(msg, "address not allowed") {
			t.Errorf("%s: message %q, want an address-not-allowed refusal", u, msg)
		}
	}
}

// TestProbeFailureCategories checks the error to message mapping,
// including the timeout cases that are slow to provoke over the network.
func TestProbeFailureCategories(t *testing.T) {
	timeoutErr := &net.OpError{Op: "dial", Net: "tcp", Err: &timeoutError{}}
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timed out"},
		{fmt.Errorf("Get \"http://x/_gos3/info\": %w", context.DeadlineExceeded), "timed out"},
		{&url.Error{Op: "Get", URL: "http://x", Err: timeoutErr}, "timed out"},
		{&url.Error{Op: "Get", URL: "http://x", Err: errors.New("malformed HTTP response \"SSH-2.0\"")}, "unreachable"},
		{errors.New("connect: connection refused"), "unreachable"},
		{fmt.Errorf("%w: GET %s returned 503 Service Unavailable", discovery.ErrNotGos3, discovery.InfoPath), "HTTP 503"},
		{fmt.Errorf("%w: GET %s returned 2x0 nope", discovery.ErrNotGos3, discovery.InfoPath), "not a gos3 server"},
		{fmt.Errorf("%w: service is \"returned 200\"", discovery.ErrNotGos3), "not a gos3 server"},
		{fmt.Errorf("%w: invalid JSON: x", discovery.ErrNotGos3), "not a gos3 server"},
	}
	for _, tc := range cases {
		if got := probeFailure(tc.err); got != tc.want {
			t.Errorf("probeFailure(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

// TestProbeConcurrencyLimit checks that at most maxConcurrentProbes probes
// run at once; further requests get 429.
func TestProbeConcurrencyLimit(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	arrived := 0
	allIn := make(chan struct{})
	peerInfo := discovery.Info{Service: discovery.ServiceName, ID: "fedcba9876543210", Name: "peer", Version: "1", APIVersion: "v1"}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrived++
		if arrived == maxConcurrentProbes {
			close(allIn)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerInfo)
	}))
	defer slow.Close()
	defer unblock() // before slow.Close waits for its handlers

	var wg sync.WaitGroup
	results := make(chan int, maxConcurrentProbes)
	for range maxConcurrentProbes {
		wg.Go(func() {
			results <- e.root("POST", "/discovery/probe", map[string]any{"url": slow.URL}).status
		})
	}
	select {
	case <-allIn:
	case <-time.After(10 * time.Second):
		t.Fatal("probes did not reach the target")
	}
	msg := e.root("POST", "/discovery/probe", map[string]any{"url": slow.URL}).expectError(t, http.StatusTooManyRequests, codeTooManyRequests)
	if !strings.Contains(msg, "probes") {
		t.Errorf("message %q", msg)
	}
	unblock()
	wg.Wait()
	close(results)
	for s := range results {
		if s != http.StatusOK {
			t.Errorf("blocked probe status = %d", s)
		}
	}
	// Slots are released afterwards.
	e.root("POST", "/discovery/probe", map[string]any{"url": slow.URL}).expectStatus(t, http.StatusOK)
}
