//go:build !windows

package config

// restrictPath is a no-op outside Windows: file modes (0600 files, 0700
// directories) already keep secrets private there.
func restrictPath(string, bool) error { return nil }
