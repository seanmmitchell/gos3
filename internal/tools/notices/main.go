// Command notices generates THIRD_PARTY_NOTICES: the copyright and license
// notices of the third-party code that gos3 distributes.
//
//   - Go: the modules linked into the gos3 binary on every release platform
//     (`go list -deps ./cmd/gos3`), and the Go standard library.
//   - npm: the packages in the web UI bundles, as recorded in
//     web/.notices/*.json by web/modules/third-party-notices.ts during
//     `pnpm generate:embed` (the UI embedded in the binary) and `pnpm build`
//     (the standalone UI image).
//   - Executables: the third-party programs bundled in the gos3 container
//     images next to the gos3 binary (restic, see the Dockerfile), with
//     license texts kept in fallback/.
//
// Run it from the repository root through `make notices`, which builds both
// web UI variants first. With -check it compares instead of writing and
// fails when the file is out of date; CI runs it after the web UI builds.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Artifacts an entry can be included in (see header).
const (
	artifactGos3      = "gos3"
	artifactGos3Image = "gos3-image"
	artifactGos3UI    = "gos3-ui"
)

// platforms are the GOOS/GOARCH pairs of the release binaries
// (.github/workflows/release.yml); the linked modules can differ by platform.
var platforms = []string{
	"linux/amd64", "linux/arm64",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
}

// manifests are the web UI build records (web/modules/third-party-notices.ts)
// and the artifact each one ships in.
var manifests = []struct{ file, artifact string }{
	{"embedded.json", artifactGos3},
	{"standalone.json", artifactGos3UI},
}

// fallback is the license text of a shipped package that does not include
// one, copied from its upstream repository.
type fallback struct {
	file   string // in fallback/
	source string // upstream URL of the copied file
}

var fallbacks = map[string]fallback{
	"@iconify-json/lucide": {"lucide.LICENSE", "https://github.com/lucide-icons/lucide/blob/70562c1ee1c4fdcf736fe97bc893fb8511927934/LICENSE"},
	"vaul-vue":             {"vaul-vue.LICENSE", "https://github.com/unovue/vaul-vue/blob/1b1f6dfdba6a775410508884097443d35c9a8690/LICENSE"},
}

//go:embed fallback
var fallbackFS embed.FS

// executable is a third-party program that the gos3 container images ship
// next to the gos3 binary. Only the executable is downloaded, so its license
// text is copied from its upstream repository into fallback/.
type executable struct {
	name    string
	license string // SPDX identifier
	fallback
}

// executables are the bundled programs. Keep each source URL at the version
// the Dockerfile installs (TestExecutableVersionsMatchDockerfile checks it).
var executables = []executable{
	{"restic", "BSD-2-Clause", fallback{"restic.LICENSE", "https://github.com/restic/restic/blob/v0.19.1/LICENSE"}},
}

// excerpts limits a package's license file to the text before a marker.
// Vite's LICENSE.md continues with the licenses of the dependencies bundled
// into Vite itself, which run at build time; the web UI only contains the
// helpers Vite injects, which are Vite's own code.
var excerpts = map[string]string{
	"vite": "\n# Licenses of bundled dependencies",
}

