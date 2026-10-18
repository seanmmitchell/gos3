package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// answerServer is a fake server answering every credentials request with
// doc.
func answerServer(t *testing.T, doc map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/restic/credentials") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The CLI runs what the server answers (restic, or `eval` of `env`), so it
// refuses an answer naming anything that could run a program: before, a
// malicious or tampered answer could set RESTIC_PASSWORD_COMMAND,
// PROMPT_COMMAND or LD_PRELOAD, an sftp.command option or an sftp
// repository.
func TestResticRefusesUnsafeAnswer(t *testing.T) {
	clearEnv(t)
	s3 := "s3:https://s3.example.com/b/p"
	creds := map[string]any{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}
	with := func(k, v string) map[string]any {
		m := map[string]any{k: v}
		for n, x := range creds {
			m[n] = x
		}
		return m
	}
	for name, doc := range map[string]map[string]any{
		"password command": {"repository": s3, "password": "pw", "env": with("RESTIC_PASSWORD_COMMAND", "sh -c id")},
		"shell hook":       {"repository": s3, "password": "pw", "env": with("PROMPT_COMMAND", "id")},
		"preload":          {"repository": s3, "password": "pw", "env": with("LD_PRELOAD", "/tmp/x.so")},
		"sftp option":      {"repository": s3, "password": "pw", "options": []string{"sftp.command=sh -c id"}},
		"rclone option":    {"repository": s3, "password": "pw", "options": []string{"rclone.program=/tmp/x"}},
		"sftp repository":  {"repository": "sftp:-oProxyCommand=id:/x", "password": "pw"},
	} {
		url := answerServer(t, doc)
		for _, cmd := range [][]string{{"env", "--shell", "bash"}, {"exec"}} {
			args := append([]string{"restic", cmd[0], "--bucket", "b", "--server", url, "--access-key", "AK", "--secret-key", "SK"}, cmd[1:]...)
			if cmd[0] == "exec" {
				args = append(args, "--", "snapshots")
			}
			code, stdout, stderr := runCmd(t, args...)
			if code != 1 || !strings.Contains(stderr, "refusing the server's answer") || stdout != "" {
				t.Errorf("%s, %s: exit %d, stdout %q, stderr %q", name, cmd[0], code, stdout, stderr)
			}
		}
	}

	// gos3's own answers pass.
	url := answerServer(t, map[string]any{"repository": s3, "password": "pw", "env": creds, "options": []string{"s3.bucket-lookup=path"}})
	code, stdout, stderr := runCmd(t, "restic", "env", "--bucket", "b", "--server", url, "--access-key", "AK", "--secret-key", "SK", "--shell", "bash")
	if code != 0 || !strings.Contains(stdout, "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("valid answer: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// Plain HTTP to another machine needs --allow-http.
func TestResticPlainHTTP(t *testing.T) {
	clearEnv(t)
	base := []string{"restic", "env", "--bucket", "b", "--access-key", "AK", "--secret-key", "SK"}
	code, _, stderr := runCmd(t, append(base, "--server", "http://192.0.2.1:9000")...)
	if code != 1 || !strings.Contains(stderr, "--allow-http") {
		t.Fatalf("http to another machine: exit %d: %s", code, stderr)
	}
	// With the opt-in the request is made (and fails: nothing listens).
	code, _, stderr = runCmd(t, append(base, "--server", "http://192.0.2.1:9000", "--allow-http", "--timeout", "200ms")...)
	if code != 1 || strings.Contains(stderr, "--allow-http") {
		t.Fatalf("--allow-http: exit %d: %s", code, stderr)
	}
	// Loopback needs no opt-in. Allow time for localhost resolution and
	// IPv6-to-IPv4 fallback on slower CI runners; this checks HTTP policy,
	// not connection speed.
	url := answerServer(t, map[string]any{"repository": "/srv/repo", "password": "pw"})
	if code, _, stderr := runCmd(t, append(base, "--server", strings.Replace(url, "127.0.0.1", "localhost", 1), "--timeout", "5s")...); code != 0 {
		t.Fatalf("loopback: exit %d: %s", code, stderr)
	}
}
