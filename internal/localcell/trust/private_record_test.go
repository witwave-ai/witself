package trust

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func recordTestPolicy() privateRecordPolicy {
	return privateRecordPolicy{
		valid: true, ownerUser: true, protected: true, daclPresent: true,
		identity: [3]uint64{1, 2, 3}, observedIdentity: [3]uint64{1, 2, 3},
		aces: []privateRecordACE{{principal: recordUser, rights: privateRecordFullAccess}, {principal: recordSystem, rights: privateRecordFullAccess}},
	}
}

func TestTrustPrivateRecordPolicy(t *testing.T) {
	cases := []struct {
		name   string
		change func(*privateRecordPolicy)
		valid  bool
	}{
		{"owner-and-system", func(_ *privateRecordPolicy) {}, true},
		{"reversed-order", func(p *privateRecordPolicy) { p.aces[0], p.aces[1] = p.aces[1], p.aces[0] }, true},
		{"user-is-system", func(p *privateRecordPolicy) { p.userIsSystem = true; p.aces = p.aces[:1] }, true},
		{"wrong-owner", func(p *privateRecordPolicy) { p.ownerUser = false }, false},
		{"missing-owner", func(p *privateRecordPolicy) { p.ownerUser = false; p.valid = false }, false},
		{"null-dacl", func(p *privateRecordPolicy) { p.daclPresent = false }, false},
		{"unprotected-dacl", func(p *privateRecordPolicy) { p.protected = false }, false},
		{"missing-user", func(p *privateRecordPolicy) { p.aces = p.aces[1:] }, false},
		{"missing-system", func(p *privateRecordPolicy) { p.aces = p.aces[:1] }, false},
		{"duplicate-trustee", func(p *privateRecordPolicy) { p.aces[1] = p.aces[0] }, false},
		{"extra-trustee", func(p *privateRecordPolicy) {
			p.aces = append(p.aces, privateRecordACE{principal: recordUnknown, rights: privateRecordFullAccess})
		}, false},
		{"unknown-trustee", func(p *privateRecordPolicy) { p.aces[1].principal = recordUnknown }, false},
		{"inherited-ace", func(p *privateRecordPolicy) { p.aces[0].flags = 0x10 }, false},
		{"inherit-only-ace", func(p *privateRecordPolicy) { p.aces[0].flags = 0x08 }, false},
		{"inheritance-flags", func(p *privateRecordPolicy) { p.aces[0].flags = 0x03 }, false},
		{"unknown-ace", func(p *privateRecordPolicy) { p.aces[0].kind = 0x7f }, false},
		{"deny-ace", func(p *privateRecordPolicy) { p.aces[0].kind = 1 }, false},
		{"wrong-rights", func(p *privateRecordPolicy) { p.aces[0].rights-- }, false},
		{"malformed-projection", func(p *privateRecordPolicy) { p.valid = false }, false},
		{"reparse-leaf", func(p *privateRecordPolicy) { p.reparse = true }, false},
		{"reparse-ancestor", func(p *privateRecordPolicy) { p.ancestorReparse = true }, false},
		{"wrong-object-type", func(p *privateRecordPolicy) { p.directory = true }, false},
		{"changed-handle-identity", func(p *privateRecordPolicy) { p.observedIdentity[2]++ }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := recordTestPolicy()
			tc.change(&p)
			err := checkPrivateRecordPolicy(p)
			if tc.valid {
				if err != nil {
					t.Fatal("valid privacy policy refused")
				}
			} else if err != ReasonStateIO {
				t.Fatal("fixed privacy refusal required")
			}
		})
	}
}

type recordTestObject struct {
	identity [3]uint64
	data     []byte
	private  bool
}
type recordTestStore struct {
	mu       sync.Mutex
	objects  map[string]*recordTestObject
	sequence uint64
}
type recordTestOps struct {
	store                               *recordTestStore
	events                              []string
	fail                                string
	closeFirst                          bool
	replaceRead                         bool
	largeRead                           bool
	publishThenFail                     bool
	reads, publishes, removals, handles int
}
type recordTestHandle struct {
	ops      *recordTestOps
	object   *recordTestObject
	name     string
	kind     string
	position int
	closed   bool
}

