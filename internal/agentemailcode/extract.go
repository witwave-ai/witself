// Package agentemailcode deterministically extracts conservative verification
// code candidates from already-decoded email text.
package agentemailcode

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minimumCodeDigits              = 4
	maximumCodeDigits              = 8
	maximumAssociationGap          = 64
	maximumPhraseSeparatorBytes    = 32
	maximumKeywordBytes            = len("one") + len("time") + len("code") + 2*maximumPhraseSeparatorBytes
	maximumURLTokenInspectionBytes = 256
)

// MaximumCandidates is the maximum number of distinct values returned by an
// extraction. ExtractBounded reports additional distinct values as overflow.
const MaximumCandidates = 32

// Candidate is one distinct verification-code candidate. Occurrences counts
// only appearances that are independently associated with a supported keyword.
type Candidate struct {
	Value       string
	Occurrences int
}

// Result is the bounded output of ExtractBounded. Overflow reports that at
// least one additional distinct candidate was recognized but omitted.
type Result struct {
	Candidates []Candidate
	Overflow   bool
}

// Extract returns standalone ASCII numeric verification-code candidates in
// first-seen order. A candidate must be locally connected to a supported
// keyword using a deliberately small grammar. Extract does not interpret links,
// trust email metadata, choose a candidate, or consume a code. The returned
// values are capped at MaximumCandidates; callers that need to distinguish a
// complete result from an overflow use ExtractBounded.
func Extract(text string) []Candidate {
	return ExtractBounded(text).Candidates
}

// ExtractBounded extracts at most MaximumCandidates distinct values and
// reports whether further distinct values were omitted. It scans the complete
// input even after overflow so repetition counts for retained values remain
// accurate.
func ExtractBounded(text string) Result {
	var result Result
	indexes := make(map[string]int, MaximumCandidates)

	for start := 0; start < len(text); {
		if !isASCIIDigit(text[start]) {
			start++
			continue
		}

		end := start + 1
		for end < len(text) && isASCIIDigit(text[end]) {
			end++
		}
		digits := end - start
		if digits >= minimumCodeDigits && digits <= maximumCodeDigits &&
			standaloneNumber(text, start, end) &&
			!partOfStructuredNumber(text, start, end) &&
			!embeddedInURLLikeToken(text, start, end) &&
			locallyAssociated(text, start, end) {
			value := text[start:end]
			if index, ok := indexes[value]; ok {
				result.Candidates[index].Occurrences++
			} else if len(result.Candidates) == MaximumCandidates {
				result.Overflow = true
			} else {
				// Do not retain the decoded email's potentially 1 MiB backing
				// string merely because one short candidate was returned.
				value = strings.Clone(value)
				indexes[value] = len(result.Candidates)
				result.Candidates = append(result.Candidates, Candidate{Value: value, Occurrences: 1})
			}
		}
		start = end
	}

	return result
}

func locallyAssociated(text string, start, end int) bool {
	left := start - maximumAssociationGap - maximumKeywordBytes
	if left < 0 {
		left = 0
	}
	for i := left; i < start; i++ {
		keywordEnd, ok := keywordAt(text, i)
		if !ok || keywordEnd > start || start-keywordEnd > maximumAssociationGap {
			continue
		}
		if allowedKeywordToCodeGap(text[keywordEnd:start]) {
			return true
		}
	}

	right := end + maximumAssociationGap + 1
	if right > len(text) {
		right = len(text)
	}
	for i := end; i < right; i++ {
		_, ok := keywordAt(text, i)
		if ok && allowedCodeToKeywordGap(text[end:i]) {
			return true
		}
	}
	return false
}

