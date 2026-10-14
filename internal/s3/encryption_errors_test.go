package s3

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// G1-a, G4-b: encryption-provider failures and unsupported operations map
// to S3 errors in mapStorageError, wherever the backend returns them.
func TestMapEncryptionAndUnsupportedErrors(t *testing.T) {
	err := s3err.As(mapStorageError(fmt.Errorf("restic: archive object: exit status 1: %w", storage.ErrEncryptionUnavailable)))
	if err.Code != s3err.ErrInternalError.Code || err.Message == s3err.ErrInternalError.Message {
		t.Fatalf("encryption failure: %+v", err)
	}
	err = s3err.As(mapStorageError(fmt.Errorf("backing %q cannot store data encrypted (restic): %w", "m", errors.ErrUnsupported)))
	if err.Code != s3err.ErrNotImplemented.Code || err.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("unsupported: %+v", err)
	}
}
