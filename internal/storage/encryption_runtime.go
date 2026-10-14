package storage

import "errors"

// ErrEncryptionUnavailable reports that a bucket's encryption could not
// serve a request: its key is missing, or its archive provider (restic) is
// not installed, too old, unreachable or failing. Backends wrap it (with the
// details for the server log), so callers test with errors.Is.
//
// The condition belongs to one bucket's encryption setup, not to the health
// of the backend storing the bucket: a router must not mark the backend
// down, and fail over every other bucket on it, because of it.
var ErrEncryptionUnavailable = errors.New("storage: encryption provider unavailable")

// PreparingReader is an optional extension of the readers returned by
// Backend.OpenObject whose data must be fetched before it can be read (a
// restic version is restored from its repository on first use). Opening
// such an object is cheap, so requests that end without reading it (a
// failed precondition, a HEAD, an unsatisfiable range) cost nothing.
//
// Prepare fetches the data now, so that a caller can report a failure
// before it starts a response. Reads prepare implicitly; Prepare may be
// called more than once and returns the error a read would return.
type PreparingReader interface {
	Prepare() error
}
