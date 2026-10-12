package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// AWS documentation example credentials.
const (
	testAK = "AKIAIOSFODNN7EXAMPLE"
	testSK = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// docTime is the request time of the AWS SigV4 documentation examples.
var docTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

type staticCreds map[string]string

func (s staticCreds) LookupCredentials(ak string) (string, Identity, error) {
	sk, ok := s[ak]
	if !ok {
		return "", Identity{}, s3err.ErrInvalidAccessKeyID
	}
	return sk, Identity{AccessKey: ak, User: "user-" + ak}, nil
}

var testCreds = staticCreds{testAK: testSK, "other": "other-secret"}

func newTestVerifier(now time.Time, mods ...func(*Options)) *Verifier {
	o := Options{Region: "us-east-1", Now: func() time.Time { return now }}
	for _, m := range mods {
		m(&o)
	}
	return NewVerifier(testCreds, o)
}

func liveVerifier(mods ...func(*Options)) *Verifier {
	o := Options{Region: "us-east-1"}
	for _, m := range mods {
		m(&o)
	}
	return NewVerifier(testCreds, o)
}

func errCode(err error) string {
	if err == nil {
		return ""
	}
	var e *s3err.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "non-s3: " + err.Error()
}

// outcome is what the test server reports about one request.
type outcome struct {
	Code        string              `json:"code,omitempty"`
	Message     string              `json:"message,omitempty"`
	Stage       string              `json:"stage,omitempty"` // "auth" or "body"
	User        string              `json:"user,omitempty"`
	Anonymous   bool                `json:"anonymous,omitempty"`
	Method      Method              `json:"method,omitempty"`
	Region      string              `json:"region,omitempty"`
	PayloadMode string              `json:"payloadMode,omitempty"`
	DecodedLen  int64               `json:"decodedLen"`
	Body        []byte              `json:"body,omitempty"`
	Trailer     map[string][]string `json:"trailer,omitempty"`
}

// runAuth authenticates r and drains the body, reporting the outcome.
func runAuth(v *Verifier, r *http.Request) outcome {
	res, err := v.Authenticate(r)
	if err != nil {
		e := s3err.As(err)
		return outcome{Code: e.Code, Message: e.Message, Stage: "auth"}
	}
	o := outcome{
		User: res.Identity.User, Anonymous: res.Identity.IsAnonymous, Method: res.Method,
		Region: res.Region, PayloadMode: res.PayloadMode, DecodedLen: res.DecodedContentLength,
	}
	data, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		e := s3err.As(err)
		o.Code, o.Message, o.Stage = e.Code, e.Message, "body"
		return o
	}
	o.Body = data
	if res.Trailer == nil {
		o.Code = "nil Trailer func"
		return o
	}
	tr := res.Trailer()
	if tr == nil {
		o.Code = "nil Trailer"
		return o
	}
	if len(tr) > 0 {
		o.Trailer = tr
	}
	return o
}

// newAuthServer starts a server that authenticates every request with v.
func newAuthServer(t *testing.T, v *Verifier) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(authHandler(v))
	t.Cleanup(srv.Close)
	return srv
}

func authHandler(v *Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := runAuth(v, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(o)
	})
}

// send performs req with client and decodes the server's outcome.
func send(t *testing.T, client *http.Client, req *http.Request) outcome {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	var o outcome
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	return o
}
