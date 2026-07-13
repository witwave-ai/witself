package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCandidateInputUsesConservativeDefaults(t *testing.T) {
	confidence := 0.5
	in, err := normalizeSetFactInput(SetFactInput{
		Subject: "user", Predicate: "preferences/editor",
		Value: json.RawMessage(`"vim"`), SourceKind: FactSourceInference,
		Confidence: &confidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Subject != "self" || in.ValueType != "string" || *in.Confidence != 0.5 {
		t.Fatalf("candidate input = %#v", in)
	}
}

func TestNormalizeFactCandidateListOptions(t *testing.T) {
	opts, err := normalizeFactCandidateListOptions(FactCandidateListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Status != "open" || opts.Limit != 100 {
		t.Fatalf("default options = %#v", opts)
	}

	opts, err = normalizeFactCandidateListOptions(FactCandidateListOptions{Status: "confirmed", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Status != "confirmed" || opts.Limit != 500 {
		t.Fatalf("explicit options = %#v", opts)
	}

	for _, opts := range []FactCandidateListOptions{
		{Status: "unknown"},
		{Limit: -1},
		{Limit: 501},
	} {
		if _, err := normalizeFactCandidateListOptions(opts); !errors.Is(err, ErrFactInputInvalid) {
			t.Errorf("normalize %#v error = %v", opts, err)
		}
	}
}

func TestGetFactCandidateRequiresAgent(t *testing.T) {
	st := &Store{}
	_, err := st.GetFactCandidate(context.Background(), Principal{Kind: PrincipalOperator}, "fcand_1")
	if !errors.Is(err, ErrFactForbidden) {
		t.Fatalf("error = %v", err)
	}
}
