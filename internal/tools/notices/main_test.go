package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// manifestJSON returns a build record listing packages as name, version, dir
// triples.
func manifestJSON(t *testing.T, pkgs ...string) string {
	t.Helper()
	type pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dir     string `json:"dir"`
	}
	m := struct {
		Packages []pkg `json:"packages"`
	}{Packages: []pkg{}}
	for i := 0; i+2 < len(pkgs); i += 3 {
		m.Packages = append(m.Packages, pkg{pkgs[i], pkgs[i+1], pkgs[i+2]})
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNormalize(t *testing.T) {
	got := normalize("\r\n\nMIT License  \r\n\r\nCopyright (c) Someone\t\r\n\n")
	if want := "MIT License\n\nCopyright (c) Someone"; got != want {
		t.Fatalf("normalize = %q, want %q", got, want)
	}
}

func TestReadTexts(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"NOTICE":       "Notice text\n",
		"LICENSE.md":   "License text\n# Licenses of bundled dependencies\nOther\n",
		"license.js":   "module.exports = 1\n",
		"LICENSE-MIT":  "MIT text\n",
		"licenses.txt": "not a license file name\n",
		"README.md":    "readme\n",
	})
	texts, err := readTexts(dir, "\n# Licenses of bundled dependencies")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, tx := range texts {
		files = append(files, tx.file)
	}
	if got, want := strings.Join(files, ","), "LICENSE-MIT,LICENSE.md,NOTICE"; got != want {
		t.Fatalf("files = %s, want %s", got, want)
	}
	if texts[1].body != "License text" {
		t.Fatalf("excerpt = %q", texts[1].body)
	}
}

func TestDeclaredLicense(t *testing.T) {
	for _, tc := range []struct{ pkg, want string }{
		{`{"license": "MIT"}`, "MIT"},
		{`{"license": "(MIT OR Apache-2.0)"}`, "(MIT OR Apache-2.0)"},
		{`{"license": {"type": "ISC"}}`, "ISC"},
		{`{"licenses": [{"type": "MIT"}, {"type": "Apache-2.0"}]}`, "MIT OR Apache-2.0"},
		{`{"name": "x"}`, ""},
	} {
		dir := writeFiles(t, map[string]string{"package.json": tc.pkg})
		got, err := declaredLicense(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("declaredLicense(%s) = %q, want %q", tc.pkg, got, tc.want)
		}
	}
}

func TestNpmEntriesMergesVersionsAndArtifacts(t *testing.T) {
	v1 := writeFiles(t, map[string]string{"package.json": `{"license":"MIT"}`, "LICENSE": "Copyright 2020 A\n"})
	v2 := writeFiles(t, map[string]string{"package.json": `{"license":"MIT"}`, "LICENSE": "Copyright 2020 A\n"})
	v3 := writeFiles(t, map[string]string{"package.json": `{"license":"MIT"}`, "LICENSE": "Copyright 2024 A\n"})
	web := writeFiles(t, map[string]string{
		"embedded.json":   manifestJSON(t, "a", "1.0.0", v1),
		"standalone.json": manifestJSON(t, "a", "1.0.0", v2, "a", "2.0.0", v3),
	})
	entries, err := npmEntries(web)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	e := entries[0]
	if e.license != "MIT" || strings.Join(e.artifacts, ",") != "gos3,gos3-ui" || len(e.texts) != 2 {
		t.Fatalf("entry = %+v", e)
	}
}

func TestNpmEntriesRequiresLicenseText(t *testing.T) {
	pkg := writeFiles(t, map[string]string{"package.json": `{"license":"MIT"}`})
	web := writeFiles(t, map[string]string{
		"embedded.json":   manifestJSON(t, "no-license-file", "1.0.0", pkg),
		"standalone.json": manifestJSON(t),
	})
	if _, err := npmEntries(web); err == nil || !strings.Contains(err.Error(), "no-license-file") {
		t.Fatalf("err = %v, want a missing license error", err)
	}
}