// licenseFile matches license and notice file names: LICENSE, LICENSE.md,
// LICENSE-MIT, LICENCE.txt, COPYING, NOTICE, ...
var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|unlicense)([._-][a-z0-9._-]*)?$`)

// entry is one third-party component in the notices.
type entry struct {
	name      string   // Go module path or npm package name
	license   string   // declared license (npm only)
	source    string   // upstream URL when the texts are not from the package
	artifacts []string // artifacts that include it
	texts     []text   // license and notice files
}

type text struct {
	file string
	body string
}

func main() {
	out := flag.String("o", "THIRD_PARTY_NOTICES", "output file")
	webDir := flag.String("web", filepath.Join("web", ".notices"), "directory of the web UI build records")
	check := flag.Bool("check", false, "fail if the output file is out of date instead of writing it")
	flag.Parse()

	if err := run(*out, *webDir, *check); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(out, webDir string, check bool) error {
	if _, err := os.Stat(filepath.Join("cmd", "gos3")); err != nil {
		return errors.New("run from the repository root")
	}
	goEntries, err := goEntries()
	if err != nil {
		return err
	}
	npmEntries, err := npmEntries(webDir)
	if err != nil {
		return err
	}
	exeEntries, err := executableEntries()
	if err != nil {
		return err
	}
	data := render(goEntries, npmEntries, exeEntries)
	if !check {
		return os.WriteFile(out, data, 0o644)
	}
	current, err := os.ReadFile(out)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !bytes.Equal(current, data) {
		return fmt.Errorf("%s is out of date (first difference at line %d): run `make notices` and commit the result", out, firstDiffLine(current, data))
	}
	return nil
}

// goEntries lists the Go standard library and the modules linked into the
// gos3 binary on any release platform.
func goEntries() ([]entry, error) {
	goroot, err := goOutput(nil, "env", "GOROOT")
	if err != nil {
		return nil, err
	}
	std, err := readTexts(strings.TrimSpace(goroot), "")
	if err != nil {
		return nil, err
	}
	if len(std) == 0 {
		return nil, errors.New("no LICENSE in GOROOT")
	}
	entries := []entry{{name: "Go standard library", artifacts: []string{artifactGos3}, texts: std}}

	modules := map[string]string{} // path -> directory
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		listed, err := goOutput([]string{"GOOS=" + goos, "GOARCH=" + goarch, "CGO_ENABLED=0"},
			"list", "-deps", "-f", `{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Dir}}{{end}}{{end}}`, "./cmd/gos3")
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(listed, "\n") {
			path, dir, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			if dir == "" {
				return nil, fmt.Errorf("module %s is not in the module cache (go mod download)", path)
			}
			modules[path] = dir
		}
	}
	for _, path := range sortedKeys(modules) {
		texts, err := readTexts(modules[path], "")
		if err != nil {
			return nil, err
		}
		if len(texts) == 0 {
			return nil, fmt.Errorf("Go module %s has no license file", path)
		}
		entries = append(entries, entry{name: path, artifacts: []string{artifactGos3}, texts: texts})
	}
	return entries, nil
}

func goOutput(env []string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// manifest is a web UI build record (.notices/<kind>.json).
type manifest struct {
	Packages []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dir     string `json:"dir"`
	} `json:"packages"`
}

// npmEntries lists the packages in the web UI builds, one entry per package
// name (versions are left out so that routine updates rarely change the
// file; the texts of all shipped versions are kept).
func npmEntries(webDir string) ([]entry, error) {
	byName := map[string]*entry{}
	for _, m := range manifests {
		path := filepath.Join(webDir, m.file)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s is missing: build the web UI first (`make notices` does)", path)
		}
		if err != nil {
			return nil, err
		}
		var man manifest
		if err := json.Unmarshal(data, &man); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, pkg := range man.Packages {
			if pkg.Dir == "" {
				return nil, fmt.Errorf("%s: npm package %s@%s ships in %s, but the build could not locate its directory (see web/modules/third-party-notices.ts)", path, pkg.Name, pkg.Version, m.artifact)
			}
			e := byName[pkg.Name]
			if e == nil {
				e = &entry{name: pkg.Name}
				byName[pkg.Name] = e
			}
			if !slices.Contains(e.artifacts, m.artifact) {
				e.artifacts = append(e.artifacts, m.artifact)
			}
			license, err := declaredLicense(pkg.Dir)
			if err != nil {
				return nil, err
			}
			if license != "" && !slices.Contains(strings.Split(e.license, " / "), license) {
				e.license = strings.TrimPrefix(e.license+" / "+license, " / ")
			}
			texts, err := readTexts(pkg.Dir, excerpts[pkg.Name])
			if err != nil {
				return nil, err
			}
			if len(texts) == 0 {
				fb, ok := fallbacks[pkg.Name]
				if !ok {
					return nil, fmt.Errorf("npm package %s@%s (%s) has no license file: add its upstream license text to internal/tools/notices/fallback", pkg.Name, pkg.Version, pkg.Dir)
				}
				body, err := fallbackFS.ReadFile("fallback/" + fb.file)
				if err != nil {
					return nil, err
				}
				texts = []text{{file: "LICENSE", body: normalize(string(body))}}
				e.source = fb.source
			}
			for _, t := range texts {
				if !slices.Contains(e.texts, t) {
					e.texts = append(e.texts, t)
				}
			}
		}
	}
	var entries []entry
	for _, name := range sortedKeys(byName) {
		e := byName[name]
		slices.Sort(e.artifacts)
		entries = append(entries, *e)
	}
	return entries, nil
}

// executableEntries lists the programs bundled in the container images.
func executableEntries() ([]entry, error) {
	var entries []entry
	for _, x := range executables {
		body, err := fallbackFS.ReadFile("fallback/" + x.file)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{
			name:      x.name,
			license:   x.license,
			source:    x.source,
			artifacts: []string{artifactGos3Image},
			texts:     []text{{file: "LICENSE", body: normalize(string(body))}},
		})
	}
	return entries, nil
}

// declaredLicense returns the license field of a package.json: an SPDX
// expression, or the legacy {type} object or licenses array.
func declaredLicense(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", err
	}
	type typed struct {
		Type string `json:"type"`
	}
	var pkg struct {
		License  json.RawMessage `json:"license"`
		Licenses []typed         `json:"licenses"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", fmt.Errorf("%s/package.json: %w", dir, err)
	}
	var s string
	if json.Unmarshal(pkg.License, &s) == nil && s != "" {
		return s, nil
	}
	var t typed
	if json.Unmarshal(pkg.License, &t) == nil && t.Type != "" {
		return t.Type, nil
	}
	var types []string
	for _, l := range pkg.Licenses {
		types = append(types, l.Type)
	}
	return strings.Join(types, " OR "), nil
}

