package trust

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestHandoffTransitionTable(t *testing.T) {
	states := []string{"staged", "activating", "activated", "superseded", "retired", "faulted"}
	for _, from := range states {
		for _, to := range states {
			t.Run(from+"_to_"+to, func(t *testing.T) {
				j := HandoffJournal{Generations: map[string]HandoffRecord{"fixture": {State: from}}}
				allowed := from != to && (to == "faulted" || from == "staged" && to == "activating" || from == "activating" && to == "activated" || from == "activated" && to == "superseded" || from == "superseded" && to == "retired")
				err := handoffTransition(&j, "fixture", to)
				if (err == nil) != allowed {
					t.Fatal("incorrect transition classification")
				}
				if err != nil && j.Generations["fixture"].State != from {
					t.Fatal("refused transition mutated state")
				}
			})
		}
	}
}

func TestHandoffInterruptedActivationReconcilesOrFaults(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		name := "complete"
		if tamper {
			name = "fault"
		}
		t.Run(name, func(t *testing.T) {
			root, g, _ := payloadFixture(t)
			j, err := loadHandoff(root)
			if err != nil {
				t.Fatal("fixture journal")
			}
			if handoffTransition(&j, g.Name, "activating") != nil || saveHandoff(root, &j) != nil {
				t.Fatal("fixture intent")
			}
			if tamper && os.WriteFile(filepath.Join(root, "payload", g.Name, filepath.FromSlash(g.Host)), []byte("evil"), 0755) != nil {
				t.Fatal("fixture tamper")
			}
			j, err = InspectHandoff(t.Context(), root)
			if err != nil {
				t.Fatalf("reconciliation: %s", reasonOf(err))
			}
			want := "activated"
			if tamper {
				want = "faulted"
			}
			if j.Generations[g.Name].State != want {
				t.Fatalf("reconciled state=%s", j.Generations[g.Name].State)
			}
			if !tamper && j.Generations[g.Name].Receipt == nil {
				t.Fatal("reconciliation lacks activation receipt")
			}
			if _, err = InspectHandoff(t.Context(), root); err != nil {
				t.Fatal("reconciliation not stable")
			}
		})
	}
}

func handoffSupersededFixture(t *testing.T) (string, Generation, Generation) {
	t.Helper()
	f := payloadInput(t)
	first, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatal("fixture stage")
	}
	if _, _, err = Activate(t.Context(), f.root, first.Name, f.clock); err != nil {
		t.Fatal("fixture activate")
	}
	f = payloadNext(t, f, 2)
	next, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatal("fixture next stage")
	}
	if _, _, err = Activate(t.Context(), f.root, next.Name, f.clock); err != nil {
		t.Fatal("fixture next activate")
	}
	return f.root, first, next
}

func TestRetireActivatedDescriptorAndProcessReferences(t *testing.T) {
	root, first, next := handoffSupersededFixture(t)
	j, err := InspectHandoff(t.Context(), root)
	if err != nil || j.Generations[first.Name].State != "superseded" {
		t.Fatal("previous activation not superseded")
	}
	if _, err := LoadGeneration(root, first.Name); err != nil {
		t.Fatal("superseded generation removed automatically")
	}
	noProcess := func(context.Context, string) (bool, error) { return false, nil }
	if err := retireGeneration(t.Context(), root, next.Name, noProcess); err != ReasonGenerationReferenced {
		t.Fatal("active retirement not refused")
	}
	dir := filepath.Join(root, "service")
	if privateDirectory(dir, true) != nil {
		t.Fatal("fixture service directory")
	}
	d := map[string]any{"payload": ServicePayload{Generation: first.Name, Host: filepath.Join(root, "payload", first.Name, filepath.FromSlash(first.Host))}}
	if writePrivateJSON(dir, "descriptor.json", d) != nil {
		t.Fatal("fixture descriptor")
	}
	if err := retireGeneration(t.Context(), root, first.Name, noProcess); err != ReasonGenerationReferenced {
		t.Fatal("descriptor reference not refused")
	}
	if os.Remove(filepath.Join(dir, "descriptor.json")) != nil {
		t.Fatal("fixture descriptor removal")
	}
	if err := retireGeneration(t.Context(), root, first.Name, func(context.Context, string) (bool, error) { return true, nil }); err != ReasonGenerationReferenced {
		t.Fatal("process reference not refused")
	}
	if err := retireGeneration(t.Context(), root, first.Name, func(context.Context, string) (bool, error) { return false, ReasonProcessObservationUnavailable }); err != ReasonProcessObservationUnavailable {
		t.Fatal("unknown process visibility allowed retirement")
	}
	if err := retireGeneration(t.Context(), root, first.Name, noProcess); err != nil {
		t.Fatal("unreferenced superseded retirement failed")
	}
	j, err = loadHandoff(root)
	if err != nil || j.Generations[first.Name].State != "retired" {
		t.Fatal("retirement not recorded")
	}
	if _, err = os.Lstat(filepath.Join(root, "payload", first.Name)); !os.IsNotExist(err) {
		t.Fatal("retired generation retained")
	}
	if _, err = LoadGeneration(root, next.Name); err != nil {
		t.Fatal("retirement damaged active generation")
	}
}

