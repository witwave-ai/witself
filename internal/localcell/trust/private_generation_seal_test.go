package trust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// This fake supplies observations/effects to the production protocol. It has no
// seal state machine: assertions below inspect its independently recorded native
// effects, object policy, and owned handle lifetimes.
type sealTestObject struct {
	name      string
	directory bool
	identity  [3]uint64
	policy    privateRecordPolicy
	data      []byte
	readOnly  bool
}

type sealTestOps struct {
	objects       []*sealTestObject
	handles       []*sealTestHandle
	events        []string
	seen          map[string]int
	before        func(string)
	observeHook   func(string, int, *privateGenerationSealObservation)
	parentHook    func() error
	inventoryHook func(int, []privateGenerationSealEntry) []privateGenerationSealEntry
	fail          string
	failAt        int
	closeBefore   bool
	closes        int
	sets          int
	restores      int
	foreignWrites int
	original      []byte
}

type sealTestHandle struct {
	ops      *sealTestOps
	object   *sealTestObject
	recovery bool
	closes   int
	closed   bool
}

func sealTestPolicy(identity [3]uint64, directory, sealed bool) privateRecordPolicy {
	// Independent literals: FILE_GENERIC_READ | FILE_GENERIC_EXECUTE, and FA.
	rights := uint32(0x1f01ff)
	if sealed {
		rights = 0x1200a9
	}
	return privateRecordPolicy{
		valid: true, ownerUser: true, protected: true, daclPresent: true,
		directory: directory, wantDirectory: directory,
		identity: identity, observedIdentity: identity,
		aces: []privateRecordACE{{principal: recordUser, rights: rights}, {principal: recordSystem, rights: rights}},
	}
}

func (o *sealTestOps) event(stage string) error {
	o.events = append(o.events, stage)
	o.seen[stage]++
	if o.before != nil {
		o.before(stage)
	}
	if o.fail == stage && o.seen[stage] == o.failAt {
		return ReasonStateIO
	}
	return nil
}

func (o *sealTestOps) checkParent() error {
	if err := o.event("parent"); err != nil {
		return err
	}
	if o.parentHook != nil {
		return o.parentHook()
	}
	return nil
}

func (o *sealTestOps) openRecovery() (privateGenerationSealHandle, []byte, error) {
	h := &sealTestHandle{ops: o, object: o.objects[0], recovery: true}
	o.handles = append(o.handles, h)
	return h, append([]byte(nil), o.original...), o.event("recovery-open")
}

func (o *sealTestOps) openReader(index int) (privateGenerationSealHandle, error) {
	h := &sealTestHandle{ops: o, object: o.objects[index]}
	o.handles = append(o.handles, h)
	return h, o.event("reader-open:" + h.object.name)
}

func (o *sealTestOps) observe(index int, handle privateGenerationSealHandle, stage string) (privateGenerationSealObservation, error) {
	object := o.objects[index]
	if err := o.event("observe:" + stage + ":" + object.name); err != nil {
		return privateGenerationSealObservation{}, err
	}
	identity := object.identity
	if handle != nil {
		h := handle.(*sealTestHandle)
		if h.closed {
			return privateGenerationSealObservation{}, ReasonStateIO
		}
		identity = h.object.identity
	}
	p := object.policy
	p.aces = append([]privateRecordACE(nil), p.aces...)
	p.identity, p.observedIdentity = identity, object.identity
	observed := privateGenerationSealObservation{policy: p, size: int64(len(object.data)), readOnly: object.readOnly}
	if o.observeHook != nil {
		o.observeHook(stage, index, &observed)
	}
	return observed, nil
}

func (o *sealTestOps) inventory(_ context.Context, index int, _ privateGenerationSealHandle, limit int) ([]privateGenerationSealEntry, error) {
	if err := o.event("inventory:" + o.objects[index].name); err != nil {
		return nil, err
	}
	var entries []privateGenerationSealEntry
	prefix := o.objects[index].name
	if prefix != "" {
		prefix += "/"
	}
	for _, object := range o.objects[1:] {
		if !strings.HasPrefix(object.name, prefix) {
			continue
		}
		name := strings.TrimPrefix(object.name, prefix)
		if name != "" && !strings.Contains(name, "/") {
			entries = append(entries, privateGenerationSealEntry{name: name, directory: object.directory})
		}
	}
	if o.inventoryHook != nil {
		entries = o.inventoryHook(index, entries)
	}
	if limit < 1 {
		return nil, ReasonStateIO
	}
	return entries, nil
}

func (o *sealTestOps) setRoot(handle privateGenerationSealHandle, original []byte) error {
	h := handle.(*sealTestHandle)
	if !h.recovery || h.closed {
		return ReasonStateIO
	}
	if h.object != o.objects[0] {
		o.foreignWrites++
	}
	if original == nil {
		o.sets++
		h.object.policy = sealTestPolicy(h.object.identity, true, true)
		return o.event("set")
	}
	o.restores++
	if !bytes.Equal(original, o.original) {
		return ReasonStateIO
	}
	if err := o.event("restore"); err != nil {
		return err
	}
	h.object.policy = sealTestPolicy(h.object.identity, true, false)
	return nil
}

func (o *sealTestOps) close() error {
	o.closes++
	return o.event("close-parent")
}

func (h *sealTestHandle) ReadAt(b []byte, offset int64) (int, error) {
	if err := h.ops.event("read:" + h.object.name); err != nil {
		return 0, err
	}
	if h.closed || h.recovery || h.object.directory {
		return 0, ReasonStateIO
	}
	return bytes.NewReader(h.object.data).ReadAt(b, offset)
}

func (h *sealTestHandle) Close() error {
	h.closes++
	stage := "close-reader:" + h.object.name
	if h.recovery {
		stage = "close-recovery"
	}
	err := h.ops.event(stage)
	if err == nil || !h.ops.closeBefore {
		h.closed = true
	}
	return err
}