func keywordAt(text string, start int) (int, bool) {
	if start < 0 || start >= len(text) || isWordBefore(text, start) {
		return 0, false
	}

	var end int
	var ok bool
	switch asciiLower(text[start]) {
	case 'v':
		end, ok = phraseAt(text, start, "verification", "code")
	case 's':
		end, ok = phraseAt(text, start, "security", "code")
	case 'o':
		if end, ok = phraseAt(text, start, "one", "time", "code"); !ok {
			end, ok = asciiWordAt(text, start, "otp")
		}
	case 'p':
		if end, ok = asciiWordAt(text, start, "passcode"); !ok {
			end, ok = asciiWordAt(text, start, "pin")
		}
	case 'c':
		end, ok = asciiWordAt(text, start, "code")
	}
	if !ok || isWordAt(text, end) {
		return 0, false
	}
	return end, true
}

func phraseAt(text string, start int, words ...string) (int, bool) {
	position := start
	for index, word := range words {
		if index > 0 {
			var ok bool
			position, ok = phraseSeparator(text, position)
			if !ok {
				return 0, false
			}
		}
		var ok bool
		position, ok = asciiWordAt(text, position, word)
		if !ok {
			return 0, false
		}
	}
	return position, true
}

func phraseSeparator(text string, start int) (int, bool) {
	position := start
	for position < len(text) {
		size, ok := whitespaceAt(text, position)
		if !ok {
			break
		}
		position += size
		if position-start > maximumPhraseSeparatorBytes {
			return 0, false
		}
	}
	hadWhitespace := position > start
	if size, ok := dashAt(text, position); ok {
		position += size
		if position-start > maximumPhraseSeparatorBytes {
			return 0, false
		}
		for position < len(text) {
			size, ok := whitespaceAt(text, position)
			if !ok {
				break
			}
			position += size
			if position-start > maximumPhraseSeparatorBytes {
				return 0, false
			}
		}
		return position, true
	}
	return position, hadWhitespace
}

func asciiWordAt(text string, start int, word string) (int, bool) {
	if start < 0 || len(text)-start < len(word) {
		return 0, false
	}
	for i := range len(word) {
		if asciiLower(text[start+i]) != word[i] {
			return 0, false
		}
	}
	return start + len(word), true
}

type gapWord struct {
	start int
	end   int
}

func allowedKeywordToCodeGap(gap string) bool {
	words, count, ok := parseGap(gap)
	if !ok {
		return false
	}
	switch count {
	case 0:
		return true
	case 1:
		return gapWordEquals(gap, words[0], "is") ||
			gapWordEquals(gap, words[0], "equals") ||
			gapWordEquals(gap, words[0], "below")
	case 2:
		return gapWordEquals(gap, words[0], "is") && gapWordEquals(gap, words[1], "below")
	default:
		return false
	}
}

func allowedCodeToKeywordGap(gap string) bool {
	words, count, ok := parseGap(gap)
	if !ok {
		return false
	}
	switch count {
	case 0:
		return true
	case 1:
		return gapWordEquals(gap, words[0], "is") ||
			gapWordEquals(gap, words[0], "as") ||
			gapWordEquals(gap, words[0], "for") ||
			gapWordEquals(gap, words[0], "your")
	case 2:
		return (gapWordEquals(gap, words[0], "is") ||
			gapWordEquals(gap, words[0], "as") ||
			gapWordEquals(gap, words[0], "for")) &&
			(gapWordEquals(gap, words[1], "your") || gapWordEquals(gap, words[1], "the"))
	default:
		return false
	}
}

func parseGap(gap string) ([3]gapWord, int, bool) {
	var words [3]gapWord
	count := 0
	for i := 0; i < len(gap); {
		switch {
		case isASCIIAlpha(gap[i]):
			if count == len(words) {
				return words, count, false
			}
			start := i
			for i < len(gap) && isASCIIAlpha(gap[i]) {
				i++
			}
			words[count] = gapWord{start: start, end: i}
			count++
		case isASCIIDigit(gap[i]) || gap[i] == '_':
			return words, count, false
		case gap[i] == '.' || gap[i] == '!' || gap[i] == '?' || gap[i] == ';':
			return words, count, false
		case gap[i] < utf8.RuneSelf:
			if isASCIIWhitespace(gap[i]) {
				i++
				continue
			}
			i++
		default:
			r, size := utf8.DecodeRuneInString(gap[i:])
			if r == utf8.RuneError && size == 1 {
				return words, count, false
			}
			if unicode.IsLetter(r) || unicode.IsNumber(r) || (!unicode.IsSpace(r) && !unicode.IsPunct(r)) || isHardClausePunctuation(r) {
				return words, count, false
			}
			i += size
		}
	}
	return words, count, true
}

