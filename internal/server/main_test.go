package server

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// TestMain lets the test binary act as the fake restic of restictest.
func TestMain(m *testing.M) { restictest.Main(m) }