func sealTestFixture(t *testing.T) (*privateGeneration, []privateGenerationSealExpected, [][3]uint64, *privateGenerationSealSnapshot, *sealTestOps) {
	t.Helper()
	g := generationTestStart(t, newGenerationTestOps())
	if g.mkdir(context.Background(), "nested") != nil {
		t.Fatal("D0A directory prerequisite failed")
	}
	generationTestWrite(t, g, "nested/executable", "run")
	generationTestWrite(t, g, "data", "data")
	generationTestWrite(t, g, "metadata", "private")
	if g.complete(context.Background()) != nil {
		t.Fatal("D0A completion prerequisite failed")
	}
	expected := []privateGenerationSealExpected{
		{name: "", directory: true, mode: 0500},
		{name: "nested", directory: true, mode: 0700},
		{name: "nested/executable", size: 3, digest: sha256.Sum256([]byte("run")), mode: 0755, executable: true},
		{name: "data", size: 4, digest: sha256.Sum256([]byte("data")), mode: 0644},
		{name: "metadata", size: 7, digest: sha256.Sum256([]byte("private")), mode: 0600, metadata: true},
	}
	identities := make([][3]uint64, len(expected))
	for i := range identities {
		identities[i] = [3]uint64{7, 0, uint64(i + 1)}
	}
	snapshot, err := snapshotPrivateGeneration(g, expected, identities)
	if err != nil {
		t.Fatal("completed snapshot prerequisite failed")
	}
	o := sealTestOpsForSnapshot(snapshot)
	o.objects[2].data = []byte("run")
	o.objects[3].data = []byte("data")
	o.objects[4].data = []byte("private")
	return g, expected, identities, snapshot, o
}

func sealTestOpsForSnapshot(snapshot *privateGenerationSealSnapshot) *sealTestOps {
	o := &sealTestOps{seen: map[string]int{}, failAt: 1, original: []byte("bounded original descriptor")}
	for _, node := range snapshot.nodes {
		o.objects = append(o.objects, &sealTestObject{name: node.name, directory: node.directory, identity: node.identity, policy: sealTestPolicy(node.identity, node.directory, false)})
	}
	return o
}

func sealTestClosed(t *testing.T, o *sealTestOps, uncertain bool) {
	t.Helper()
	for _, h := range o.handles {
		if h.closes != 1 || (!uncertain && !h.closed) {
			t.Fatal("owned handle close was omitted, retried, or unconfirmed")
		}
	}
	if o.closes != 1 || o.foreignWrites != 0 {
		t.Fatal("ancestor close count or foreign mutation differs")
	}
}

func sealTestRefused(t *testing.T, s *sealedPrivateGeneration, err, want error, o *sealTestOps) {
	t.Helper()
	if s != nil || err != want {
		t.Fatalf("refusal classification differs: got session=%t error=%v", s != nil, err)
	}
	sealTestClosed(t, o, true)
}

func TestPrivateGenerationSealPolicy(t *testing.T) {
	cases := []struct {
		name   string
		change func(*privateRecordPolicy)
		valid  bool
	}{
		{"exact-root-read-execute", func(*privateRecordPolicy) {}, true},
		{"reversed-order", func(p *privateRecordPolicy) { p.aces[0], p.aces[1] = p.aces[1], p.aces[0] }, true},
		{"user-is-System", func(p *privateRecordPolicy) { p.userIsSystem = true; p.aces = p.aces[:1] }, true},
		{"user-is-System-no-entries", func(p *privateRecordPolicy) { p.userIsSystem = true; p.aces = nil }, false},
		{"user-is-System-two-entries", func(p *privateRecordPolicy) { p.userIsSystem = true }, false},
		{"user-is-System-three-entries", func(p *privateRecordPolicy) { p.userIsSystem = true; p.aces = append(p.aces, p.aces[0]) }, false},
		{"user-is-System-four-entries", func(p *privateRecordPolicy) { p.userIsSystem = true; p.aces = append(p.aces, p.aces...) }, false},
		{"user-is-System-System-classified", func(p *privateRecordPolicy) { p.userIsSystem = true; p.aces = p.aces[1:] }, false},
		{"root-full-access", func(p *privateRecordPolicy) { p.aces[0].rights = 0x1f01ff; p.aces[1].rights = 0x1f01ff }, false},
		{"extra-root-write", func(p *privateRecordPolicy) { p.aces[0].rights |= 0x0002 }, false},
		{"extra-system-write", func(p *privateRecordPolicy) { p.aces[1].rights |= 0x0002 }, false},
		{"more-restrictive-mask", func(p *privateRecordPolicy) { p.aces[0].rights &^= 0x0020 }, false},
		{"system-missing-rights", func(p *privateRecordPolicy) { p.aces[1].rights &^= 0x0020 }, false},
		{"generic-mask", func(p *privateRecordPolicy) { p.aces[0].rights = 0xa0000000 }, false},
		{"system-generic-mask", func(p *privateRecordPolicy) { p.aces[1].rights = 0xa0000000 }, false},
		{"wrong-owner", func(p *privateRecordPolicy) { p.ownerUser = false }, false},
		{"invalid-descriptor", func(p *privateRecordPolicy) { p.valid = false }, false},
		{"null-dacl", func(p *privateRecordPolicy) { p.daclPresent = false }, false},
		{"unprotected", func(p *privateRecordPolicy) { p.protected = false }, false},
		{"empty-dacl", func(p *privateRecordPolicy) { p.aces = nil }, false},
		{"missing-user", func(p *privateRecordPolicy) { p.aces = p.aces[1:] }, false},
		{"missing-System", func(p *privateRecordPolicy) { p.aces = p.aces[:1] }, false},
		{"duplicate-user", func(p *privateRecordPolicy) { p.aces[1] = p.aces[0] }, false},
		{"duplicate-System", func(p *privateRecordPolicy) { p.aces[0] = p.aces[1] }, false},
		{"extra-entry", func(p *privateRecordPolicy) { p.aces = append(p.aces, p.aces[0]) }, false},
		{"unknown-trustee", func(p *privateRecordPolicy) { p.aces[1].principal = recordUnknown }, false},
		{"unknown-user-trustee", func(p *privateRecordPolicy) { p.aces[0].principal = recordUnknown }, false},
		{"inherited", func(p *privateRecordPolicy) { p.aces[0].flags = 0x10 }, false},
		{"inherit-only", func(p *privateRecordPolicy) { p.aces[0].flags = 0x08 }, false},
		{"inheritance-flags", func(p *privateRecordPolicy) { p.aces[0].flags = 0x03 }, false},
		{"deny-ace", func(p *privateRecordPolicy) { p.aces[0].kind = 1 }, false},
		{"unknown-ace", func(p *privateRecordPolicy) { p.aces[0].kind = 0xff }, false},
		{"system-wrong-kind", func(p *privateRecordPolicy) { p.aces[1].kind = 1 }, false},
		{"system-nonzero-flags", func(p *privateRecordPolicy) { p.aces[1].flags = 0x10 }, false},
		{"reparse", func(p *privateRecordPolicy) { p.reparse = true }, false},
		{"ancestor-reparse", func(p *privateRecordPolicy) { p.ancestorReparse = true }, false},
		{"wrong-type", func(p *privateRecordPolicy) { p.directory = false }, false},
		{"want-directory-false", func(p *privateRecordPolicy) { p.wantDirectory = false }, false},
		{"wrong-identity", func(p *privateRecordPolicy) { p.observedIdentity[2]++ }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sealTestPolicy([3]uint64{1, 2, 3}, true, true)
			tc.change(&p)
			err := checkPrivateGenerationSealPolicy(p)
			if tc.valid && err != nil || !tc.valid && err != ReasonStateIO {
				t.Fatal("exact sealed-root policy classification differs")
			}
		})
	}
	t.Run("B1-unchanged", func(t *testing.T) {
		if checkPrivateRecordPolicy(sealTestPolicy([3]uint64{1, 2, 3}, true, true)) != ReasonStateIO {
			t.Fatal("B1 accepted a sealed root")
		}
		if checkPrivateRecordPolicy(sealTestPolicy([3]uint64{1, 2, 3}, false, false)) != nil {
			t.Fatal("unchanged full-access descendant policy refused")
		}
	})
}

