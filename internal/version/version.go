// Package version reports the gos3 build version. The values are set at
// link time, e.g.
//
//	go build -ldflags "-X github.com/seanmmitchell/gos3/internal/version.Version=v0.1.0 \
//	  -X github.com/seanmmitchell/gos3/internal/version.Commit=abc1234 \
//	  -X github.com/seanmmitchell/gos3/internal/version.Date=2026-10-10T12:00:00Z"
//
// (see the Makefile). When Commit or Date are not set, they are filled from
// the VCS information the Go toolchain embeds (go build in a git checkout).
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// Link-time variables. Version defaults to "dev".
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the resolved build information.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	Modified  bool   `json:"modified,omitempty"` // built from a dirty tree (VCS info only)
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

var (
	once     sync.Once
	resolved Info
)

// Get returns the build information: the link-time values, completed from
// the embedded VCS build settings when they were not set.
func Get() Info {
	once.Do(func() {
		resolved = Info{
			Version:   Version,
			Commit:    Commit,
			Date:      Date,
			GoVersion: runtime.Version(),
			Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		}
		if resolved.Version == "" {
			resolved.Version = "dev"
		}
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if resolved.Commit == "" {
					resolved.Commit = s.Value
				}
			case "vcs.time":
				if resolved.Date == "" {
					resolved.Date = s.Value
				}
			case "vcs.modified":
				resolved.Modified = s.Value == "true" && Commit == ""
			}
		}
	})
	return resolved
}

// Short returns the version alone (e.g. "v0.1.0" or "dev"), as reported in
// /_gos3/info and the Server header.
func Short() string { return Get().Version }

// String returns a one-line human readable description, e.g.
// "gos3 v0.1.0 (commit abc1234, built 2026-10-10T12:00:00Z, go1.25.0 linux/amd64)".
func String() string {
	i := Get()
	s := "gos3 " + i.Version + " ("
	if i.Commit != "" {
		c := i.Commit
		if len(c) > 12 {
			c = c[:12]
		}
		if i.Modified {
			c += "-dirty"
		}
		s += "commit " + c + ", "
	}
	if i.Date != "" {
		s += "built " + i.Date + ", "
	}
	return s + fmt.Sprintf("%s %s)", i.GoVersion, i.Platform)
}
