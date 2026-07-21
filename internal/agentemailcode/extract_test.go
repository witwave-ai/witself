package agentemailcode

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestExtractNoCandidates(t *testing.T) {
	tests := []string{
		"",
		"There are no numbers here.",
		"The meeting is on 2026-07-21; call 303-555-1212; card ending 4242.",
		"For help with your code, call 5551212 or use card 4242 4242 4242 4242.",
		"A barcode has 123456 digits; pinpoint 9876; optimal 87654321.",
	}
	for _, text := range tests {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("Extract(%q) = %#v, want no candidates", text, got)
		}
	}
}

func TestExtractSingleCandidate(t *testing.T) {
	want := []Candidate{{Value: "123456", Occurrences: 1}}
	if got := Extract("Your verification code is 123456."); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractCaseSpacingPunctuationAndSupportedKeywords(t *testing.T) {
	text := strings.Join([]string{
		"VeRiFiCaTiOn   CoDe:\t[1234]",
		"SECURITY-CODE -- 23456",
		"one\n time\n code = 345678",
		"ONE - TIME - CODE: 4567890",
		"PassCode (56789012)",
		"otp # 6789",
		"PIN is below:\n78901",
		"CoDe equals 890123",
		"901234 is your verification code",
		"Use 912345 as the one-time code",
		"CODE\u00a0—\u00a09234",
		"9234 is your one‑time code",
	}, ".\n")
	want := []Candidate{
		{Value: "1234", Occurrences: 1},
		{Value: "23456", Occurrences: 1},
		{Value: "345678", Occurrences: 1},
		{Value: "4567890", Occurrences: 1},
		{Value: "56789012", Occurrences: 1},
		{Value: "6789", Occurrences: 1},
		{Value: "78901", Occurrences: 1},
		{Value: "890123", Occurrences: 1},
		{Value: "901234", Occurrences: 1},
		{Value: "912345", Occurrences: 1},
		{Value: "9234", Occurrences: 2},
	}
	if got := Extract(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractCountsAssociatedRepetitionsInFirstSeenOrder(t *testing.T) {
	text := "Code: 123456. OTP is 654321. Security code: 123456. Use 123456 as your one-time code. PIN: 654321."
	want := []Candidate{
		{Value: "123456", Occurrences: 3},
		{Value: "654321", Occurrences: 2},
	}
	if got := Extract(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractRejectsNonStandaloneAndOutOfRangeNumbers(t *testing.T) {
	text := "code 123 code 123456789 code a1234 code 1234b code _2345 code 2345_ code é3456 code 3456é code １２３４ code 87654321"
	want := []Candidate{{Value: "87654321", Occurrences: 1}}
	if got := Extract(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractRejectsStructuredAndUnrelatedNumbers(t *testing.T) {
	text := strings.Join([]string{
		"Your verification code expires on 2026-07-21.",
		"For help with your security code, call 303-555-1212.",
		"The one-time code message mentions card 4242 4242 4242 4242.",
		"OTP documentation lists address 192.168.1234.5.",
	}, "\n")
	if got := Extract(text); len(got) != 0 {
		t.Fatalf("Extract() = %#v, want no candidates", got)
	}
}

func TestExtractRequiresLocalAssociation(t *testing.T) {
	text := "Code delivery was requested" + strings.Repeat(" ", maximumAssociationGap+1) + "123456"
	if got := Extract(text); len(got) != 0 {
		t.Fatalf("Extract() = %#v, want no candidates", got)
	}
}

func TestExtractRejectsURLLikeTokens(t *testing.T) {
	tests := []string{
		"https://x.test/?code=1234",
		"https://x.test/path/otp/2345",
		"Visit /verify?security-code=3456",
		"https://x.test/#pin=4567",
	}
	for _, text := range tests {
		if got := Extract(text); len(got) != 0 {
			t.Errorf("Extract(%q) = %#v, want no candidates", text, got)
		}
	}

	want := []Candidate{{Value: "5678", Occurrences: 1}}
	if got := Extract("Code=5678"); !reflect.DeepEqual(got, want) {
		t.Fatalf("plain labeled value = %#v, want %#v", got, want)
	}
}

func TestExtractBoundedReportsDistinctOverflowAndKeepsCounts(t *testing.T) {
	var text strings.Builder
	for i := 0; i < MaximumCandidates+2; i++ {
		fmt.Fprintf(&text, "Code: %04d. ", 1000+i)
	}
	text.WriteString("Code: 1000. Code: 1031. Code: 1033.")

	result := ExtractBounded(text.String())
	if !result.Overflow || len(result.Candidates) != MaximumCandidates {
		t.Fatalf("bounded result = %#v, want %d candidates with overflow", result, MaximumCandidates)
	}
	for index, candidate := range result.Candidates {
		wantValue := fmt.Sprintf("%04d", 1000+index)
		wantOccurrences := 1
		if index == 0 || index == MaximumCandidates-1 {
			wantOccurrences = 2
		}
		if candidate.Value != wantValue || candidate.Occurrences != wantOccurrences {
			t.Errorf("candidate[%d] = %#v, want value %q occurrences %d", index, candidate, wantValue, wantOccurrences)
		}
	}
	if got := Extract(text.String()); !reflect.DeepEqual(got, result.Candidates) {
		t.Fatalf("Extract compatibility result differs: %#v vs %#v", got, result.Candidates)
	}
}

func TestExtractBoundedDoesNotReportOverflowAtExactCap(t *testing.T) {
	var text strings.Builder
	for i := 0; i < MaximumCandidates; i++ {
		fmt.Fprintf(&text, "OTP: %04d. ", 2000+i)
	}
	result := ExtractBounded(text.String())
	if result.Overflow || len(result.Candidates) != MaximumCandidates {
		t.Fatalf("exact-cap result = %#v", result)
	}
}

func TestExtractHandlesMaximumDecodedTextBound(t *testing.T) {
	const maximumDecodedText = 1024 * 1024
	suffix := "\nOTP: 76543210"
	text := strings.Repeat("x", maximumDecodedText-len(suffix)) + suffix
	want := []Candidate{{Value: "76543210", Occurrences: 1}}
	if got := Extract(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract(1 MiB) = %#v, want %#v", got, want)
	}
}