func TestPrivateGenerationSealProtocol(t *testing.T) {
	ctx := context.Background()
	t.Run("completed-snapshot-owned", func(t *testing.T) {
		g, expected, identities, snapshot, _ := sealTestFixture(t)
		expected[2].name = "changed"
		expected[2].digest[0]++
		identities[2][2]++
		if snapshot.nodes[2].name != "nested/executable" || snapshot.nodes[2].digest != sha256.Sum256([]byte("run")) || snapshot.nodes[2].identity == identities[2] {
			t.Fatal("snapshot retained caller-owned metadata")
		}
		if !g.completed || !g.terminal || g.result != nil || g.abort() != nil {
			t.Fatal("completed builder terminal state changed")
		}
	})
	t.Run("completed-only", func(t *testing.T) {
		cases := []struct {
			name string
			edit func(*privateGeneration)
		}{
			{"active", func(g *privateGeneration) { g.terminal = false }},
			{"failed", func(g *privateGeneration) { g.completed = false; g.result = ReasonStateIO }},
			{"result", func(g *privateGeneration) { g.result = ReasonStateIO }},
			{"unconfirmed-close", func(g *privateGeneration) { g.nodes[2].closed = false }},
			{"unattempted-close", func(g *privateGeneration) { g.nodes[2].attempted = false }},
			{"parent-not-closed", func(g *privateGeneration) { g.opsClosed = false }},
			{"live-reader", func(g *privateGeneration) { g.readers = append(g.readers, &privateGenerationNode{}) }},
			{"unsynced", func(g *privateGeneration) { g.nodes[2].synced = false }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				g, expected, identities, _, _ := sealTestFixture(t)
				tc.edit(g)
				if snapshot, err := snapshotPrivateGeneration(g, expected, identities); err != ReasonStateInvalid || snapshot != nil {
					t.Fatal("non-completed input accepted")
				}
			})
		}
		_, expected, identities, _, _ := sealTestFixture(t)
		if snapshot, err := snapshotPrivateGeneration(nil, expected, identities); err != ReasonStateInvalid || snapshot != nil {
			t.Fatal("nil completed input accepted")
		}
	})
	t.Run("ordered-transition-and-terminal-session", func(t *testing.T) {
		g, _, _, snapshot, o := sealTestFixture(t)
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		if err != nil || s == nil {
			t.Fatal("seal transition failed")
		}
		var effects []string
		for _, event := range o.events {
			if strings.HasPrefix(event, "reader-open:") || event == "recovery-open" || event == "set" || event == "close-recovery" {
				effects = append(effects, event)
			}
		}
		want := []string{"recovery-open", "reader-open:", "reader-open:nested", "reader-open:nested/executable", "reader-open:data", "reader-open:metadata", "set", "close-recovery"}
		if !reflect.DeepEqual(effects, want) || o.seen["observe:pre-set:"] == 0 || o.seen["observe:post-set:"] == 0 {
			t.Fatal("required acquire/check/set/readback/privileged-close order differs")
		}
		setIndex, closeIndex, inventoryBefore, inventoryAfter := -1, -1, false, false
		for i, event := range o.events {
			if event == "set" {
				setIndex = i
			}
			if event == "close-recovery" {
				closeIndex = i
			}
			if event == "inventory:" {
				if setIndex < 0 {
					inventoryBefore = true
				} else if closeIndex < 0 {
					inventoryAfter = true
				}
			}
		}
		if !inventoryBefore || !inventoryAfter || closeIndex <= setIndex {
			t.Fatal("verification did not bracket the mutation before recovery close")
		}
		if got, err := s.read(ctx, "nested/executable"); err != nil || string(got) != "run" || s.verify(ctx) != nil {
			t.Fatal("retained verified read failed")
		}
		firstClose, secondClose := s.close(), s.close()
		if firstClose != nil || secondClose != nil || g.abort() != nil || !g.completed || o.restores != 0 || checkPrivateGenerationSealPolicy(o.objects[0].policy) != nil {
			t.Fatal("successful terminal operation unsealed or changed the completed tree")
		}
		before := len(o.events)
		if _, err := s.read(ctx, "data"); err != ReasonStateIO || s.verify(ctx) != ReasonStateIO || len(o.events) != before {
			t.Fatal("closed session acquired a new capability")
		}
		sealTestClosed(t, o, false)
	})
	t.Run("identity-before-set", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		reached := false
		o.observeHook = func(stage string, index int, observed *privateGenerationSealObservation) {
			if stage == "pre-set" && index == 0 {
				reached = true
				observed.policy.identity[2]++
				observed.policy.observedIdentity = observed.policy.identity
			}
		}
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if !reached || o.sets != 0 || o.restores != 0 || o.seen["inventory:"] == 0 {
			t.Fatal("pre-set identity boundary was not reached or allowed a DACL effect")
		}
	})
	t.Run("privileged-close", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		o.fail = "close-recovery"
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if o.sets != 1 || o.seen["close-recovery"] != 1 || o.restores != 0 {
			t.Fatal("ambiguous privileged close was retried, reopened, restored, or not reached")
		}
	})
	t.Run("cancel-before-commit", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		cancelCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		o.before = func(stage string) {
			if stage == "set" {
				cancel()
			}
		}
		s, err := sealPrivateGeneration(cancelCtx, snapshot, o)
		sealTestRefused(t, s, err, context.Canceled, o)
		if o.sets != 1 || o.restores != 1 || checkPrivateRecordPolicy(o.objects[0].policy) != nil {
			t.Fatal("cancellation suppressed verified root restoration")
		}
	})
	t.Run("cancelled-partial-open-owns-handle", func(t *testing.T) {
		for _, stage := range []string{"recovery-open", "reader-open:", "reader-open:data"} {
			t.Run(stage, func(t *testing.T) {
				_, _, _, snapshot, o := sealTestFixture(t)
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				o.fail = stage
				o.before = func(observed string) {
					if observed == stage {
						cancel()
					}
				}
				s, err := sealPrivateGeneration(cancelCtx, snapshot, o)
				sealTestRefused(t, s, err, context.Canceled, o)
				if o.seen[stage] != 1 || len(o.handles) == 0 || o.sets != 0 || o.restores != 0 {
					t.Fatal("cancelled partial open lost handle ownership or reached mutation")
				}
				sealTestClosed(t, o, false)
			})
		}
	})
	t.Run("cancel-after-commit-check", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		cancelCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		o.before = func(stage string) {
			if stage == "close-recovery" {
				cancel()
			}
		}
		s, err := sealPrivateGeneration(cancelCtx, snapshot, o)
		if err != nil || s == nil || o.restores != 0 || o.sets != 1 {
			t.Fatal("cancellation after final check undid the committed transition")
		}
		if s.close() != nil {
			t.Fatal("committed session close failed")
		}
		sealTestClosed(t, o, false)
	})
	t.Run("sealed-input-refuses-fresh-verifier-accepts", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		o.objects[0].policy = sealTestPolicy(o.objects[0].identity, true, true)
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if o.sets != 0 || o.restores != 0 {
			t.Fatal("already sealed input was repaired")
		}
		fresh := sealTestOpsForSnapshot(snapshot)
		fresh.objects = o.objects
		s, err = openSealedPrivateGeneration(ctx, snapshot, fresh)
		if err != nil || s == nil || fresh.seen["recovery-open"] != 0 || fresh.sets != 0 || fresh.restores != 0 {
			t.Fatal("fresh sealed verifier required a mutation capability")
		}
		if s.close() != nil {
			t.Fatal("fresh verifier close failed")
		}
		sealTestClosed(t, fresh, false)
	})
}

