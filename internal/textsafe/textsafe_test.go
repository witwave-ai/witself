package textsafe

import "testing"

func TestIsBidiControlStrictestUnion(t *testing.T) {
	controls := []rune{
		'\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
	}
	for _, r := range controls {
		if !IsBidiControl(r) {
			t.Errorf("IsBidiControl(%U) = false, want true", r)
		}
	}

	nonControls := []rune{
		'\u061b', '\u061d', '\u200d', '\u2010',
		'\u2029', '\u202f', '\u2065', '\u206a',
	}
	for _, r := range nonControls {
		if IsBidiControl(r) {
			t.Errorf("IsBidiControl(%U) = true, want false", r)
		}
	}
}
