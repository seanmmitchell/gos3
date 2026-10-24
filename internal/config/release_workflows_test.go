package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflowStep struct {
	Name             string            `yaml:"name"`
	ID               string            `yaml:"id"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	WorkingDirectory string            `yaml:"working-directory"`
	With             map[string]string `yaml:"with"`
	Env              map[string]string `yaml:"env"`
}
type workflowJob struct {
	Steps       []workflowStep    `yaml:"steps"`
	Outputs     map[string]string `yaml:"outputs"`
	If          string            `yaml:"if"`
	Needs       yaml.Node         `yaml:"needs"`
	Uses        string            `yaml:"uses"`
	With        map[string]any    `yaml:"with"`
	Permissions map[string]string `yaml:"permissions"`
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
		out, err := workflowBash(t, mock+script, "TAG=v1.2.3", "ASSETS=dist/a\ndist/checksums.txt", "GH_AUDIT_LOG="+filepath.ToSlash(log), "REFUSE_CREATE="+map[bool]string{false: "false", true: "true"}[refuse])
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

// releaseBinaries returns the bin-* artifact names build.yml's binaries
// matrix uploads and the archive file names they hold for version.
func releaseBinaries(version string) (artifacts, archives []string) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		ext := ".tar.gz"
		if goos == "windows" {
			ext = ".zip"
		}
		for _, goarch := range []string{"amd64", "arm64"} {
			for _, suffix := range []string{"", "-headless"} {
				artifacts = append(artifacts, "bin-"+goos+"-"+goarch+suffix)
				archives = append(archives, "gos3_"+strings.TrimPrefix(version, "v")+"_"+goos+"_"+goarch+suffix+ext)
			}
		}
	}
	return artifacts, archives
}

// stepOutputs parses a $GITHUB_OUTPUT file, including name<<DELIMITER
// multi-line values.
func stepOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		if name, delim, ok := strings.Cut(lines[i], "<<"); ok {
			var value []string
			for i++; i < len(lines) && lines[i] != delim; i++ {
				value = append(value, lines[i])
			}
			out[name] = strings.Join(value, "\n")
		} else if name, value, ok := strings.Cut(lines[i], "="); ok {
			out[name] = value
		}
	}
	return out
}

func TestReleaseBinariesBoundToExpectedSet(t *testing.T) {
	const (
		filter = `.artifacts[] | [.id, .name, .expired] | @tsv`
		url    = "repos/owner/repo/actions/runs/42/artifacts?per_page=100"
	)
	job := readWorkflow(t, "release.yml").Jobs["release"]
	if job.Permissions["actions"] != "read" {
		t.Error("release job cannot list the run's artifacts")
	}
	var verify, archives, publish *workflowStep
	downloads := 0
	for i := range job.Steps {
		s := &job.Steps[i]
		values := []string{s.Run}
		for _, v := range s.With {
			values = append(values, v)
		}
		for _, v := range values {
			if strings.Contains(v, "dist/*") || strings.Contains(v, "gos3_*") {
				t.Errorf("step %q selects release files by glob: %q", s.Name, v)
			}
		}
		switch {
		case s.ID == "artifacts":
			verify = s
		case s.ID == "archives":
			archives = s
		case s.Name == "Create, upload and publish the release":
			publish = s
		case strings.HasPrefix(s.Uses, "actions/download-artifact@"):
			downloads++
			if verify == nil || s.With["pattern"] != "" || s.With["name"] != "" || s.With["artifact-ids"] != "${{ steps.artifacts.outputs.ids }}" || s.With["path"] != "dist" {
				t.Errorf("binaries are not downloaded by the verified artifact IDs: %v", s.With)
			}
		case strings.HasPrefix(s.Uses, "actions/attest-build-provenance@"):
			if s.With["subject-path"] != "${{ steps.archives.outputs.subjects }}" {
				t.Errorf("attestation subjects are not the verified archives: %q", s.With["subject-path"])
			}
		}
	}
	if downloads != 1 {
		t.Errorf("release job has %d artifact downloads, want 1", downloads)
	}
	if verify == nil || archives == nil || publish == nil {
		t.Fatal("artifact verification, archive verification or publishing step not found")
	}
	if publish.Env["ASSETS"] != "${{ steps.archives.outputs.assets }}" {
		t.Errorf("release uploads %q, not the verified assets", publish.Env["ASSETS"])
	}

	names, files := releaseBinaries("v1.2.3")
	listing := func(names ...string) string {
		var b strings.Builder
		b.WriteString("7\tui-embed\tfalse\n")
		for i, n := range names {
			b.WriteString(strconv.Itoa(1000+i) + "\t" + n + "\tfalse\n")
		}
		return b.String()
	}
	mock := `gh() {
  if [ "$#" != 5 ] || [ "$1 $2 $3" != "api --paginate --jq" ] || [ "$4" != "$WANT_FILTER" ] || [ "$5" != "$WANT_URL" ]; then
    echo "unexpected gh $*" >&2
    return 2
  fi
  printf '%s' "$LISTING"
}
`
	for _, tc := range []struct {
		name    string
		listing string
		ok      bool
	}{
		{"exact", listing(names...), true},
		{"extra bin-zz", listing(append(slices.Clone(names), "bin-zz")...), false},
		{"extra same-archive name", listing(append(slices.Clone(names), "bin-linux-amd64-x")...), false},
		{"missing", listing(names[:len(names)-1]...), false},
		{"duplicate", listing(append(slices.Clone(names), names[0])...), false},
		{"expired", strings.Replace(listing(names...), "bin-darwin-arm64\tfalse", "bin-darwin-arm64\ttrue", 1), false},
		{"empty", "", false},
	} {
		output := filepath.Join(t.TempDir(), "output")
		out, err := workflowBash(t, mock+verify.Run, "GH_REPO=owner/repo", "GITHUB_RUN_ID=42",
			"GITHUB_OUTPUT="+filepath.ToSlash(output), "WANT_FILTER="+filter, "WANT_URL="+url, "LISTING="+tc.listing)
		if (err == nil) != tc.ok {
			t.Fatalf("artifact check %s: %v %s", tc.name, err, out)
		}
		if tc.ok {
			ids := strings.Split(stepOutputs(t, output)["ids"], ",")
			if len(ids) != len(names) || ids[0] != "1000" || slices.Contains(ids, "7") {
				t.Fatalf("artifact check %s: ids %v", tc.name, ids)
			}
		}
	}

	for _, tc := range []struct {
		name  string
		files []string
		ok    bool
	}{
		{"exact", files, true},
		{"extra file", append(slices.Clone(files), "gos3_1.2.3_linux_amd64.tar.gz.sig"), false},
		{"planted checksums", append(slices.Clone(files), "checksums.txt"), false},
		{"other version", append(slices.Clone(files[1:]), "gos3_1.2.4_linux_amd64.tar.gz"), false},
		{"missing", files[1:], false},
	} {
		dist := t.TempDir()
		for _, f := range tc.files {
			if err := os.WriteFile(filepath.Join(dist, f), []byte(f), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		output := filepath.Join(t.TempDir(), "output")
		out, err := workflowBash(t, "cd \""+filepath.ToSlash(dist)+"\"\n"+archives.Run, "TAG=v1.2.3", "GITHUB_OUTPUT="+filepath.ToSlash(output))
		if (err == nil) != tc.ok {
			t.Fatalf("archive check %s: %v %s", tc.name, err, out)
		}
		if !tc.ok {
			continue
		}
		var subjects []string
		for _, f := range files {
			subjects = append(subjects, "dist/"+f)
		}
		outputs := stepOutputs(t, output)
		if outputs["subjects"] != strings.Join(subjects, "\n") || outputs["assets"] != strings.Join(append(subjects, "dist/checksums.txt"), "\n") {
			t.Fatalf("archive check outputs: %q", outputs)
		}
		sums, err := os.ReadFile(filepath.Join(dist, "checksums.txt"))
		if err != nil || strings.Count(string(sums), "\n") != len(files) {
			t.Fatalf("checksums: %v %q", err, sums)
		}

		log := filepath.Join(t.TempDir(), "commands.log")
		out, err = workflowBash(t, `gh() { printf '%s\n' "$*" >> "$GH_AUDIT_LOG"; }
`+publish.Run, "TAG=v1.2.3", "GH_AUDIT_LOG="+filepath.ToSlash(log), "ASSETS="+outputs["assets"])
		commands, _ := os.ReadFile(log)
		if err != nil || !strings.Contains(string(commands), "release upload v1.2.3 "+strings.Join(append(subjects, "dist/checksums.txt"), " ")+"\n") {
			t.Fatalf("upload is not the verified assets: %v %s %s", err, out, commands)
		}
	}
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