func TestPrivateGenerationSealFailedSessionIsTerminal(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"verify", "read"} {
		t.Run(operation, func(t *testing.T) {
			_, _, _, snapshot, o := sealTestFixture(t)
			s, err := sealPrivateGeneration(ctx, snapshot, o)
			if err != nil {
				t.Fatal("seal prerequisite failed")
			}
			if operation == "verify" {
				o.fail, o.failAt = "observe:verify:", o.seen["observe:verify:"]+1
				err = s.verify(ctx)
			} else {
				// The full-tree read succeeds; fail the requested byte delivery.
				o.fail, o.failAt = "read:data", o.seen["read:data"]+2
				var data []byte
				data, err = s.read(ctx, "data")
				if data != nil {
					t.Fatal("failed read returned bytes")
				}
			}
			if err != ReasonStateIO || s.result != err || o.seen[o.fail] != o.failAt {
				t.Fatal("targeted I/O failure did not become the stored outcome")
			}
			o.fail = ""
			before := len(o.events)
			for range 2 {
				if s.verify(ctx) != err || len(o.events) != before {
					t.Fatal("verify after failure changed outcome or performed a backend operation")
				}
				if data, later := s.read(ctx, "data"); later != err || data != nil || len(o.events) != before {
					t.Fatal("read after failure changed outcome or performed a backend operation")
				}
				if s.close() != err || len(o.events) != before {
					t.Fatal("close after failure changed outcome or performed a backend operation")
				}
			}
			sealTestClosed(t, o, false)
		})
	}
}

