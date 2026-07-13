package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestProjectFactOccurrenceDateUsesCalendarSemantics(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	window := upcomingFactWindow{
		from:     time.Date(2026, 7, 12, 23, 30, 0, 0, time.UTC), // July 12 in Denver.
		until:    time.Date(2026, 7, 13, 7, 30, 0, 0, time.UTC),  // July 13 at 01:30.
		location: denver,
	}

	today, ok := projectFactOccurrence(temporalTestFact("date", `"2026-07-12"`), window)
	if !ok || today.occurrence.OccursOn != "2026-07-12" || today.occurrence.OccursAt != nil {
		t.Fatalf("today projection = %#v, %v", today, ok)
	}
	tomorrow, ok := projectFactOccurrence(temporalTestFact("date", `"2026-07-13"`), window)
	if !ok || tomorrow.occurrence.OccursOn != "2026-07-13" {
		t.Fatalf("tomorrow projection = %#v, %v", tomorrow, ok)
	}
	if _, ok := projectFactOccurrence(temporalTestFact("date", `"2026-07-14"`), window); ok {
		t.Fatal("date after the window was projected")
	}
}

func TestProjectFactOccurrenceDateHonorsExclusiveMidnightUntil(t *testing.T) {
	window := upcomingFactWindow{
		from:     time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
		until:    time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC),
		location: time.UTC,
	}
	if _, ok := projectFactOccurrence(temporalTestFact("date", `"2026-07-13"`), window); ok {
		t.Fatal("date at the exclusive midnight boundary was projected")
	}
}

func TestProjectFactOccurrenceDateTimeUsesInstantSemantics(t *testing.T) {
	window := upcomingFactWindow{
		from:     time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC),
		until:    time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC),
		location: time.UTC,
	}
	projected, ok := projectFactOccurrence(temporalTestFact("datetime", `"2026-07-12T13:00:00-06:00"`), window)
	if !ok || projected.occurrence.OccursAt == nil {
		t.Fatalf("datetime projection = %#v, %v", projected, ok)
	}
	if want := time.Date(2026, 7, 12, 19, 0, 0, 0, time.UTC); !projected.occurrence.OccursAt.Equal(want) {
		t.Fatalf("occurs_at = %s, want %s", projected.occurrence.OccursAt, want)
	}
	if _, ok := projectFactOccurrence(temporalTestFact("datetime", `"2026-07-12T20:00:00Z"`), window); ok {
		t.Fatal("datetime at exclusive until was projected")
	}
}

func TestProjectAnnualDateOccurrencesAcrossYears(t *testing.T) {
	window := upcomingFactWindow{
		from:     time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC),
		until:    time.Date(2028, 7, 14, 0, 0, 0, 0, time.UTC),
		location: time.UTC,
	}
	fact := temporalTestFact("date", `"1990-07-13"`)
	fact.Recurrence = FactRecurrenceAnnual
	got := projectFactOccurrences(fact, window, 10)
	want := []string{"2026-07-13", "2027-07-13", "2028-07-13"}
	if len(got) != len(want) {
		t.Fatalf("occurrences = %#v, want %v", got, want)
	}
	for i := range want {
		if got[i].occurrence.OccursOn != want[i] {
			t.Errorf("occurrence %d = %q, want %q", i, got[i].occurrence.OccursOn, want[i])
		}
	}
}

func TestProjectAnnualFebruary29SkipsNonLeapYears(t *testing.T) {
	window := upcomingFactWindow{
		from:     time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		until:    time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
		location: time.UTC,
	}
	fact := temporalTestFact("date", `"2000-02-29"`)
	fact.Recurrence = FactRecurrenceAnnual
	got := projectFactOccurrences(fact, window, 10)
	if len(got) != 1 || got[0].occurrence.OccursOn != "2028-02-29" {
		t.Fatalf("occurrences = %#v, want only 2028-02-29", got)
	}
}

func TestProjectAnnualDateDoesNotPrecedeBaseDate(t *testing.T) {
	window := upcomingFactWindow{
		from:     time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		until:    time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		location: time.UTC,
	}
	fact := temporalTestFact("date", `"2027-07-13"`)
	fact.Recurrence = FactRecurrenceAnnual
	got := projectFactOccurrences(fact, window, 10)
	if len(got) != 1 || got[0].occurrence.OccursOn != "2027-07-13" {
		t.Fatalf("occurrences = %#v, want only base date", got)
	}
}

func TestProjectFactOccurrenceHonorsResolvedAssertionValidity(t *testing.T) {
	from := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	window := upcomingFactWindow{from: from, until: from.Add(48 * time.Hour), location: time.UTC}
	fact := temporalTestFact("datetime", `"2026-07-13T18:00:00Z"`)

	future := from.Add(time.Second)
	fact.ValidFrom = &future
	if _, ok := projectFactOccurrence(fact, window); ok {
		t.Fatal("not-yet-valid resolved assertion was projected")
	}

	fact.ValidFrom = nil
	expired := from.Add(-time.Nanosecond)
	fact.ValidUntil = &expired
	if _, ok := projectFactOccurrence(fact, window); ok {
		t.Fatal("expired resolved assertion was projected")
	}

	fact.ValidUntil = &from
	if _, ok := projectFactOccurrence(fact, window); !ok {
		t.Fatal("assertion valid exactly at from was not projected")
	}
}

func TestProjectFactOccurrenceSkipsMalformedTemporalValues(t *testing.T) {
	window := upcomingFactWindow{
		from:     time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		until:    time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		location: time.UTC,
	}
	for _, fact := range []Fact{
		temporalTestFact("date", `"07/13/2026"`),
		temporalTestFact("datetime", `"tomorrow"`),
		temporalTestFact("datetime", `42`),
		temporalTestFact("string", `"2026-07-13"`),
	} {
		if _, ok := projectFactOccurrence(fact, window); ok {
			t.Fatalf("malformed fact was projected: %#v", fact)
		}
	}
}

func TestNormalizeUpcomingFactOptions(t *testing.T) {
	from := time.Date(2026, 7, 12, 18, 0, 0, 0, time.FixedZone("MDT", -6*60*60))
	window, opts, err := normalizeUpcomingFactOptions(UpcomingFactOptions{
		From: from, Subject: " MYSELF ", PredicatePrefix: "schedule/", Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Subject != "self" || opts.PredicatePrefix != "schedule/" || opts.Location != time.UTC {
		t.Fatalf("options = %#v", opts)
	}
	if !window.from.Equal(from.UTC()) || !window.until.Equal(from.UTC().Add(defaultUpcomingFactWindow)) {
		t.Fatalf("window = %#v", window)
	}

	for _, invalid := range []UpcomingFactOptions{
		{From: from, Until: from},
		{From: from, Until: from.Add(time.Hour), Limit: 501},
		{From: from, Until: from.Add(time.Hour), Subject: "bad\x00subject"},
		{From: from, Until: from.Add(time.Hour), PredicatePrefix: "Bad Prefix"},
	} {
		if _, _, err := normalizeUpcomingFactOptions(invalid); !errors.Is(err, ErrFactInputInvalid) {
			t.Fatalf("error = %v, want ErrFactInputInvalid for %#v", err, invalid)
		}
	}
}

func temporalTestFact(valueType, value string) Fact {
	return Fact{
		ID:        "fact_test",
		Subject:   "self",
		Predicate: "schedule/test",
		ValueType: valueType,
		Value:     json.RawMessage(value),
	}
}
