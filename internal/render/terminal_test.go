package render

import (
	"fmt"
	"strings"
	"testing"
)

func runes(rs ...rune) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteRune(r)
	}
	return b.String()
}

// escapedUnicode and escapedHex build the visible sequences Safe emits without
// putting a backslash escape in this source file.
func escapedUnicode(r rune) string {
	return fmt.Sprintf("%cu%04x", 92, r)
}

func escapedHex(r rune) string {
	return fmt.Sprintf("%cx%02x", 92, r)
}

func TestSafeNeutralizesEscapes(t *testing.T) {
	esc := runes(0x1b)
	in := "hello" + esc + "[31mred" + esc + "[0m\nnext\tline"
	out := Safe(in)
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("raw escape survived: %q", out)
	}
	if !strings.Contains(out, escapedHex(0x1b)+"[31m") || !strings.Contains(out, "\nnext\tline") {
		t.Fatalf("out=%q", out)
	}
}

func TestSafeNeutralizesC0(t *testing.T) {
	out := Safe("a" + runes(0x00) + "b" + runes(0x07) + "c")
	if strings.ContainsRune(out, 0) || strings.ContainsRune(out, 7) {
		t.Fatalf("control survived: %q", out)
	}
	if !strings.Contains(out, escapedHex(0x00)) || !strings.Contains(out, escapedHex(0x07)) {
		t.Fatalf("out=%q", out)
	}
}

func TestSafeNeutralizesC1(t *testing.T) {
	out := Safe("a" + runes(0x9b) + "31mb" + runes(0x85) + "c")
	for _, r := range []rune{0x9b, 0x85} {
		if strings.ContainsRune(out, r) {
			t.Fatalf("C1 control %U survived: %q", r, out)
		}
	}
	if !strings.Contains(out, escapedUnicode(0x9b)+"31m") || !strings.Contains(out, escapedUnicode(0x85)) {
		t.Fatalf("out=%q", out)
	}
}

func TestSafeNeutralizesBidiControls(t *testing.T) {
	in := "invoice" + runes(0x202e, 0x202c, 0x200f, 0x061c, 0x2066) + "text" + runes(0x2069)
	out := Safe(in)
	for _, r := range []rune{0x202e, 0x202c, 0x200f, 0x061c, 0x2066, 0x2069} {
		if strings.ContainsRune(out, r) {
			t.Fatalf("bidi control %U survived: %q", r, out)
		}
	}
	if !strings.Contains(out, escapedUnicode(0x202e)) || !strings.Contains(out, escapedUnicode(0x2069)) {
		t.Fatalf("out=%q", out)
	}
}

func TestSafeNeutralizesLineSeparators(t *testing.T) {
	out := Safe("one" + runes(0x2028) + "two" + runes(0x2029) + "three")
	if strings.ContainsRune(out, 0x2028) || strings.ContainsRune(out, 0x2029) {
		t.Fatalf("separator survived: %q", out)
	}
	if !strings.Contains(out, escapedUnicode(0x2028)) || !strings.Contains(out, escapedUnicode(0x2029)) {
		t.Fatalf("out=%q", out)
	}
}

func TestSafePreservesOrdinaryUnicode(t *testing.T) {
	in := "你好 مرحبا 👨‍👩‍👧 café"
	if out := Safe(in); out != in {
		t.Fatalf("ordinary text changed: %q", out)
	}
}