func TestPrivateGenerationSealSessionOwnsSnapshot(t *testing.T) {
	ctx := context.Background()
	for _, field := range []string{"nodes", "parent-identities"} {
		t.Run(field, func(t *testing.T) {
			_, _, _, snapshot, o := sealTestFixture(t)
			guards := [][3]uint64{{11, 0, 1}, {11, 0, 2}}
			snapshot.parentIdentities = append([][3]uint64(nil), guards...)
			s, err := sealPrivateGeneration(ctx, snapshot, o)
			if err != nil {
				t.Fatal("seal prerequisite failed")
			}
			t.Cleanup(func() { _ = s.close() })
			parentChecks := 0
			// Model the native adapter's comparison of retained guards with the
			// owned snapshot; the platform-neutral protocol delegates that check.
			o.parentHook = func() error {
				parentChecks++
				if !reflect.DeepEqual(s.snapshot.parentIdentities, guards) {
					return ReasonStateIO
				}
				return nil
			}
			if field == "nodes" {
				snapshot.nodes[3].digest[0]++
			} else {
				snapshot.parentIdentities[0][2]++
			}
			if s.verify(ctx) != nil || parentChecks == 0 {
				t.Fatal("caller mutation changed session verification or bypassed parent checks")
			}
			if s.close() != nil {
				t.Fatal("owned snapshot session close failed")
			}
			sealTestClosed(t, o, false)
		})
	}
}

func TestPrivateGenerationSealSystemSessionRequiresUserPrincipal(t *testing.T) {
	_, _, _, snapshot, o := sealTestFixture(t)
	p := sealTestPolicy(o.objects[0].identity, true, true)
	p.userIsSystem = true
	p.aces = p.aces[1:] // A System-classified ACE is not the required User ACE.
	o.objects[0].policy = p
	s, err := openSealedPrivateGeneration(context.Background(), snapshot, o)
	sealTestRefused(t, s, err, ReasonStateIO, o)
	if o.seen["observe:pre-open:"] != 1 || len(o.handles) != 0 || o.sets != 0 || o.restores != 0 {
		t.Fatal("System-classified sole ACE escaped the sealed-root opening check")
	}
}

func TestPrivateGenerationSealReadChecksWholeTree(t *testing.T) {
	ctx := context.Background()
	_, _, _, snapshot, o := sealTestFixture(t)
	s, err := sealPrivateGeneration(ctx, snapshot, o)
	if err != nil {
		t.Fatal("seal prerequisite failed")
	}
	o.objects[4].data = []byte("changed") // Same size, different member from data.
	before := o.seen["read:metadata"]
	if data, err := s.read(ctx, "data"); err != ReasonStateIO || data != nil {
		t.Fatal("read accepted corruption in an unrequested member or returned bytes")
	}
	if o.seen["read:metadata"] != before+1 || o.restores != 0 || o.sets != 1 {
		t.Fatal("read did not verify the corrupt member or attempted rollback")
	}
	sealTestClosed(t, o, false)
}

func TestPrivateGenerationSealReadPostReadIdentity(t *testing.T) {
	ctx := context.Background()
	_, _, _, snapshot, o := sealTestFixture(t)
	s, err := sealPrivateGeneration(ctx, snapshot, o)
	if err != nil {
		t.Fatal("seal prerequisite failed")
	}
	reads := o.seen["read:data"]
	reached := false
	o.observeHook = func(stage string, index int, observed *privateGenerationSealObservation) {
		// Allow whole-tree verification, then fail only after bytes have been
		// buffered for the requested read, with all earlier observations valid.
		if stage == "post-read" && index == 3 && o.seen["read:data"] == reads+2 {
			reached = true
			observed.policy.observedIdentity[2]++
		}
	}
	if data, err := s.read(ctx, "data"); err != ReasonStateIO || data != nil {
		t.Fatal("post-read identity failure was accepted or exposed buffered bytes")
	}
	if !reached || o.restores != 0 || o.sets != 1 {
		t.Fatal("post-read-only identity fault was not reached or attempted rollback")
	}
	sealTestClosed(t, o, false)
}

func TestPrivateGenerationSealCheckBoundaries(t *testing.T) {
	// These observations are an independent contract for a root plus one short
	// file (one ReadAt reaching EOF). Do not derive rows from the candidate trace.
	// The second verify pass in seal is the post-set tree verification boundary.
	cases := []struct {
		name string
		want []string
	}{
		{"seal", []string{
			"pre-open:", "recovery:", "pre-open:", "reader:",
			"ancestor:", "pre-open:data", "reader:data",
			"verify:", "pre-inventory:", "post-inventory:",
			"verify:data", "ancestor:", "pre-read:data", "post-read:data",
			"pre-set:", "post-set:",
			"verify:", "pre-inventory:", "post-inventory:",
			"verify:data", "ancestor:", "pre-read:data", "post-read:data",
		}},
		{"read", []string{
			"verify:", "pre-inventory:", "post-inventory:",
			"verify:data", "ancestor:", "pre-read:data", "post-read:data",
			"ancestor:", "pre-read:data", "post-read:data",
		}},
		{"rollback", []string{
			"pre-open:", "recovery:", "pre-open:", "reader:",
			"ancestor:", "pre-open:data", "reader:data",
			"verify:", "pre-inventory:", "post-inventory:",
			"verify:data", "ancestor:", "pre-read:data", "post-read:data",
			"pre-set:", "post-set:", "rollback:", "restored:",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, snapshot, _ := sealTestFixture(t)
			snapshot.nodes = []privateGenerationSealNode{snapshot.nodes[0], snapshot.nodes[3]}
			o := sealTestOpsForSnapshot(snapshot)
			o.objects[1].data = []byte("data")
			if tc.name == "rollback" {
				o.fail = "observe:post-set:"
			}
			s, err := sealPrivateGeneration(context.Background(), snapshot, o)
			if tc.name == "rollback" {
				sealTestRefused(t, s, err, ReasonStateIO, o)
				if o.restores != 1 {
					t.Fatal("boundary trace did not restore after the post-set fault")
				}
			} else {
				if err != nil {
					t.Fatal("boundary trace seal prerequisite failed")
				}
				t.Cleanup(func() {
					if s.close() != nil {
						t.Error("boundary trace session close failed")
					}
					sealTestClosed(t, o, false)
				})
				if tc.name == "read" {
					o.events = nil
					if data, err := s.read(context.Background(), "data"); err != nil || string(data) != "data" {
						t.Fatal("boundary trace read prerequisite failed")
					}
				}
			}
			var got, want []string
			for _, event := range o.events {
				if event == "parent" || strings.HasPrefix(event, "observe:") {
					got = append(got, event)
				}
			}
			for _, boundary := range tc.want {
				want = append(want, "parent", "observe:"+boundary)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("check boundary sequence differs: got %q; want %q", got, want)
			}
		})
	}
}

