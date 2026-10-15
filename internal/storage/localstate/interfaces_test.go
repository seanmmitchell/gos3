package localstate_test

import (
	"github.com/seanmmitchell/gos3/internal/storage/localstate"
	"github.com/seanmmitchell/gos3/internal/storage/native"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// A Store serves wherever the records of an s3 backing live in its home:
// as the native store's StateStore and as an s3store aux namespace.
var (
	_ native.StateStore = (*localstate.Store)(nil)
	_ s3store.AuxStore  = (*localstate.Store)(nil)
)
