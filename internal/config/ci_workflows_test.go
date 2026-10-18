package config

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func workflowNeeds(j workflowJob) map[string]bool {
	needs := make(map[string]bool)
	if j.Needs.Kind == yaml.ScalarNode {
		needs[j.Needs.Value] = true
	} else {
		for _, n := range j.Needs.Content {
			needs[n.Value] = true
		}
	}
	return needs
}

func TestDevPublishingWaitsForAllCI(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	publish := ci.Jobs["dev-build"]
	if publish.Uses != "./.github/workflows/dev-build.yml" || publish.If != "github.event_name == 'push' && github.ref == 'refs/heads/dev'" {
		t.Fatal("dev publishing must only follow a push to dev in the same CI run")
	}
	needs := workflowNeeds(publish)
	for name := range ci.Jobs {
		if name != "dev-build" && !needs[name] {
			t.Errorf("dev publishing does not wait for CI job %q", name)
		}
	}
	dev := readWorkflow(t, "dev-build.yml")
	if len(dev.On) != 1 {
		t.Fatal("dev build must not have an independent trigger")
	}
	if _, ok := dev.On["workflow_call"]; !ok {
		t.Fatal("dev build must be called by CI")
	}
	build := dev.Jobs["build"]
	if build.With["ref"] != "${{ github.sha }}" || build.With["run-tests"] != false {
		t.Fatal("dev build must reuse validation of the same commit")
	}
}

func TestUncachedReleaseAlwaysValidates(t *testing.T) {
	build := readWorkflow(t, "build.yml")
	call := build.On["workflow_call"].(map[string]any)
	inputs := call["inputs"].(map[string]any)
	validation := inputs["run-tests"].(map[string]any)
	if validation["default"] != true {
		t.Fatal("standalone build callers must validate by default")
	}
	for _, name := range []string{"test", "ui-test"} {
		if build.Jobs[name].If != "inputs.run-tests || !inputs.use-cache" {
			t.Errorf("%s must validate uncached releases even when run-tests is false", name)
		}
	}
	release := readWorkflow(t, "release.yml").Jobs["build"]
	if release.With["use-cache"] != false || release.With["run-tests"] == false {
		t.Fatal("release must rebuild without caches and run independent validation")
	}
	binaries := build.Jobs["binaries"]
	needs := workflowNeeds(binaries)
	for _, name := range []string{"test", "ui-test", "ui"} {
		if !needs[name] {
			t.Errorf("binaries do not wait for %s", name)
		}
	}
	wantGate := "${{ !cancelled() && needs.ui.result == 'success' && " +
		"((needs.test.result == 'success' && needs.ui-test.result == 'success') || " +
		"(!inputs.run-tests && inputs.use-cache && " +
		"needs.test.result == 'skipped' && needs.ui-test.result == 'skipped')) }}"
	if binaries.If != wantGate {
		t.Fatal("failed or cancelled validation and UI builds must block binaries")
	}
}

func TestBrowserArtifactsBoundToCIProducer(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	web := ci.Jobs["web"]
	want := map[string]bool{
		"${{ needs.web.outputs.embedded-artifact-id }}":   false,
		"${{ needs.web.outputs.standalone-artifact-id }}": false,
	}
	for _, step := range ci.Jobs["playwright"].Steps {
		if !strings.HasPrefix(step.Uses, "actions/download-artifact@") {
			continue
		}
		id := step.With["artifact-ids"]
		if _, ok := want[id]; !ok || step.With["name"] != "" {
			t.Fatal("browser artifacts must come from immutable web producer outputs")
		}
		want[id] = true
	}
	for id, found := range want {
		if !found {
			t.Errorf("missing browser artifact %s", id)
		}
	}
	for _, id := range []string{"embedded", "standalone"} {
		if web.Outputs[id+"-artifact-id"] != "${{ steps."+id+".outputs.artifact-id }}" {
			t.Errorf("%s output is not bound to its upload", id)
		}
	}
	// Publishing happens later in the same run. Its artifact names must not
	// collide with the browser-test artifacts (upload-artifact is immutable).
	for _, step := range web.Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && !strings.HasPrefix(step.With["name"], "ci-") {
			t.Fatal("CI UI uploads must have their own artifact namespace")
		}
	}
}
