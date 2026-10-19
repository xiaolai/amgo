package render

import (
	"strings"
	"testing"
)

func TestSafeNeutralizesEscapes(t *testing.T) {
	in := "hello\x1b[31mred\x1b[0m\nnext\tline"
	out := Safe(in)
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("raw escape survived: %q", out)
	}
	if !strings.Contains(out, `\x1b[31m`) || !strings.Contains(out, "\nnext\tline") {
		t.Fatalf("out=%q", out)
	}
}

func TestSafeNeutralizesC0(t *testing.T) {
	out := Safe("a\x00b\x07c")
	if strings.ContainsRune(out, 0) || strings.ContainsRune(out, 7) {
		t.Fatalf("control survived: %q", out)
	}
	if !strings.Contains(out, `\x00`) || !strings.Contains(out, `\x07`) {
		t.Fatalf("out=%q", out)
	}
}
