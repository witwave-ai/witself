package syncmodel

import (
	"errors"
	"fmt"
	"testing"
)

func captureEffects() []CapturedEffect {
	return []CapturedEffect{
		{Reference: "effect-first", Operation: OperationID{1}, AtomicGroup: "group", ObjectID: "object", Value: StringValue("first"), Dependencies: []Dependency{}},
		{Reference: "effect-second", Operation: OperationID{1}, AtomicGroup: "group", ObjectID: "object", Value: StringValue("second"), Dependencies: []Dependency{{Kind: "generation", AccountID: "account-fixture", RealmID: "realm", OwnerID: "owner", ObjectID: "dependency", Generation: GenerationID{1}}}},
	}
}

func TestCaptureOmittedSemanticJournal(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]CapturedEffect) []CapturedEffect
	}{
		{"omitted", func([]CapturedEffect) []CapturedEffect { return nil }},
		{"empty", func([]CapturedEffect) []CapturedEffect { return []CapturedEffect{} }},
		{"partial", func(e []CapturedEffect) []CapturedEffect { return e[:1] }},
		{"same_count_wrong_effect", func(e []CapturedEffect) []CapturedEffect { e[1].Value = StringValue("substituted"); return e }},
		{"same_count_missing_dependency", func(e []CapturedEffect) []CapturedEffect { e[1].Dependencies = []Dependency{}; return e }},
		{"reordered", func(e []CapturedEffect) []CapturedEffect { return []CapturedEffect{e[1], e[0]} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			j, err := AdmitCapture(fixtureEnvelope().OriginalIncarnation, 1, "bundle", true, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range captureEffects() {
				if err := j.RecordEffect(e); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := j.Seal(1, test.mutate(captureEffects())); err == nil {
				t.Fatal("incomplete semantic journal sealed")
			}
			b, err := j.Seal(1, captureEffects())
			if err != nil || !b.Sealed || b.OriginClass != "original" || len(b.Effects) != 2 || len(b.LogicalOperations) != 1 || len(b.AtomicGroups) != 1 {
				t.Fatal("complete repeated-object effects did not seal")
			}
			if err := j.RecordEffect(captureEffects()[0]); err == nil {
				t.Fatal("mutation after seal accepted")
			}
			if _, err := j.Seal(2, captureEffects()); err == nil {
				t.Fatal("second bundle sealed")
			}
		})
	}
}

func TestCaptureRequiresExplicitCapabilityAndActiveControl(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {true, false}, {false, true}} {
		if _, err := AdmitCapture(fixtureEnvelope().OriginalIncarnation, 1, "bundle", flags[0], flags[1]); err == nil {
			t.Fatal("unadmitted writer accepted")
		}
	}
	var unadmitted CaptureJournal
	if err := unadmitted.RecordEffect(captureEffects()[0]); err == nil {
		t.Fatal("zero-value writer accepted")
	}
	j, err := AdmitCapture(fixtureEnvelope().OriginalIncarnation, 1, "bundle", true, true)
	if err != nil {
		t.Fatal(err)
	}
	e := captureEffects()[1]
	e.Dependencies[0].AccountID = "other-account"
	if err := j.RecordEffect(e); err == nil {
		t.Fatal("foreign dependency accepted")
	}
	if _, err := j.Seal(1, nil); err == nil {
		t.Fatal("empty mutation sealed")
	}
}

func resourceOperation(i int) OperationID { return OperationID{byte(i), byte(i >> 8)} }

