package main

import (
	"strings"
	"testing"
)

func TestResticAPIErrorCannotInjectTerminalEscapes(t *testing.T) {
	err := apiError(403, []byte(`{"error":{"message":"denied\u001b[2J\nnew line","code":"bad\u001b]0;title\u0007"}}`))
	if strings.ContainsAny(err.Error(), "\x1b\n\r\x07") {
		t.Fatalf("unsafe terminal error: %q", err)
	}
}