const recordTestName = "record.json"
const recordTestFence = ".record.lock"
const recordTestTemp = ".record-pending"

func newRecordTestOps() *recordTestOps {
	return &recordTestOps{store: &recordTestStore{objects: map[string]*recordTestObject{
		recordTestName: {identity: [3]uint64{1, 1, 1}, data: []byte("previous complete record"), private: true},
	}, sequence: 1}, closeFirst: true}
}

func (o *recordTestOps) checkDirectory(_ bool) error {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "directory")
	if o.fail == "directory" {
		return ReasonStateIO
	}
	return nil
}
func (o *recordTestOps) inspect(name string) (privateRecordInfo, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "inspect")
	if o.fail == "inspect" {
		return privateRecordInfo{}, ReasonStateIO
	}
	obj, ok := o.store.objects[name]
	if !ok {
		return privateRecordInfo{}, os.ErrNotExist
	}
	return privateRecordInfo{identity: obj.identity, size: int64(len(obj.data))}, nil
}
func (o *recordTestOps) open(name string) (privateRecordHandle, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "open")
	if o.fail == "open" {
		return nil, ReasonStateIO
	}
	obj, ok := o.store.objects[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	if o.replaceRead {
		obj = &recordTestObject{identity: [3]uint64{1, 1, 99}, data: append([]byte(nil), obj.data...), private: true}
	}
	if o.largeRead {
		obj = &recordTestObject{identity: obj.identity, data: make([]byte, maxStateBytes+1), private: true}
	}
	o.handles++
	return &recordTestHandle{ops: o, object: obj, name: name, kind: "read"}, nil
}
func (o *recordTestOps) inspectHandle(handle privateRecordHandle) (privateRecordInfo, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	h := handle.(*recordTestHandle)
	o.events = append(o.events, "validate-"+h.kind)
	if o.fail == "validate" || o.fail == "validate-"+h.kind || !h.object.private || h.closed {
		return privateRecordInfo{}, ReasonStateIO
	}
	size := int64(len(h.object.data))
	// Model a record growing after the opened size observation, exercising the
	// stream cap independently of the metadata cap.
	if o.largeRead {
		size = 1
	}
	return privateRecordInfo{identity: h.object.identity, size: size}, nil
}
func (o *recordTestOps) create(name string, temporary bool) (privateRecordHandle, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	kind := "fence"
	if temporary {
		kind = "temp"
	}
	o.events = append(o.events, "create-private-"+kind)
	if o.fail == "create" || o.fail == "create-"+kind {
		return nil, ReasonStateIO
	}
	if _, exists := o.store.objects[name]; exists {
		return nil, ReasonStateIO
	}
	o.store.sequence++
	obj := &recordTestObject{identity: [3]uint64{1, 1, o.store.sequence}, private: true}
	o.store.objects[name] = obj
	o.handles++
	return &recordTestHandle{ops: o, object: obj, name: name, kind: kind}, nil
}
func (o *recordTestOps) removeOwned(handle privateRecordHandle) error {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	h := handle.(*recordTestHandle)
	o.events = append(o.events, "remove-"+h.kind)
	if current, ok := o.store.objects[h.name]; ok && current == h.object {
		delete(o.store.objects, h.name)
		o.removals++
	}
	return nil
}
func (o *recordTestOps) closeBeforePublish() bool { return o.closeFirst }
func (o *recordTestOps) recheck(_ string, handle privateRecordHandle, prior *privateRecordInfo) error {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "recheck")
	h := handle.(*recordTestHandle)
	if o.fail == "recheck" || o.store.objects[h.name] != h.object {
		return ReasonStateIO
	}
	current := o.store.objects[recordTestName]
	if prior == nil && current != nil || prior != nil && (current == nil || current.identity != prior.identity) {
		return ReasonStateIO
	}
	return nil
}
func (o *recordTestOps) publish(handle privateRecordHandle, name string) (bool, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "publish")
	o.publishes++
	if o.fail == "publish" {
		return false, ReasonStateIO
	}
	h := handle.(*recordTestHandle)
	delete(o.store.objects, h.name)
	o.store.objects[name] = h.object
	h.name = name
	if o.publishThenFail {
		return true, ReasonStateIO
	}
	return true, nil
}
func (o *recordTestOps) complete(_ privateRecordHandle, _ string) error {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "complete")
	if o.fail == "complete" {
		return ReasonStateIO
	}
	return nil
}
func (o *recordTestOps) close() error { return nil }
func (h *recordTestHandle) Read(b []byte) (int, error) {
	o := h.ops
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "read")
	o.reads++
	if h.closed || o.fail == "read" {
		return 0, ReasonStateIO
	}
	if h.position >= len(h.object.data) {
		return 0, io.EOF
	}
	n := copy(b, h.object.data[h.position:])
	h.position += n
	return n, nil
}
func (h *recordTestHandle) Write(b []byte) (int, error) {
	o := h.ops
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "write")
	if h.closed || !h.object.private || o.fail == "write" {
		return 0, ReasonStateIO
	}
	if o.fail == "short-write" {
		return len(b) - 1, nil
	}
	h.object.data = append(h.object.data, b...)
	return len(b), nil
}
func (h *recordTestHandle) Sync() error {
	o := h.ops
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "sync")
	if h.closed || o.fail == "sync" {
		return ReasonStateIO
	}
	return nil
}
func (h *recordTestHandle) Close() error {
	o := h.ops
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	o.events = append(o.events, "close-"+h.kind)
	if h.closed {
		return ReasonStateIO
	}
	h.closed = true
	o.handles--
	if o.fail == "close" && h.kind == "temp" {
		return ReasonStateIO
	}
	return nil
}