func TestBoundedResourceAdmission(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		usage func(int) ResourceUsage
	}{
		{"envelope", MaxEnvelopeBytes, func(n int) ResourceUsage { return ResourceUsage{EnvelopeBytes: n, TransportBytes: n} }},
		{"dependencies", MaxDependencies, func(n int) ResourceUsage { return ResourceUsage{DependencyCount: n} }},
		{"fragments", MaxFragments, func(n int) ResourceUsage { return ResourceUsage{FragmentCount: n} }},
		{"compound_payload", MaxCompoundBytes, func(n int) ResourceUsage { return ResourceUsage{CompoundBytes: n, TransportBytes: n} }},
	} {
		for _, delta := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/%d", test.name, delta), func(t *testing.T) {
				var q ResourceQueue
				base := ResourceUsage{EnvelopeBytes: 1, TransportBytes: 1}
				if err := q.Admit("account-a", resourceOperation(0), base); err != nil {
					t.Fatal(err)
				}
				err := q.Admit("account-a", resourceOperation(1), test.usage(test.limit+delta))
				if (delta <= 0 && err != nil) || (delta > 0 && !errors.Is(err, ErrResourceCapacity)) {
					t.Fatal("unexpected admission boundary")
				}
				count, size := q.Usage("account-a")
				if delta > 0 && (count != 1 || size != 1) {
					t.Fatal("refusal changed queued work")
				}
				if err := q.Admit("account-b", resourceOperation(1), test.usage(test.limit)); err != nil {
					t.Fatal("account budget contaminated")
				}
			})
		}
	}
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("pending/%d", delta), func(t *testing.T) {
			var q ResourceQueue
			for i := 0; i < MaxPendingOperations+delta; i++ {
				err := q.Admit("account-a", resourceOperation(i), ResourceUsage{TransportBytes: 1})
				if i < MaxPendingOperations && err != nil || i == MaxPendingOperations && !errors.Is(err, ErrResourceCapacity) {
					t.Fatal("unexpected pending operation boundary")
				}
			}
			want := min(MaxPendingOperations+delta, MaxPendingOperations)
			if n, size := q.Usage("account-a"); n != want || size != want {
				t.Fatal("pending refusal lost work")
			}
			if err := q.Admit("account-b", resourceOperation(0), ResourceUsage{TransportBytes: 1}); err != nil {
				t.Fatal("pending budget crossed accounts")
			}
		})
		t.Run(fmt.Sprintf("transport/%d", delta), func(t *testing.T) {
			var q ResourceQueue
			for i := 0; i < 7; i++ {
				if err := q.Admit("account-a", resourceOperation(i), ResourceUsage{TransportBytes: MaxCompoundBytes, CompoundBytes: MaxCompoundBytes}); err != nil {
					t.Fatal(err)
				}
			}
			err := q.Admit("account-a", resourceOperation(7), ResourceUsage{TransportBytes: MaxCompoundBytes + delta})
			if delta <= 0 && err != nil || delta > 0 && !errors.Is(err, ErrResourceCapacity) {
				t.Fatal("unexpected transport byte boundary")
			}
			want := MaxTransportBytes + delta
			if delta > 0 {
				want = 7 * MaxCompoundBytes
			}
			if _, size := q.Usage("account-a"); size != want {
				t.Fatal("transport refusal changed queued work")
			}
			if err := q.Admit("account-b", resourceOperation(7), ResourceUsage{TransportBytes: MaxCompoundBytes}); err != nil {
				t.Fatal("transport budget crossed accounts")
			}
		})
	}
}

func TestBoundedResourceRetryAndNodeIsolation(t *testing.T) {
	var q, otherNode ResourceQueue
	u := ResourceUsage{EnvelopeBytes: 10, TransportBytes: 10}
	for range 2 {
		if err := q.Admit("account", OperationID{1}, u); err != nil {
			t.Fatal(err)
		}
	}
	if n, b := q.Usage("account"); n != 1 || b != 10 {
		t.Fatal("retry reserved twice")
	}
	u.TransportBytes++
	if err := q.Admit("account", OperationID{1}, u); err == nil {
		t.Fatal("changed retry accepted")
	}
	if err := otherNode.Admit("account", OperationID{1}, u); err != nil {
		t.Fatal("independent node inherited reservation")
	}
}
