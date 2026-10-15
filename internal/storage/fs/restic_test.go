package fs

import (
	"context"
	"os/exec"

	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Helpers for tests that run restic against a bucket's repository directly.

// resticSettings returns the recovery record form of how restic reaches the
// repository of the bucket with ID id.
func (b *Backend) resticSettings(id string) (resticRepository, error) {
	loc, err := b.resticLocation(id)
	if err != nil {
		return resticRepository{}, err
	}
	return resticRepository{Repository: loc.Repository, Password: loc.Password}, nil
}

// resticCommand returns a restic command with args for the repository of
// the bucket with ID id.
func (b *Backend) resticCommand(ctx context.Context, id string, args ...string) (*exec.Cmd, error) {
	loc, err := b.resticLocation(id)
	if err != nil {
		return nil, err
	}
	return b.resticRuntime().Command(ctx, loc, args...)
}

// resticCommandFor returns a restic command with args for the repository
// of the recovery record cfg, as an operator recovering an archive with
// only that record would run it.
func resticCommandFor(ctx context.Context, cfg resticRepository, args ...string) (*exec.Cmd, error) {
	return new(restic.Runtime).Command(ctx, restic.Location{Repository: cfg.Repository, Password: cfg.Password}, args...)
}