// readTexts returns the license and notice files in a directory, license
// files first. A non-empty cut ends each text at that marker.
func readTexts(dir, cut string) ([]text, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var texts []text
	for _, de := range des {
		if de.IsDir() || !licenseFile.MatchString(de.Name()) {
			continue
		}
		switch strings.ToLower(filepath.Ext(de.Name())) {
		case ".js", ".mjs", ".cjs", ".ts", ".json", ".map", ".html":
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		body := string(data)
		if cut != "" {
			body, _, _ = strings.Cut(body, cut)
		}
		if body = normalize(body); body != "" {
			texts = append(texts, text{file: de.Name(), body: body})
		}
	}
	slices.SortFunc(texts, func(a, b text) int {
		if an, bn := isNotice(a.file), isNotice(b.file); an != bn {
			if an {
				return 1
			}
			return -1
		}
		return strings.Compare(a.file, b.file)
	})
	return texts, nil
}

func isNotice(file string) bool {
	return strings.HasPrefix(strings.ToLower(file), "notice")
}

// normalize uses LF line endings, removes trailing whitespace and leading or
// trailing blank lines.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

const rule = "================================================================================"

const header = `THIRD-PARTY SOFTWARE NOTICES
============================

gos3 is licensed under the MIT License (see LICENSE). It includes the
third-party software listed below, which is used under the license terms
reproduced in this file.

Each entry names the artifacts that include it:

  gos3        the gos3 binary, as shipped in the release archives and the gos3
              container image, including its embedded web UI
  gos3-image  the gos3 container images (default and headless) only, as a
              separate executable next to the gos3 binary
  gos3-ui     the standalone web UI container image

This file is generated by ` + "`make notices`" + ` (internal/tools/notices) from the
Go modules linked into gos3, the npm packages in the web UI bundles and the
executables bundled in the container images. Do not edit it by hand.
`

// render formats the notices file.
func render(goEntries, npmEntries, exeEntries []entry) []byte {
	var b strings.Builder
	b.WriteString(header)

	width := 0
	for _, e := range slices.Concat(goEntries, npmEntries, exeEntries) {
		width = max(width, len(e.name))
	}
	b.WriteString("\n\nCONTENTS\n\n")
	for _, group := range []struct {
		title   string
		entries []entry
	}{{"Go", goEntries}, {"npm", npmEntries}, {"Executables", exeEntries}} {
		fmt.Fprintf(&b, "%s\n", group.title)
		for _, e := range group.entries {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, e.name, strings.Join(e.artifacts, ", "))
		}
		b.WriteString("\n")
	}

	for _, e := range slices.Concat(goEntries, npmEntries, exeEntries) {
		fmt.Fprintf(&b, "\n%s\n%s\n", rule, e.name)
		if e.license != "" {
			fmt.Fprintf(&b, "License: %s\n", e.license)
		}
		fmt.Fprintf(&b, "Included in: %s\n", strings.Join(e.artifacts, ", "))
		if e.source != "" {
			fmt.Fprintf(&b, "License text from %s (the package does not include it)\n", e.source)
		}
		fmt.Fprintf(&b, "%s\n", rule)
		for _, t := range e.texts {
			if len(e.texts) > 1 {
				fmt.Fprintf(&b, "\n--- %s ---\n", t.file)
			}
			fmt.Fprintf(&b, "\n%s\n", t.body)
		}
	}
	return []byte(b.String())
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// firstDiffLine returns the 1-based number of the first line that differs.
func firstDiffLine(a, b []byte) int {
	al, bl := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := range min(len(al), len(bl)) {
		if al[i] != bl[i] {
			return i + 1
		}
	}
	return min(len(al), len(bl)) + 1
}
