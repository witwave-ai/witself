package store

import (
	"strings"
	"testing"
)

func TestMemoryRecallAnyTermsQuery(t *testing.T) {
	for _, tt := range []struct {
		name     string
		query    string
		want     string
		eligible bool
	}{
		{name: "plain", query: "alpha delta", want: "alpha OR delta", eligible: true},
		{name: "and", query: "alpha AND beta", want: "alpha OR beta", eligible: true},
		{name: "quoted", query: `"beta alpha"`},
		{name: "minus", query: "alpha -gamma"},
		{name: "parenthesized_minus", query: "alpha (-gamma)"},
		{name: "not_minus", query: "deploy !-staging"},
		{name: "and_minus", query: "alpha &-gamma"},
		{name: "or_minus", query: "alpha |-gamma"},
		{name: "less_than_minus", query: "alpha <-gamma"},
		{name: "closing_parenthesis_minus", query: "alpha )-gamma"},
		{name: "lowercase_or", query: "beta or gamma"},
		{name: "mixedcase_or", query: "beta oR gamma"},
		{name: "punctuation_or", query: "drill, OR & (plan)"},
		{name: "single_word", query: "migration"},
		{name: "single_compound", query: "slice-85"},
		{name: "only_common_words", query: "the a"},
		{name: "punctuated_common_words", query: "(the) a,"},
		{name: "punctuation_only", query: "& ( ) !"},
		{name: "first_twelve", query: "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi", want: "alpha OR beta OR gamma OR delta OR epsilon OR zeta OR eta OR theta OR iota OR kappa OR lambda OR mu", eligible: true},
		{name: "unicode", query: "café straße", want: "café OR straße", eligible: true},
		{name: "natural_language", query: "what did we decide about the migration", want: "decide OR migration", eligible: true},
		{name: "deduplicate_in_order", query: "BETA alpha beta ALPHA", want: "BETA OR alpha", eligible: true},
		{name: "deduplicate_trimmed_key", query: "Drill, (drill) PLAN plan", want: "Drill, OR PLAN", eligible: true},
		{name: "original_fields_count", query: "the migration", want: "migration", eligible: true},
		{name: "short_runes", query: "a é 1 xy", want: "xy", eligible: true},
		{name: "byte_limit", query: strings.Repeat("é", 65) + " " + strings.Repeat("é", 64), want: strings.Repeat("é", 64), eligible: true},
		{name: "one_punctuated_field", query: "alpha,beta"},
		{name: "punctuation", query: "drill, & (plan)", want: "drill, OR (plan)", eligible: true},
		{name: "version", query: "v0.0.337 drill", want: "v0.0.337 OR drill", eligible: true},
		{name: "hyphenated_identifier", query: "slice-85 drill", want: "slice-85 OR drill", eligible: true},
		{name: "hyphenated_word", query: "e-mail drill", want: "e-mail OR drill", eligible: true},
		{name: "source_path", query: "internal/store/memory.go recall", want: "internal/store/memory.go OR recall", eligible: true},
		{name: "email", query: "foo@bar.com mail", want: "foo@bar.com OR mail", eligible: true},
		{name: "domain", query: "witself.ai recall", want: "witself.ai OR recall", eligible: true},
		{name: "empty", query: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, eligible := memoryRecallAnyTermsQuery(tt.query)
			if got != tt.want || eligible != tt.eligible {
				t.Fatalf("fallback = %q / %v, want %q / %v", got, eligible, tt.want, tt.eligible)
			}
		})
	}
}