func TestPrivateGenerationSealInventory(t *testing.T) {
	ctx := context.Background()
	t.Run("caller-shape-and-labels", func(t *testing.T) {
		cases := []struct {
			name string
			edit func(*privateGeneration, *[]privateGenerationSealExpected, *[][3]uint64)
		}{
			{"empty", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { *e = nil }},
			{"missing", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { *e = (*e)[:4] }},
			{"extra", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				*e = append(*e, privateGenerationSealExpected{name: "extra", mode: 0644})
			}},
			{"duplicate", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[3].name = (*e)[2].name
			}},
			{"case-alias", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[3].name = strings.ToUpper((*e)[2].name)
			}},
			{"case-mismatch", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { (*e)[3].name = "DATA" }},
			{"overlong-expected-name", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[3].name = strings.Repeat("a", 241)
			}},
			{"wrong-type", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[3].directory = true
			}},
			{"wrong-size", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { (*e)[3].size++ }},
			{"root-label", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { (*e)[0].mode = 0700 }},
			{"nested-label", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { (*e)[1].mode = 0500 }},
			{"directory-digest", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { (*e)[1].digest[0] = 1 }},
			{"directory-executable", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[1].executable = true
			}},
			{"directory-metadata", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[1].metadata = true
			}},
			{"executable-mode-false-label", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[2].executable = false
			}},
			{"data-mode-true-label", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[3].executable = true
			}},
			{"unexpected-payload-mode", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { (*e)[3].mode = 0444 }},
			{"private-mode-not-designated", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[4].metadata = false
			}},
			{"designated-metadata-executable", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) {
				(*e)[4].executable = true
			}},
			{"designated-metadata-public-mode", func(_ *privateGeneration, e *[]privateGenerationSealExpected, _ *[][3]uint64) { (*e)[4].mode = 0644 }},
			{"missing-identity", func(_ *privateGeneration, _ *[]privateGenerationSealExpected, ids *[][3]uint64) {
				(*ids)[3] = [3]uint64{}
			}},
			{"duplicate-identity", func(_ *privateGeneration, _ *[]privateGenerationSealExpected, ids *[][3]uint64) {
				(*ids)[3] = (*ids)[2]
			}},
			{"identity-count", func(_ *privateGeneration, _ *[]privateGenerationSealExpected, ids *[][3]uint64) { *ids = (*ids)[:4] }},
			{"builder-byte-accounting", func(g *privateGeneration, _ *[]privateGenerationSealExpected, _ *[][3]uint64) { g.bytes++ }},
			{"builder-name-accounting", func(g *privateGeneration, _ *[]privateGenerationSealExpected, _ *[][3]uint64) {
				delete(g.names, "data")
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				g, expected, identities, _, _ := sealTestFixture(t)
				tc.edit(g, &expected, &identities)
				if snapshot, err := snapshotPrivateGeneration(g, expected, identities); err != ReasonStateInvalid || snapshot != nil {
					t.Fatal("inconsistent caller expectation accepted")
				}
			})
		}
	})
	t.Run("invalid-snapshot-locator-shapes", func(t *testing.T) {
		for _, field := range []string{"parent-identities", "parent-path", "node-name"} {
			t.Run(field, func(t *testing.T) {
				_, _, _, snapshot, o := sealTestFixture(t)
				switch field {
				case "parent-identities":
					snapshot.parentIdentities = make([][3]uint64, 129)
				case "parent-path":
					snapshot.parentPath = strings.Repeat("a", 4*260+1)
				case "node-name":
					snapshot.nodes[3].name = strings.Repeat("a", 241)
				}
				if copied, err := clonePrivateGenerationSealSnapshot(snapshot); err != ReasonStateInvalid || copied != nil {
					t.Fatal("unbounded snapshot locator or name accepted")
				}
				s, err := sealPrivateGeneration(ctx, snapshot, o)
				sealTestRefused(t, s, err, ReasonStateInvalid, o)
				if len(o.handles) != 0 || o.sets != 0 || o.restores != 0 || !reflect.DeepEqual(o.events, []string{"close-parent"}) {
					t.Fatal("invalid bounded caller shape reached native observation or acquisition")
				}
			})
		}
	})
	t.Run("node-validator-exact-bounds", func(t *testing.T) {
		root := privateGenerationSealNode{privateGenerationSealExpected: privateGenerationSealExpected{directory: true, mode: 0500}, identity: [3]uint64{1, 0, 1}}
		file := func(name string, id uint64) privateGenerationSealNode {
			return privateGenerationSealNode{privateGenerationSealExpected: privateGenerationSealExpected{name: name, mode: 0644, digest: sha256.Sum256(nil)}, identity: [3]uint64{1, 0, id}}
		}
		for _, count := range []int{4096, 4097} {
			nodes := []privateGenerationSealNode{root}
			for i := 1; i < count; i++ {
				nodes = append(nodes, file(fmt.Sprintf("f%d", i), uint64(i+1)))
			}
			if validPrivateGenerationSealNodes(nodes) != (count == 4096) {
				t.Fatal("root-inclusive object bound differs")
			}
		}
		for _, size := range []int64{0, 1 << 31, 1<<31 + 1, -1} {
			n := file("file", 2)
			n.size = size
			if validPrivateGenerationSealNodes([]privateGenerationSealNode{root, n}) != (size >= 0 && size <= 1<<31) {
				t.Fatal("total byte bound differs")
			}
		}
		large, extra := file("large", 2), file("extra", 3)
		large.size, extra.size = 1<<31, 1
		if validPrivateGenerationSealNodes([]privateGenerationSealNode{root, large, extra}) {
			t.Fatal("aggregate byte overflow accepted")
		}
		for _, depth := range []int{16, 17} {
			nodes := []privateGenerationSealNode{root}
			name := ""
			for i := 1; i < depth; i++ {
				if name != "" {
					name += "/"
				}
				name += "d"
				nodes = append(nodes, privateGenerationSealNode{privateGenerationSealExpected: privateGenerationSealExpected{name: name, directory: true, mode: 0700}, identity: [3]uint64{1, 0, uint64(i + 1)}})
			}
			if validPrivateGenerationSealNodes(nodes) != (depth == 16) {
				t.Fatal("root-inclusive depth bound differs")
			}
		}
		for _, length := range []int{64, 65} {
			if validPrivateGenerationSealNodes([]privateGenerationSealNode{root, file(strings.Repeat("a", length), 2)}) != (length == 64) {
				t.Fatal("component bound differs")
			}
		}
		for _, length := range []int{240, 241} {
			nodes := []privateGenerationSealNode{root}
			name := ""
			for i := 0; i < 3; i++ {
				if name != "" {
					name += "/"
				}
				name += strings.Repeat("d", 60)
				nodes = append(nodes, privateGenerationSealNode{privateGenerationSealExpected: privateGenerationSealExpected{name: name, directory: true, mode: 0700}, identity: [3]uint64{1, 0, uint64(i + 2)}})
			}
			name += "/" + strings.Repeat("f", length-len(name)-1)
			nodes = append(nodes, file(name, 5))
			if validPrivateGenerationSealNodes(nodes) != (length == 240) {
				t.Fatal("relative path bound differs")
			}
		}
		for _, name := range []string{"", ".", "..", "data.", "a/b", "a\\b", "a:b", "CON", "LPT1.txt", "caf\u00e9"} {
			if validPrivateGenerationSealNodes([]privateGenerationSealNode{root, file(name, 2)}) {
				t.Fatal("unsupported or unparented name accepted")
			}
		}
	})
	t.Run("extra-member", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		reached := false
		o.inventoryHook = func(index int, entries []privateGenerationSealEntry) []privateGenerationSealEntry {
			if index == 0 {
				reached = true
				entries = append(entries, privateGenerationSealEntry{name: "undeclared"})
			}
			return entries
		}
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if !reached || len(o.handles) != len(snapshot.nodes)+1 || o.sets != 0 || o.restores != 0 || o.seen["inventory:nested"] != 0 {
			t.Fatal("extra member was not refused at the targeted retained enumeration boundary")
		}
	})
	t.Run("native-inventory-shapes", func(t *testing.T) {
		cases := []struct {
			name string
			edit func([]privateGenerationSealEntry) []privateGenerationSealEntry
		}{
			{"missing", func(e []privateGenerationSealEntry) []privateGenerationSealEntry { return e[1:] }},
			{"duplicate", func(e []privateGenerationSealEntry) []privateGenerationSealEntry { return append(e, e[0]) }},
			{"case-alias", func(e []privateGenerationSealEntry) []privateGenerationSealEntry {
				e[0].name = strings.ToUpper(e[0].name)
				return e
			}},
			{"wrong-type", func(e []privateGenerationSealEntry) []privateGenerationSealEntry {
				e[0].directory = !e[0].directory
				return e
			}},
			{"unknown-subtree", func(e []privateGenerationSealEntry) []privateGenerationSealEntry {
				return append(e, privateGenerationSealEntry{name: "unknown", directory: true})
			}},
			{"relative-entry", func(e []privateGenerationSealEntry) []privateGenerationSealEntry {
				e[0].name = "nested/other"
				return e
			}},
			{"empty-name", func(e []privateGenerationSealEntry) []privateGenerationSealEntry { e[0].name = ""; return e }},
			{"count-overflow", func(e []privateGenerationSealEntry) []privateGenerationSealEntry {
				for len(e) <= 4096 {
					e = append(e, privateGenerationSealEntry{name: "overflow"})
				}
				return e
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, _, _, snapshot, o := sealTestFixture(t)
				o.inventoryHook = func(index int, entries []privateGenerationSealEntry) []privateGenerationSealEntry {
					if index == 0 {
						return tc.edit(entries)
					}
					return entries
				}
				s, err := sealPrivateGeneration(ctx, snapshot, o)
				sealTestRefused(t, s, err, ReasonStateIO, o)
				if o.seen["inventory:"] != 1 || o.sets != 0 || o.restores != 0 {
					t.Fatal("inventory refusal effects differ")
				}
			})
		}
	})
	t.Run("digest-mismatch", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		o.objects[3].data = []byte("evil")
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if o.seen["read:data"] == 0 || o.sets != 0 || o.restores != 0 {
			t.Fatal("same-size digest mismatch did not reach bounded content verification")
		}
	})
	t.Run("readonly-payload-refuses-without-repair", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		o.objects[3].readOnly = true
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if !o.objects[3].readOnly || o.sets != 0 || o.restores != 0 {
			t.Fatal("readonly projection repaired or mutation reached")
		}
	})
	t.Run("read-rejects-changed-requested-content", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		if err != nil {
			t.Fatal("seal prerequisite failed")
		}
		if data, err := s.read(ctx, "data"); err != nil || string(data) != "data" {
			t.Fatal("initial verified read failed")
		}
		o.objects[3].data = []byte("evil")
		if data, err := s.read(ctx, "data"); err != ReasonStateIO || data != nil {
			t.Fatal("read reused cached content success")
		}
		if o.restores != 0 || o.sets != 1 {
			t.Fatal("read failure attempted an unavailable rollback")
		}
		sealTestClosed(t, o, false)
	})
}

