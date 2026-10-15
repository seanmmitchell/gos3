package memory

import "github.com/seanmmitchell/gos3/internal/storage/index"

// Names of the index package under the names the tests of this package
// use.
var (
	newVersionID       = index.NewVersionID
	stampFromVersionID = index.StampFromVersionID
)

const versionIDLength = index.VersionIDLength