func gapWordEquals(gap string, word gapWord, expected string) bool {
	if word.end-word.start != len(expected) {
		return false
	}
	for i := range len(expected) {
		if asciiLower(gap[word.start+i]) != expected[i] {
			return false
		}
	}
	return true
}

func standaloneNumber(text string, start, end int) bool {
	return !isWordBefore(text, start) && !isWordAt(text, end)
}

func partOfStructuredNumber(text string, start, end int) bool {
	if start >= 2 && isNumericSeparator(text[start-1]) && isASCIIDigit(text[start-2]) {
		return true
	}
	return end+1 < len(text) && isNumericSeparator(text[end]) && isASCIIDigit(text[end+1])
}

// embeddedInURLLikeToken is a bounded lexical exclusion, not URL parsing. It
// prevents query/path-shaped text such as "?code=1234" or "/otp/1234" from
// turning a link into a candidate.
func embeddedInURLLikeToken(text string, start, end int) bool {
	left := start - maximumURLTokenInspectionBytes
	if left < 0 {
		left = 0
	}
	for i := start - 1; i >= left; i-- {
		if isASCIIWhitespace(text[i]) {
			left = i + 1
			break
		}
	}

	right := end + maximumURLTokenInspectionBytes
	if right > len(text) {
		right = len(text)
	}
	for i := end; i < right; i++ {
		if isASCIIWhitespace(text[i]) {
			right = i
			break
		}
	}

	for i := left; i < right; i++ {
		switch text[i] {
		case '/', '?', '&':
			return true
		case '#':
			// A spaced label such as "code # 1234" is split into separate
			// tokens above; an in-token fragment marker is URL-like.
			return true
		}
	}
	return false
}

func isNumericSeparator(value byte) bool {
	return value == '-' || value == '/' || value == '.' || value == ':'
}

func isWordBefore(text string, position int) bool {
	if position <= 0 || position > len(text) {
		return false
	}
	r, size := utf8.DecodeLastRuneInString(text[:position])
	return isWordRune(r, size)
}

func isWordAt(text string, position int) bool {
	if position < 0 || position >= len(text) {
		return false
	}
	r, size := utf8.DecodeRuneInString(text[position:])
	return isWordRune(r, size)
}

func isWordRune(r rune, size int) bool {
	return (r == utf8.RuneError && size == 1) || r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func isASCIIAlpha(value byte) bool {
	value = asciiLower(value)
	return value >= 'a' && value <= 'z'
}

func isASCIIDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isASCIIWhitespace(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func whitespaceAt(text string, position int) (int, bool) {
	if position < 0 || position >= len(text) {
		return 0, false
	}
	if text[position] < utf8.RuneSelf {
		if isASCIIWhitespace(text[position]) {
			return 1, true
		}
		return 0, false
	}
	r, size := utf8.DecodeRuneInString(text[position:])
	return size, size > 1 && unicode.IsSpace(r)
}

func dashAt(text string, position int) (int, bool) {
	if position < 0 || position >= len(text) {
		return 0, false
	}
	if text[position] == '-' {
		return 1, true
	}
	r, size := utf8.DecodeRuneInString(text[position:])
	switch r {
	case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
		return size, true
	default:
		return 0, false
	}
}

func isHardClausePunctuation(r rune) bool {
	switch r {
	case '\u3002', '\uff01', '\uff1f', '\uff1b':
		return true
	default:
		return false
	}
}

func asciiLower(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}
