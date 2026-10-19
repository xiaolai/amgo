// Package render makes untrusted text safe for a terminal without changing the
// lossless JSON representation.
package render

import (
	"fmt"
	"strings"
)

// Safe neutralizes escape and control characters while preserving newlines and
// tabs. Peer-controlled text must pass through this before human output.
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
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