func TestHandoffPrivateReadsAndStrictLocalJSON(t *testing.T) {
	root, g, _ := payloadFixture(t)
	dir := filepath.Join(root, "payload")
	name := filepath.Join(dir, "handoff.json")
	original, err := os.ReadFile(name)
	if err != nil {
		t.Fatal("fixture journal read")
	}
	for _, bad := range [][]byte{[]byte(`{"version":1,"version":1,"revision":1,"generations":{}}`), []byte(`{"version":1,"revision":1,"generations":{},"unexpected":true}`)} {
		if os.WriteFile(name, bad, 0600) != nil {
			t.Fatal("fixture corrupt write")
		}
		if _, err := InspectHandoff(t.Context(), root); err == nil {
			t.Fatal("invalid journal accepted")
		}
	}
	if os.WriteFile(name, original, 0600) != nil || os.Chmod(name, 0644) != nil {
		t.Fatal("fixture privacy change")
	}
	if _, err := InspectHandoff(t.Context(), root); err == nil {
		t.Fatal("public journal accepted")
	}
	if os.Chmod(name, 0600) != nil {
		t.Fatal("fixture privacy restore")
	}
	var j HandoffJournal
	if json.Unmarshal(original, &j) != nil || j.Generations[g.Name].State != "staged" {
		t.Fatal("fixture original invalid")
	}
	if runtime.GOOS != "linux" {
		if err := Retire(t.Context(), root, g.Name); err != ReasonHandoffTransition {
			t.Fatal("staged retirement classification")
		}
	}
}

func TestRetireResumesCommittedCleanup(t *testing.T) {
	root, first, next := handoffSupersededFixture(t)
	j, err := loadHandoff(root)
	if err != nil {
		t.Fatal("fixture handoff")
	}
	if handoffTransition(&j, first.Name, "retired") != nil || saveHandoff(root, &j) != nil {
		t.Fatal("fixture retirement intent")
	}
	if os.Chmod(filepath.Join(root, "payload", first.Name), 0700) != nil {
		t.Fatal("fixture interrupted cleanup")
	}
	noProcess := func(context.Context, string) (bool, error) { return false, nil }
	if err := retireGeneration(t.Context(), root, first.Name, noProcess); err != nil {
		t.Fatal("committed cleanup did not resume")
	}
	if err := retireGeneration(t.Context(), root, first.Name, noProcess); err != nil {
		t.Fatal("completed cleanup not idempotent")
	}
	if _, err := LoadGeneration(root, next.Name); err != nil {
		t.Fatal("resumed cleanup damaged current generation")
	}
}

func TestHandoffRefusesAmbiguousIntents(t *testing.T) {
	f := payloadInput(t)
	first, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatal("first fixture stage")
	}
	f = payloadNext(t, f, 2)
	second, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatal("second fixture stage")
	}
	j, err := loadHandoff(f.root)
	if err != nil {
		t.Fatal("fixture journal")
	}
	j.Generations[first.Name] = HandoffRecord{State: "activating"}
	j.Generations[second.Name] = HandoffRecord{State: "activating"}
	if saveHandoff(f.root, &j) != nil {
		t.Fatal("fixture ambiguous intent")
	}
	if _, err := InspectHandoff(t.Context(), f.root); err != ReasonStateInvalid {
		t.Fatal("ambiguous intents guessed an activation order")
	}
}

func TestHandoffRechecksOwnershipAndRetirementIdentity(t *testing.T) {
	for _, kind := range []string{"ownership", "identity"} {
		t.Run(kind, func(t *testing.T) {
			root, first, next := handoffSupersededFixture(t)
			want := ReasonOwnershipOverlap
			if kind == "ownership" {
				receipt := ChannelReceipt{Channel: "shell", OwnedPaths: []string{filepath.Join(root, "payload", first.Name)}, InstallingIdentity: strconv.Itoa(os.Getuid()), DeclaredAt: first.StagedAt, Revision: 1}
				if WriteChannelReceipt(t.Context(), root, receipt) != nil {
					t.Fatal("fixture ownership revision")
				}
				if p, _, err := Activate(t.Context(), root, next.Name, Clock{Now: next.StagedAt, Trusted: true}); err != want || p.Host != "" {
					t.Fatal("activation ignored changed package ownership")
				}
			} else {
				want = ReasonPayloadIdentity
				host := filepath.Join(root, "payload", first.Name, filepath.FromSlash(first.Host))
				replacement := host + ".replacement"
				if os.WriteFile(replacement, []byte("data"), 0755) != nil || os.Rename(replacement, host) != nil {
					t.Fatal("fixture host identity swap")
				}
			}
			called := false
			err := retireGeneration(t.Context(), root, first.Name, func(context.Context, string) (bool, error) { called = true; return false, nil })
			if err != want || called {
				t.Fatal("retirement failed to fence ownership or original executable identity before process observation")
			}
			if _, err := os.Lstat(filepath.Join(root, "payload", first.Name)); err != nil {
				t.Fatal("refused retirement removed generation")
			}
		})
	}
}

func TestLocalRecordJSONRejectsInvalidUnicode(t *testing.T) {
	for _, raw := range [][]byte{[]byte("{\"field\":\"\\ud800\"}"), {'{', '"', 'f', '"', ':', '"', 0xff, '"', '}'}} {
		if localJSONProfile(raw) != ReasonStateInvalid {
			t.Fatal("invalid local JSON unicode accepted")
		}
	}
}
