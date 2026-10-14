//go:build noui

package webui

import "io/fs"

// FS returns nil because this build contains no embedded UI assets.
func FS() fs.FS { return nil }
