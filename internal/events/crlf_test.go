package events

import (
	"strings"
	"testing"
)

func TestHardWrapDropsCR(t *testing.T) {
	out := strings.Join(hardWrap("C:\\path\\to\\file.exe\r", 80), "\n")
	if strings.Contains(out, "\r") {
		t.Fatalf("CR survived hardWrap: %q", out)
	}
	if out != "C:\\path\\to\\file.exe" {
		t.Fatalf("unexpected: %q", out)
	}
}