func assertRecordTestOrder(t *testing.T, events []string, wanted ...string) {
	t.Helper()
	last := -1
	for _, want := range wanted {
		found := -1
		for i := last + 1; i < len(events); i++ {
			if events[i] == want {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatal("required operation ordering absent")
		}
		last = found
	}
}

func TestTrustPrivateRecordProtocol(t *testing.T) {
	for _, closeFirst := range []bool{true, false} {
		name := "retained-publication-handle"
		if closeFirst {
			name = "close-before-rename"
		}
		t.Run(name, func(t *testing.T) {
			ops := newRecordTestOps()
			ops.closeFirst = closeFirst
			next := []byte("complete next record")
			err := withPrivateRecordTransaction(ops, recordTestFence, func() error {
				ops.events = append(ops.events, "callback")
				if _, err := readPrivateRecord(ops, recordTestName); err != nil {
					return err
				}
				return writePrivateRecord(ops, recordTestName, recordTestTemp, next, nil)
			})
			if err != nil {
				t.Fatal("valid transaction failed")
			}
			assertRecordTestOrder(t, ops.events, "create-private-fence", "validate-fence", "callback", "inspect", "open", "validate-read", "read", "create-private-temp", "validate-temp", "write", "sync")
			if closeFirst {
				assertRecordTestOrder(t, ops.events, "sync", "close-temp", "recheck", "publish", "complete")
			} else {
				assertRecordTestOrder(t, ops.events, "sync", "recheck", "publish", "complete", "close-temp")
			}
			if ops.handles != 0 || len(ops.store.objects) != 1 || !bytes.Equal(ops.store.objects[recordTestName].data, next) {
				t.Fatal("transaction did not close handles and retain exactly the next record")
			}
		})
	}
	t.Run("read-replaced", func(t *testing.T) {
		ops := newRecordTestOps()
		ops.replaceRead = true
		b, err := readPrivateRecord(ops, recordTestName)
		if err != ReasonStateIO || b != nil || ops.reads != 0 {
			t.Fatal("changed opened identity must refuse before returning or reading record bytes")
		}
		if ops.handles != 0 {
			t.Fatal("refused read leaked handle")
		}
	})
	t.Run("bounded-read", func(t *testing.T) {
		ops := newRecordTestOps()
		ops.largeRead = true
		b, err := readPrivateRecord(ops, recordTestName)
		if err != ReasonStateIO || b != nil {
			t.Fatal("growing record exceeded bounded read")
		}
	})
	t.Run("metadata-bound", func(t *testing.T) {
		ops := newRecordTestOps()
		ops.store.objects[recordTestName].data = make([]byte, maxStateBytes+1)
		b, err := readPrivateRecord(ops, recordTestName)
		if err != ReasonStateIO || b != nil || ops.reads != 0 {
			t.Fatal("oversized metadata must refuse before reading")
		}
	})
	t.Run("missing-optional-record", func(t *testing.T) {
		ops := newRecordTestOps()
		delete(ops.store.objects, recordTestName)
		if _, err := readPrivateRecord(ops, recordTestName); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("optional absence classification changed")
		}
	})
	for _, operation := range []string{"directory", "inspect", "open", "validate-read", "read"} {
		t.Run("read-failure-"+operation, func(t *testing.T) {
			ops := newRecordTestOps()
			ops.fail = operation
			if b, err := readPrivateRecord(ops, recordTestName); err != ReasonStateIO || b != nil {
				t.Fatal("read failure must use fixed refusal without bytes")
			}
			if ops.handles != 0 {
				t.Fatal("read failure leaked handle")
			}
		})
	}
	t.Run("callback-failure", func(t *testing.T) {
		ops := newRecordTestOps()
		before := append([]byte(nil), ops.store.objects[recordTestName].data...)
		err := withPrivateRecordTransaction(ops, recordTestFence, func() error { return ReasonStateInvalid })
		if err != ReasonStateInvalid || ops.publishes != 0 || !bytes.Equal(ops.store.objects[recordTestName].data, before) || len(ops.store.objects) != 1 || ops.handles != 0 {
			t.Fatal("failed callback must preserve previous record and release only its fence")
		}
	})
	for _, operation := range []string{"create-fence", "validate-fence"} {
		t.Run(operation+"-failure", func(t *testing.T) {
			ops := newRecordTestOps()
			ops.fail = operation
			calls := 0
			err := withPrivateRecordTransaction(ops, recordTestFence, func() error { calls++; return nil })
			if err != ReasonStateIO || calls != 0 || ops.handles != 0 || len(ops.store.objects) != 1 {
				t.Fatal("failed fence must prevent callback and release owned artifacts")
			}
		})
	}
	failures := []struct {
		name, operation string
		committed       bool
	}{
		{"creation-failure", "create-temp", false},
		{"validation-failure", "validate-temp", false},
		{"write-failure", "write", false},
		{"short-write-failure", "short-write", false},
		{"file-sync-failure", "sync", false},
		{"close-failure", "close", false},
		{"pre-publication-recheck-failure", "recheck", false},
		{"publication-failure", "publish", false},
		{"post-publication-completion-failure", "complete", true},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			ops := newRecordTestOps()
			before := append([]byte(nil), ops.store.objects[recordTestName].data...)
			sentinel := &recordTestObject{identity: [3]uint64{9, 9, 9}, data: []byte("unrelated retained artifact"), private: true}
			ops.store.objects[".unrelated-pending"] = sentinel
			next := []byte("complete next record")
			ops.fail = tc.operation
			err := writePrivateRecord(ops, recordTestName, recordTestTemp, next, nil)
			// In particular W4 must be raised here after a sync failure, before a
			// reopening could disguise publication of an unauthorized next record.
			if tc.operation == "sync" && (ops.publishes != 0 || !bytes.Equal(ops.store.objects[recordTestName].data, before)) {
				t.Fatal("file-sync failure must make zero publish calls and preserve original committed bytes")
			}
			if err != ReasonStateIO {
				t.Fatal("injected operation failure must return fixed refusal")
			}

			expected := before
			if tc.committed {
				expected = next
			}
			if !bytes.Equal(ops.store.objects[recordTestName].data, expected) {
				t.Fatal("failure violated complete old or next committed bytes")
			}
			if ops.store.objects[".unrelated-pending"] != sentinel || len(ops.store.objects) != 2 || ops.handles != 0 {
				t.Fatal("failure cleanup must remove only owned temporary artifacts and close handles")
			}
			if tc.operation != "publish" && !tc.committed && ops.publishes != 0 {
				t.Fatal("pre-publication failure must make zero publication calls")
			}

			ops.fail = ""
			reopened, reopenErr := readPrivateRecord(ops, recordTestName)
			if reopenErr != nil || !bytes.Equal(reopened, expected) {
				t.Fatal("reopening after failure did not return a complete generation")
			}
		})
	}
	t.Run("publication-reports-error-after-move", func(t *testing.T) {
		ops := newRecordTestOps()
		ops.publishThenFail = true
		next := []byte("complete next record")
		if err := writePrivateRecord(ops, recordTestName, recordTestTemp, next, nil); err != ReasonStateIO {
			t.Fatal("post-move publication failure must be reported")
		}
		b, err := readPrivateRecord(ops, recordTestName)
		if err != nil || !bytes.Equal(b, next) || len(ops.store.objects) != 1 || ops.handles != 0 {
			t.Fatal("post-move publication failure must not remove or rewind the committed next record")
		}
	})
	t.Run("retained-handle-close-failure", func(t *testing.T) {
		ops := newRecordTestOps()
		ops.closeFirst = false
		ops.fail = "close"
		next := []byte("complete next record")
		if err := writePrivateRecord(ops, recordTestName, recordTestTemp, next, nil); err != ReasonStateIO {
			t.Fatal("retained handle close failure must be reported")
		}
		if !bytes.Equal(ops.store.objects[recordTestName].data, next) || ops.handles != 0 {
			t.Fatal("post-publication close failure rewound or removed the complete record")
		}
	})
	t.Run("before-publication-hook-failure", func(t *testing.T) {
		ops := newRecordTestOps()
		before := append([]byte(nil), ops.store.objects[recordTestName].data...)
		err := writePrivateRecord(ops, recordTestName, recordTestTemp, []byte("complete next record"), func() error { return ReasonStateIO })
		if err != ReasonStateIO || ops.publishes != 0 || !bytes.Equal(before, ops.store.objects[recordTestName].data) || ops.handles != 0 || len(ops.store.objects) != 1 {
			t.Fatal("hook failure must preserve previous record and remove owned temporary artifact")
		}
	})
	t.Run("contending-writer", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		winner := newRecordTestOps()
		loser := &recordTestOps{store: winner.store, closeFirst: true}
		before := append([]byte(nil), winner.store.objects[recordTestName].data...)
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var loserCalls atomic.Int32
		go func() {
			done <- withPrivateRecordTransaction(winner, recordTestFence, func() error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ReasonStateIO
				}
			})
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("writer handshake did not enter before deadline")
		}
		err := withPrivateRecordTransaction(loser, recordTestFence, func() error { loserCalls.Add(1); return nil })
		close(release)
		select {
		case firstErr := <-done:
			if firstErr != nil {
				t.Fatal("winning writer failed")
			}
		case <-ctx.Done():
			t.Fatal("writer handshake did not finish before deadline")
		}
		if err != ReasonStateIO || loserCalls.Load() != 0 || !bytes.Equal(before, winner.store.objects[recordTestName].data) {
			t.Fatal("contending writer callback count must remain zero and prior record unchanged")
		}
		if len(winner.store.objects) != 1 || winner.handles != 0 || loser.handles != 0 {
			t.Fatal("contention leaked a fence or handle")
		}
	})
	t.Run("abandoned-fence", func(t *testing.T) {
		ops := newRecordTestOps()
		abandoned := &recordTestObject{identity: [3]uint64{2, 2, 2}, private: true}
		ops.store.objects[recordTestFence] = abandoned
		calls := 0
		err := withPrivateRecordTransaction(ops, recordTestFence, func() error { calls++; return nil })
		if err != ReasonStateIO || calls != 0 || ops.store.objects[recordTestFence] != abandoned || ops.removals != 0 {
			t.Fatal("abandoned fence must refuse without takeover or callback")
		}
	})
	for _, name := range []string{"", ".", "..", "../escape", `..\escape`, "stream:record"} {
		t.Run("invalid-leaf", func(t *testing.T) {
			ops := newRecordTestOps()
			if _, err := readPrivateRecord(ops, name); err != ReasonStateIO {
				t.Fatal("invalid record name accepted for read")
			}
			if err := writePrivateRecord(ops, name, recordTestTemp, nil, nil); err != ReasonStateIO {
				t.Fatal("invalid record name accepted for write")
			}
			if err := withPrivateRecordTransaction(ops, name, func() error { t.Fatal("invalid fence reached callback"); return nil }); err != ReasonStateIO {
				t.Fatal("invalid record name accepted for fence")
			}
		})
	}
}
