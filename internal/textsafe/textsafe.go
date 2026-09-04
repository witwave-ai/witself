// Package textsafe provides shared predicates for text that crosses display or
// terminal trust boundaries.
package textsafe

// IsBidiControl reports whether r is one of the bidirectional display controls
// rejected by Witself's CLI and control-plane sanitizers. This is intentionally
// the strictest union of the former predicates: Arabic Letter Mark, left/right
// marks, embedding/override controls (including PDF), and isolate controls
// (including PDI). Keeping the union here prevents one display boundary from
// silently accepting a spoofing control rejected by another.
func IsBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}