func TestNpmEntriesRequiresPackageDir(t *testing.T) {
	web := writeFiles(t, map[string]string{
		"embedded.json":   manifestJSON(t),
		"standalone.json": manifestJSON(t, "unlocated", "1.0.0", ""),
	})
	if _, err := npmEntries(web); err == nil || !strings.Contains(err.Error(), "unlocated@1.0.0") {
		t.Fatalf("err = %v, want a missing directory error", err)
	}
}

func TestFallbacksExist(t *testing.T) {
	for name, fb := range fallbacks {
		body, err := fallbackFS.ReadFile("fallback/" + fb.file)
		if err != nil || normalize(string(body)) == "" {
			t.Errorf("%s: fallback/%s: %v", name, fb.file, err)
		}
	}
}

func TestExecutableEntries(t *testing.T) {
	entries, err := executableEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(executables) {
		t.Fatalf("entries = %+v", entries)
	}
	for _, e := range entries {
		if e.license == "" || e.source == "" || len(e.texts) != 1 || e.texts[0].body == "" ||
			strings.Join(e.artifacts, ",") != artifactGos3Image {
			t.Errorf("entry = %+v", e)
		}
	}
}

// TestExecutableVersionsMatchDockerfile checks that the license text of each
// bundled executable is from the version the Dockerfile and the CI setup
// action install, so a version update cannot forget the notices.
func TestExecutableVersionsMatchDockerfile(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	pins := map[string]*regexp.Regexp{
		"Dockerfile": regexp.MustCompile(`(?m)^ARG RESTIC_VERSION=(\S+)\r?$`),
		filepath.Join(".github", "actions", "setup-restic", "action.yml"): regexp.MustCompile(`(?m)^\s*version=(\S+)\r?$`),
	}
	var restic *executable
	for i := range executables {
		if executables[i].name == "restic" {
			restic = &executables[i]
		}
	}
	if restic == nil {
		t.Fatal("restic is not in executables")
	}
	for file, re := range pins {
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s: no restic version pin", file)
		}
		if want := "/blob/v" + string(m[1]) + "/"; !strings.Contains(restic.source, want) {
			t.Errorf("%s installs restic %s, but the license text is from %s", file, m[1], restic.source)
		}
	}
}

func TestRender(t *testing.T) {
	out := string(render(
		[]entry{{name: "Go standard library", artifacts: []string{artifactGos3}, texts: []text{{"LICENSE", "BSD"}}}},
		[]entry{{name: "pkg", license: "MIT", artifacts: []string{artifactGos3, artifactGos3UI}, source: "https://example.com/LICENSE",
			texts: []text{{"LICENSE", "MIT text"}, {"NOTICE", "Notice"}}}},
		[]entry{{name: "tool", license: "BSD-2-Clause", artifacts: []string{artifactGos3Image}, texts: []text{{"LICENSE", "BSD text"}}}},
	))
	for _, want := range []string{
		"  Go standard library  gos3\n",
		"  pkg                  gos3, gos3-ui\n",
		"Executables\n  tool                 gos3-image\n",
		"\ntool\nLicense: BSD-2-Clause\nIncluded in: gos3-image\n",
		"\npkg\nLicense: MIT\nIncluded in: gos3, gos3-ui\nLicense text from https://example.com/LICENSE",
		"\n--- LICENSE ---\n\nMIT text\n\n--- NOTICE ---\n\nNotice\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render output lacks %q:\n%s", want, out)
		}
	}
}

func TestFirstDiffLine(t *testing.T) {
	if got := firstDiffLine([]byte("a\nb\nc"), []byte("a\nx\nc")); got != 2 {
		t.Fatalf("firstDiffLine = %d, want 2", got)
	}
	if got := firstDiffLine([]byte("a"), []byte("a\nb")); got != 2 {
		t.Fatalf("firstDiffLine = %d, want 2", got)
	}
}
