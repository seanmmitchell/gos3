package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	s := String()
	if !strings.HasPrefix(s, "gos3 "+Short()+" (") {
		t.Fatalf("String() = %q", s)
	}
	if !strings.Contains(s, runtime.Version()) || !strings.Contains(s, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("String() = %q lacks go version/platform", s)
	}
	if Get().Version == "" {
		t.Fatal("empty version")
	}
}
