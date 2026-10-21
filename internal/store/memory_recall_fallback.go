package store

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var memoryRecallAnyTermsTokenPattern = regexp.MustCompile(`[\pL\pN]+`)

var memoryRecallAnyTermsStopWords = func() map[string]bool {
	words := strings.Fields("a about after again all an and any are as at be been before being but by can could did do does for from had has have how i if in into is it its me my no not of on or our so than that the their them then there these they this those to up us was we were what when where which who why will with would you your")
	result := make(map[string]bool, len(words))
	for _, word := range words {
		result[word] = true
	}
	return result
}()

// memoryRecallAnyTermsQuery preserves explicit web-search operators and only
// broadens plain multi-field queries after their strict first page is empty.
func memoryRecallAnyTermsQuery(query string) (string, bool) {
	if strings.Contains(query, `"`) {
		return "", false
	}
	fields := strings.Fields(query)
	if len(fields) < 2 {
		return "", false
	}
	for _, field := range fields {
		if strings.EqualFold(field, "or") {
			return "", false
		}
		for i, char := range field {
			if char == '-' && (i == 0 || strings.ContainsRune("()!&|<", rune(field[i-1]))) {
				return "", false
			}
		}
	}
	seen := make(map[string]bool)
	terms := make([]string, 0, 12)
	for _, field := range fields {
		if !memoryRecallAnyTermsTokenPattern.MatchString(field) {
			continue
		}
		key := strings.TrimFunc(strings.ToLower(field), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if utf8.RuneCountInString(key) < 2 || len(key) > 128 || memoryRecallAnyTermsStopWords[key] || seen[key] {
			continue
		}
		seen[key] = true
		terms = append(terms, field)
		if len(terms) == 12 {
			break
		}
	}
	if len(terms) == 0 {
		return "", false
	}
	return strings.Join(terms, " OR "), true
}
