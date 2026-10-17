package s3store

import (
	"errors"
	"strings"
	"testing"
)

func TestMarkerRejectsPathBackingID(t *testing.T) {
	for _, id := range []string{"../../escape", `..\..\escape`, `/absolute`, `C:\escape`, `\\server\share`, ".", "..", "not-an-id", strings.Repeat("A", 32), strings.Repeat("a", 33)} {
		m := Marker{Format: markerFormat, Kind: markerKind, BackingID: id}
		if err := m.validate(""); !errors.Is(err, ErrMarkerMismatch) {
			t.Errorf("unsafe ID %q accepted: %v", id, err)
		}
	}
	m := Marker{Format: markerFormat, Kind: markerKind, BackingID: strings.Repeat("a", 32)}
	if err := m.validate(m.BackingID); err != nil {
		t.Fatalf("generated ID refused: %v", err)
	}
}
