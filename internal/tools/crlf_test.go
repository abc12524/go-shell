package tools

import (
	"strings"
	"testing"
)

func TestNormalizeCRLF(t *testing.T) {
	in := "C:\\a\\b.exe\r\nC:\\c.exe\r\nlast"
	out := normalizeCRLF(in)
	if strings.Contains(out, "\r") {
		t.Fatalf("CR survived: %q", out)
	}
	if out != "C:\\a\\b.exe\nC:\\c.exe\nlast" {
		t.Fatalf("unexpected: %q", out)
	}
	lone := normalizeCRLF("50%\rdone")
	if lone != "50%\ndone" {
		t.Fatalf("lone CR: %q", lone)
	}
}
