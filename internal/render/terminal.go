// Package render makes untrusted text safe for a terminal without changing the
// lossless JSON representation.
package render

import (
	"fmt"
	"strings"
)

// Safe neutralizes escape and control characters while preserving newlines and
// tabs. Peer-controlled text must pass through this before human output.
//
// C1 controls and Unicode bidirectional formatting characters are escaped as
// well: some terminals accept 8-bit CSI (U+009B), and bidi overrides can
// reorder displayed text so a message no longer reads as it is stored. The
// JSON representation is unaffected.
func Safe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == 0x1b:
			b.WriteString(`\x1b`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		case isBidiControl(r) || r == 0x2028 || r == 0x2029:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isBidiControl reports Unicode bidirectional formatting characters that can
// visually reorder terminal text.
func isBidiControl(r rune) bool {
	switch r {
	case 0x061c, // ARABIC LETTER MARK
		0x200e, 0x200f, // LEFT-TO-RIGHT MARK, RIGHT-TO-LEFT MARK
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, // embeddings and overrides
		0x2066, 0x2067, 0x2068, 0x2069: // isolates
		return true
	}
	return false
}