func TestPrivateGenerationSealRollback(t *testing.T) {
	ctx := context.Background()
	// Sample the first occurrence of each event emitted after mutation. Boundary
	// completeness and order are fixed by TestPrivateGenerationSealCheckBoundaries.
	t.Run("first-emitted-post-mutation-event-failures", func(t *testing.T) {
		_, _, _, snapshot, baseline := sealTestFixture(t)
		s, err := sealPrivateGeneration(ctx, snapshot, baseline)
		if err != nil {
			t.Fatal("effect trace prerequisite failed")
		}
		if s.close() != nil {
			t.Fatal("trace session close failed")
		}
		counts, selected := map[string]int{}, map[string]bool{}
		mutated := false
		for _, stage := range baseline.events {
			counts[stage]++
			if stage == "set" {
				mutated = true
			}
			if stage == "close-recovery" {
				break
			}
			if !mutated || selected[stage] {
				continue
			}
			selected[stage] = true
			at := counts[stage]
			t.Run(stage, func(t *testing.T) {
				_, _, _, snapshot, o := sealTestFixture(t)
				o.fail, o.failAt = stage, at
				s, err := sealPrivateGeneration(ctx, snapshot, o)
				sealTestRefused(t, s, err, ReasonStateIO, o)
				if o.sets != 1 || o.restores != 1 || o.seen[stage] < at || checkPrivateRecordPolicy(o.objects[0].policy) != nil {
					t.Fatal("post-mutation refusal did not reach fault and restore exact root policy")
				}
				for i := 1; i < len(o.objects); i++ {
					if o.objects[i].identity != snapshot.nodes[i].identity || checkPrivateRecordPolicy(o.objects[i].policy) != nil {
						t.Fatal("rollback changed a descendant identity or policy")
					}
				}
				if string(o.objects[2].data) != "run" || string(o.objects[3].data) != "data" || string(o.objects[4].data) != "private" {
					t.Fatal("rollback changed completed bytes")
				}
			})
		}
		if len(selected) < 10 {
			t.Fatal("post-mutation fault coverage did not reach distinct observations/effects")
		}
	})
	t.Run("pre-mutation-failures-never-restore", func(t *testing.T) {
		for _, stage := range []string{"parent", "observe:pre-open:", "recovery-open", "observe:recovery:", "reader-open:", "reader-open:nested/executable", "observe:reader:data", "inventory:", "read:data"} {
			t.Run(stage, func(t *testing.T) {
				_, _, _, snapshot, o := sealTestFixture(t)
				o.fail = stage
				s, err := sealPrivateGeneration(ctx, snapshot, o)
				sealTestRefused(t, s, err, ReasonStateIO, o)
				if o.seen[stage] != 1 || o.sets != 0 || o.restores != 0 || checkPrivateRecordPolicy(o.objects[0].policy) != nil {
					t.Fatal("pre-mutation refusal altered the tree or missed its fault")
				}
			})
		}
	})
	t.Run("foreign-root", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		originalRoot := o.objects[0]
		foreign := &sealTestObject{name: "", directory: true, identity: [3]uint64{9, 9, 9}, data: []byte("foreign sentinel")}
		foreign.policy = sealTestPolicy(foreign.identity, true, false)
		o.before = func(stage string) {
			if stage == "observe:post-set:" {
				o.objects[0] = foreign
			}
		}
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if o.sets != 1 || o.seen["observe:rollback:"] == 0 || o.restores != 0 || o.foreignWrites != 0 || string(foreign.data) != "foreign sentinel" || checkPrivateRecordPolicy(foreign.policy) != nil || checkPrivateGenerationSealPolicy(originalRoot.policy) != nil {
			t.Fatal("foreign rollback boundary failed to preserve sentinel and sealed residue without restoration")
		}
	})
	t.Run("restore-failure", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		o.fail = "restore"
		o.observeHook = func(stage string, _ int, observed *privateGenerationSealObservation) {
			if stage == "post-set" {
				observed.policy.aces[0].rights |= 2
			}
		}
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if o.sets != 1 || o.restores != 1 || checkPrivateGenerationSealPolicy(o.objects[0].policy) != nil {
			t.Fatal("failed restoration falsely claimed clean residue")
		}
	})
	for _, closeBefore := range []bool{true, false} {
		name := "close-after-actual-close"
		if closeBefore {
			name = "close-before-confirmation"
		}
		t.Run(name, func(t *testing.T) {
			_, _, _, snapshot, o := sealTestFixture(t)
			o.fail, o.closeBefore = "close-recovery", closeBefore
			s, err := sealPrivateGeneration(ctx, snapshot, o)
			sealTestRefused(t, s, err, ReasonStateIO, o)
			if o.sets != 1 || o.restores != 0 || o.handles[0].closes != 1 || o.handles[0].closed == closeBefore || checkPrivateGenerationSealPolicy(o.objects[0].policy) != nil {
				t.Fatal("ambiguous recovery close was retried or its residue misclassified")
			}
		})
	}
	t.Run("cancelled-uncertain-cleanup-is-IO", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		cancelCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		o.before = func(stage string) {
			if stage == "set" {
				cancel()
			}
		}
		o.fail = "restore"
		s, err := sealPrivateGeneration(cancelCtx, snapshot, o)
		sealTestRefused(t, s, err, ReasonStateIO, o)
		if o.sets != 1 || o.restores != 1 {
			t.Fatal("cancelled cleanup did not attempt restoration")
		}
	})
	t.Run("failed-session-close-stays-failed", func(t *testing.T) {
		_, _, _, snapshot, o := sealTestFixture(t)
		s, err := sealPrivateGeneration(ctx, snapshot, o)
		if err != nil {
			t.Fatal("seal prerequisite failed")
		}
		o.fail, o.closeBefore = "close-reader:data", true
		if s.close() != ReasonStateIO {
			t.Fatal("uncertain reader close accepted")
		}
		before := len(o.events)
		if s.close() != ReasonStateIO || len(o.events) != before {
			t.Fatal("uncertain session close retried a raw handle")
		}
		if o.restores != 0 || checkPrivateGenerationSealPolicy(o.objects[0].policy) != nil {
			t.Fatal("session close unsealed root")
		}
		sealTestClosed(t, o, true)
	})
}
