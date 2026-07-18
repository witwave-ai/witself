package avatar

import (
	"errors"
	"strings"
	"testing"
)

func TestDomainVocabularies(t *testing.T) {
	forms := []SubjectForm{
		SubjectHuman, SubjectAnimal, SubjectInsect, SubjectAnthropomorphic,
		SubjectHybrid, SubjectRobot, SubjectSymbolic,
	}
	for _, form := range forms {
		if !form.Valid() || form.Validate() != nil {
			t.Errorf("subject form %q is not valid", form)
		}
	}
	if err := SubjectForm("ghost").Validate(); !errors.Is(err, ErrInvalidSubjectForm) {
		t.Fatalf("invalid subject form error = %v", err)
	}

	policies := []AutonomyPolicy{AutonomyOperatorOnly, AutonomyAgentProposes, AutonomyAgentSelfManaged}
	for _, policy := range policies {
		if !policy.Valid() || policy.Validate() != nil {
			t.Errorf("autonomy policy %q is not valid", policy)
		}
	}
	if got := DefaultAutonomyPolicy(); got != AutonomyAgentSelfManaged {
		t.Errorf("DefaultAutonomyPolicy() = %q", got)
	}
	if err := AutonomyPolicy("anything_goes").Validate(); !errors.Is(err, ErrInvalidAutonomyPolicy) {
		t.Fatalf("invalid autonomy error = %v", err)
	}

	states := []Status{
		StatusPlaceholder, StatusGenerationDue, StatusProposed, StatusActive,
		StatusEvolutionDue, StatusRejected, StatusGenerationFailed, StatusArchived,
	}
	for _, status := range states {
		if !status.Valid() || status.Validate() != nil {
			t.Errorf("status %q is not valid", status)
		}
	}
	if err := Status("missing").Validate(); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("invalid status error = %v", err)
	}

	events := map[EventType]string{
		EventGenerationRequested: "avatar.generation.requested",
		EventProposed:            "avatar.proposed",
		EventActivated:           "avatar.activated",
		EventEvolved:             "avatar.evolved",
		EventRejected:            "avatar.rejected",
		EventGenerationFailed:    "avatar.generation.failed",
		EventRolledBack:          "avatar.rolled_back",
		EventReset:               "avatar.reset",
		EventPolicyChanged:       "avatar.policy.changed",
		EventStyleChanged:        "avatar.style.changed",
	}
	for event, wireValue := range events {
		if !event.Valid() || event.Validate() != nil || string(event) != wireValue {
			t.Errorf("event %q is invalid or differs from %q", event, wireValue)
		}
	}
	if err := EventType("avatar.did_a_thing").Validate(); !errors.Is(err, ErrInvalidEventType) {
		t.Fatalf("invalid event error = %v", err)
	}
}

func TestVocabularySlicesAreCopies(t *testing.T) {
	forms := SubjectForms()
	forms[0] = SubjectSymbolic
	if SubjectForms()[0] != SubjectHuman {
		t.Fatal("SubjectForms returned shared mutable storage")
	}
	policies := AutonomyPolicies()
	policies[0] = AutonomyAgentSelfManaged
	if AutonomyPolicies()[0] != AutonomyOperatorOnly {
		t.Fatal("AutonomyPolicies returned shared mutable storage")
	}
	statusesCopy := Statuses()
	statusesCopy[0] = StatusArchived
	if Statuses()[0] != StatusPlaceholder {
		t.Fatal("Statuses returned shared mutable storage")
	}
	events := EventTypes()
	events[0] = EventStyleChanged
	if EventTypes()[0] != EventGenerationRequested {
		t.Fatal("EventTypes returned shared mutable storage")
	}
}

func TestNormalizeDescription(t *testing.T) {
	got, err := NormalizeDescription("  A calm, curious fox.\n  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "A calm, curious fox." {
		t.Fatalf("NormalizeDescription() = %q", got)
	}
	for _, invalid := range []string{
		"",
		"   ",
		"bad\x00value",
		strings.Repeat("x", MaxDescriptionBytes+1),
		string([]byte{0xff}),
	} {
		if err := ValidateDescription(invalid); !errors.Is(err, ErrInvalidDescription) {
			t.Errorf("ValidateDescription(%q) error = %v", invalid, err)
		}
	}
}

func TestNormalizeSpecJSON(t *testing.T) {
	raw := []byte(` {
  "weight": 8,
  "locked": true,
  "layers": ["base", "expression"]
}`)
	got, err := NormalizeSpecJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"layers":["base","expression"],"locked":true,"weight":8}`
	if string(got) != want {
		t.Fatalf("NormalizeSpecJSON() = %s, want %s", got, want)
	}
	if err := ValidateSpecJSON(got); err != nil {
		t.Fatalf("canonical spec did not validate: %v", err)
	}
}

func TestNormalizeSpecJSONRejectsInvalidOrUnboundedInput(t *testing.T) {
	deep := `{"root":` + strings.Repeat(`[`, MaxSpecJSONDepth) + `0` + strings.Repeat(`]`, MaxSpecJSONDepth) + `}`
	manyNodes := `{"nodes":[` + strings.Repeat(`0,`, MaxSpecJSONNodes) + `0]}`
	tests := [][]byte{
		nil,
		[]byte(`[]`),
		[]byte(`null`),
		[]byte(`{"a":1} trailing`),
		[]byte(`{"a":1,"a":2}`),
		[]byte(`{"a":1,"\u0061":2}`),
		[]byte(`{"weight":1e20000}`),
		[]byte(`{"a":`),
		[]byte(deep),
		[]byte(manyNodes),
		bytesOf('x', MaxSpecJSONBytes+1),
		{0xff},
	}
	for _, raw := range tests {
		if _, err := NormalizeSpecJSON(raw); !errors.Is(err, ErrInvalidSpecJSON) {
			t.Errorf("NormalizeSpecJSON(%q) error = %v, want ErrInvalidSpecJSON", raw, err)
		}
	}
}

func TestNormalizeSpecJSONLeavesRoomForPostgresJSONBSeparators(t *testing.T) {
	// PostgreSQL's jsonb text form can add at most one space per comma and
	// colon. With the independently enforced node limit, two bytes per node is
	// a conservative upper bound for that expansion.
	const postgresVisualSpecLimit = 16 * 1024
	if MaxSpecJSONBytes+2*MaxSpecJSONNodes > postgresVisualSpecLimit {
		t.Fatalf("spec limit %d plus separator headroom exceeds database limit %d",
			MaxSpecJSONBytes, postgresVisualSpecLimit)
	}

	padding := strings.Repeat("x", MaxSpecJSONBytes-32)
	canonical, err := NormalizeSpecJSON([]byte(`{"padding":"` + padding + `"}`))
	if err != nil {
		t.Fatalf("near-limit compact spec was rejected: %v", err)
	}
	if len(canonical)+2*MaxSpecJSONNodes > postgresVisualSpecLimit {
		t.Fatalf("accepted canonical spec lacks jsonb separator headroom: %d bytes", len(canonical))
	}
}

func bytesOf(value byte, count int) []byte {
	return []byte(strings.Repeat(string(value), count))
}
