package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflowStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}
type workflowJob struct {
	Steps   []workflowStep    `yaml:"steps"`
	Outputs map[string]string `yaml:"outputs"`
}
type releaseWorkflow struct {
	On   map[string]any         `yaml:"on"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

func workflowRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readWorkflow(t *testing.T, name string) releaseWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workflowRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w releaseWorkflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func workflowBash(t *testing.T, script string, env ...string) (string, error) {
	t.Helper()
	path, err := exec.LookPath("bash")
	if err != nil && runtime.GOOS == "windows" {
		path = `C:\Program Files\Git\bin\bash.exe`
		_, err = os.Stat(path)
	}
	if err != nil {
		t.Skip("bash is unavailable")
	}
	cmd := exec.Command(path, "-c", "set -euo pipefail\n"+script)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseSourceGateUsesBaseWorkflow(t *testing.T) {
	w := readWorkflow(t, "release-pr.yml")
	if _, ok := w.On["pull_request_target"]; !ok {
		t.Fatal("PR can replace the source gate workflow")
	}
	var script string
	for _, s := range w.Jobs["source"].Steps {
		if s.Uses != "" {
			t.Fatal("source gate must not execute pull request actions or checkout")
		}
		script += s.Run + "\n"
	}
	for _, tc := range []struct {
		branch, repo string
		allowed      bool
	}{
		{"dev", "owner/repo", true}, {"dev", "attacker/repo", false}, {"feature", "owner/repo", false}, {"dev\"; echo injected; #", "owner/repo", false},
	} {
		out, err := workflowBash(t, script, "GITHUB_REPOSITORY=owner/repo", "HEAD_REF="+tc.branch, "HEAD_REPO="+tc.repo)
		if (err == nil) != tc.allowed {
			t.Fatalf("gate for %q %q: %v %s", tc.branch, tc.repo, err, out)
		}
	}
}

func TestReleaseNeverPublishesAnExistingDraft(t *testing.T) {
	w := readWorkflow(t, "release.yml")
	var script string
	for _, s := range w.Jobs["release"].Steps {
		if s.Name == "Create, upload and publish the release" {
			script = s.Run
		}
	}
	if script == "" {
		t.Fatal("publishing step not found")
	}
	for _, refuse := range []bool{false, true} {
		log := filepath.Join(t.TempDir(), "commands.log")
		mock := `gh() {
  printf '%s\n' "$*" >> "$GH_AUDIT_LOG"
  case "$1 $2" in
    "release view") echo true ;;
    "release create") if [ "$REFUSE_CREATE" = true ]; then return 1; fi ;;
  esac
}
`
		out, err := workflowBash(t, mock+script, "TAG=v1.2.3", "GH_AUDIT_LOG="+filepath.ToSlash(log), "REFUSE_CREATE="+map[bool]string{false: "false", true: "true"}[refuse])
		data, readErr := os.ReadFile(log)
		if readErr != nil {
			t.Fatal(readErr)
		}
		commands := string(data)
		if refuse {
			if err == nil || strings.Contains(commands, "release upload") || strings.Contains(commands, "release edit") {
				t.Fatalf("existing draft was modified or published: %v %s %s", err, out, commands)
			}
		} else if err != nil || !strings.Contains(commands, "release create") || !strings.Contains(commands, "release upload") || !strings.Contains(commands, "release edit") {
			t.Fatalf("new release failed: %v %s %s", err, out, commands)
		}
	}
}

func TestEmbeddedArtifactBoundToProducer(t *testing.T) {
	w := readWorkflow(t, "build.yml")
	ui := w.Jobs["ui"]
	if ui.Outputs["artifact-id"] != "${{ steps.upload.outputs.artifact-id }}" {
		t.Fatal("UI output is not the producer's immutable artifact ID")
	}
	found := false
	for _, s := range ui.Steps {
		if s.ID == "upload" && strings.HasPrefix(s.Uses, "actions/upload-artifact@") {
			found = true
		}
	}
	if !found {
		t.Fatal("trusted UI producer not found")
	}
	for _, s := range w.Jobs["binaries"].Steps {
		if strings.HasPrefix(s.Uses, "actions/download-artifact@") {
			if s.With["artifact-ids"] != "${{ needs.ui.outputs.artifact-id }}" || s.With["name"] != "" {
				t.Fatal("embedded artifact can be replaced by name")
			}
			return
		}
	}
	t.Fatal("UI download not found")
}

func TestSecurityChecksBoundToPublisher(t *testing.T) {
	for _, branch := range []string{"main", "dev"} {
		data, err := os.ReadFile(filepath.Join(workflowRoot(t), ".github", "rulesets", branch+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var rules struct {
			Rules []struct {
				Parameters struct {
					Checks []struct {
						Context     string `json:"context"`
						Integration int    `json:"integration_id"`
					} `json:"required_status_checks"`
				} `json:"parameters"`
			} `json:"rules"`
		}
		if err := json.Unmarshal(data, &rules); err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, r := range rules.Rules {
			for _, c := range r.Parameters.Checks {
				want := map[string]int{"Socket Security: Pull Request Alerts": 156372, "GitGuardian Security Checks": 46505}[c.Context]
				if want != 0 {
					found++
					if c.Integration != want {
						t.Errorf("%s check %q has publisher %d, want %d", branch, c.Context, c.Integration, want)
					}
				}
			}
		}
		if found != 2 {
			t.Fatalf("%s: missing required security checks", branch)
		}
	}
}
