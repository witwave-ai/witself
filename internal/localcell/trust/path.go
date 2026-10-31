package trust

import (
	"strings"
	"unicode/utf8"
)

// PathUnicodeVersion freezes the normalization tables used by manifest v1.
const PathUnicodeVersion = "16.0.0"

// ValidPath accepts only bounded relative forward-slash paths in Unicode NFC.
// A colon is forbidden anywhere to exclude drive prefixes and Windows streams.
func ValidPath(name string) bool {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00:") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return isNFC(name)
}

func decomposeRune(r rune, dst *[]rune) {
	const sBase, lBase, vBase, tBase, nCount, tCount, sCount = 0xAC00, 0x1100, 0x1161, 0x11A7, 588, 28, 11172
	if r >= sBase && r < sBase+sCount {
		s := r - sBase
		*dst = append(*dst, lBase+s/nCount, vBase+(s%nCount)/tCount)
		if t := s % tCount; t != 0 {
			*dst = append(*dst, tBase+t)
		}
		return
	}
	if decomposition, ok := canonicalDecomposition[r]; ok {
		for _, part := range decomposition {
			decomposeRune(part, dst)
		}
		return
	}
	*dst = append(*dst, r)
}

func composeRunes(a, b rune) (rune, bool) {
	const sBase, lBase, vBase, tBase, lCount, vCount, tCount, sCount = 0xAC00, 0x1100, 0x1161, 0x11A7, 19, 21, 28, 11172
	if a >= lBase && a < lBase+lCount && b >= vBase && b < vBase+vCount {
		return sBase + (a-lBase)*vCount*tCount + (b-vBase)*tCount, true
	}
	if a >= sBase && a < sBase+sCount && (a-sBase)%tCount == 0 && b > tBase && b < tBase+tCount {
		return a + b - tBase, true
	}
	result, ok := canonicalComposition[[2]rune{a, b}]
	return result, ok
}

func isNFC(s string) bool {
	decomposed := make([]rune, 0, len(s))
	for _, r := range s {
		decomposeRune(r, &decomposed)
	}
	// Stable canonical ordering, bounded by the 1024-byte path limit.
	for i := 1; i < len(decomposed); i++ {
		c := canonicalCombiningClass[decomposed[i]]
		if c == 0 {
			continue
		}
		for j := i; j > 0 && canonicalCombiningClass[decomposed[j-1]] > c; j-- {
			decomposed[j], decomposed[j-1] = decomposed[j-1], decomposed[j]
		}
	}
	if len(decomposed) == 0 {
		return true
	}
	composed := make([]rune, 0, len(decomposed))
	composed = append(composed, decomposed[0])
	starter := 0
	previous := canonicalCombiningClass[decomposed[0]]
	for _, r := range decomposed[1:] {
		class := canonicalCombiningClass[r]
		if combined, ok := composeRunes(composed[starter], r); ok && (previous == 0 || previous < class) {
			composed[starter] = combined
			continue
		}
		if class == 0 {
			starter = len(composed)
		}
		composed = append(composed, r)
		previous = class
	}
	return string(composed) == s
}
