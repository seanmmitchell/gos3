//go:build !linux && !darwin

package admin

// diskUsage is not supported on this platform; GET /stats reports
// "disk": null.
func diskUsage(path string) *diskStats { return nil }
