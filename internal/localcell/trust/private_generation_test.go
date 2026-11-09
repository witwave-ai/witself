package trust

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// The fake implements the production protocol's native boundary. Objects and
// handles have separate lifetimes; replacing a name never transfers ownership.
type generationTestObject struct {
	name      string
	directory bool
	private   bool
	data      []byte
	parent    *generationTestObject
	children  int
}

type generationTestOps struct {
	objects       map[string]*generationTestObject
	handles       []*generationTestHandle
	events        []string
	fail          string
	failAt        int
	seen          map[string]int
	before        func(string)
	closeBefore   bool
	closeAttempts int
	foreignDelete bool
	shortWrite    bool
}

type generationTestHandle struct {
	ops      *generationTestOps
	object   *generationTestObject
	reader   bool
	position int
	closed   bool
	closes   int
}

func newGenerationTestOps() *generationTestOps {
	return &generationTestOps{objects: map[string]*generationTestObject{}, seen: map[string]int{}, failAt: 1}
}

func (o *generationTestOps) event(stage string) error {
	o.events = append(o.events, stage)
	o.seen[stage]++
	if o.before != nil {
		o.before(stage)
	}
	if stage == o.fail && o.seen[stage] == o.failAt {
		return errors.New("synthetic native failure")
	}
	return nil
}

func (o *generationTestOps) checkParent() error { return o.event("parent") }

func (o *generationTestOps) create(parent privateGenerationHandle, name string, directory bool) (privateGenerationHandle, error) {
	var p *generationTestObject
	if parent != nil {
		p = parent.(*generationTestHandle).object
		name = p.name + "/" + name
	}
	if err := o.event("create:" + name); err != nil {
		return nil, err
	}
	if o.objects[name] != nil {
		return nil, errors.New("synthetic existing root")
	}
	object := &generationTestObject{name: name, directory: directory, private: true, parent: p}
	if p != nil {
		p.children++
	}
	o.objects[name] = object
	h := &generationTestHandle{ops: o, object: object}
	o.handles = append(o.handles, h)
	return h, o.event("created:" + name)
}

func (o *generationTestOps) check(handle privateGenerationHandle, privacy bool) error {
	h := handle.(*generationTestHandle)
	prefix := "check:"
	if !privacy {
		prefix = "owned:"
	}
	if err := o.event(prefix + h.object.name); err != nil {
		return err
	}
	if h.closed {
		return errors.New("synthetic closed handle")
	}
	for object := h.object; object != nil; object = object.parent {
		if o.objects[object.name] != object || (privacy && !object.private) {
			return errors.New("synthetic changed observation")
		}
	}
	return nil
}

func (o *generationTestOps) reopen(handle privateGenerationHandle) (privateGenerationHandle, error) {
	h := handle.(*generationTestHandle)
	if err := o.event("reopen:" + h.object.name); err != nil {
		return nil, err
	}
	r := &generationTestHandle{ops: o, object: h.object, reader: true}
	o.handles = append(o.handles, r)
	return r, o.event("reopened:" + h.object.name)
}

func (o *generationTestOps) remove(handle privateGenerationHandle) error {
	h := handle.(*generationTestHandle)
	if err := o.event("remove:" + h.object.name); err != nil {
		return err
	}
	if h.closed || o.objects[h.object.name] != h.object {
		o.foreignDelete = true
		return errors.New("synthetic foreign deletion refusal")
	}
	if h.object.children != 0 {
		return errors.New("synthetic nonempty directory")
	}
	delete(o.objects, h.object.name)
	if h.object.parent != nil {
		h.object.parent.children--
	}
	return nil
}

func (o *generationTestOps) close() error {
	o.closeAttempts++
	return o.event("close-parent")
}

func (h *generationTestHandle) Read(p []byte) (int, error) {
	if err := h.ops.event("read:" + h.object.name); err != nil {
		return 0, err
	}
	if h.closed {
		return 0, errors.New("synthetic closed reader")
	}
	if h.position == len(h.object.data) {
		return 0, io.EOF
	}
	n := copy(p, h.object.data[h.position:])
	h.position += n
	return n, nil
}

func (h *generationTestHandle) Write(p []byte) (int, error) {
	if err := h.ops.event("write:" + h.object.name); err != nil {
		return 0, err
	}
	if h.closed || h.reader {
		return 0, errors.New("synthetic nonwritable handle")
	}
	n := len(p)
	if h.ops.shortWrite && n > 0 {
		n--
	}
	h.object.data = append(h.object.data, p[:n]...)
	return n, nil
}

func (h *generationTestHandle) Sync() error { return h.ops.event("sync:" + h.object.name) }

func (h *generationTestHandle) Close() error {
	h.closes++
	stage := "close:"
	if h.reader {
		stage = "close-reader:"
	}
	err := h.ops.event(stage + h.object.name)
	if err != nil && h.ops.closeBefore {
		return err
	}
	h.closed = true
	return err
}

func generationTestStart(t *testing.T, o *generationTestOps) *privateGeneration {
	t.Helper()
	g, err := newPrivateGeneration(context.Background(), o, "root")
	if err != nil || g == nil {
		t.Fatal("private generation construction failed")
	}
	return g
}

func generationTestWrite(t *testing.T, g *privateGeneration, name, data string) {
	t.Helper()
	if err := g.write(context.Background(), name, int64(len(data)), strings.NewReader(data)); err != nil {
		t.Fatal("private generation write failed")
	}
}

func generationTestNoEffects(t *testing.T, o *generationTestOps, before int, forbidden ...string) {
	t.Helper()
	for _, event := range o.events[before:] {
		for _, prefix := range forbidden {
			if strings.HasPrefix(event, prefix) {
				t.Fatalf("forbidden effect after refusal: %s", event)
			}
		}
	}
}

func generationTestClosed(t *testing.T, o *generationTestOps) {
	t.Helper()
	for _, handle := range o.handles {
		if !handle.closed || handle.closes != 1 {
			t.Fatal("successful completion retained an owned handle or repeated close")
		}
	}
	if o.closeAttempts != 1 {
		t.Fatal("parent ownership not closed exactly once")
	}
}

func TestPrivateGenerationProtocol(t *testing.T) {
	ctx := context.Background()
	t.Run("exact-operation-order", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		generationTestWrite(t, g, "file", "abc")
		if got, err := g.read(ctx, "file"); err != nil || string(got) != "abc" {
			t.Fatal("ordered readback failed")
		}
		if g.complete(ctx) != nil {
			t.Fatal("ordered completion failed")
		}
		want := []string{
			"parent", "create:root", "created:root", "check:root",
			"check:root", "create:root/file", "created:root/file", "check:root/file",
			"check:root/file", "write:root/file", "check:root/file", "sync:root/file",
			"check:root/file", "reopen:root/file", "reopened:root/file", "check:root/file",
			"read:root/file", "read:root/file", "check:root/file", "close-reader:root/file",
			"check:root", "check:root/file", "close:root/file", "close:root", "close-parent",
		}
		if !reflect.DeepEqual(o.events, want) {
			t.Fatalf("successful protocol order differs: %v", o.events)
		}
		generationTestClosed(t, o)
	})
	t.Run("success", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		if g.mkdir(ctx, "nested") != nil {
			t.Fatal("nested directory refused")
		}
		generationTestWrite(t, g, "nested/first", "first payload")
		generationTestWrite(t, g, "second", "distinct bytes")
		for _, tc := range []struct{ name, want string }{{"nested/first", "first payload"}, {"second", "distinct bytes"}} {
			got, err := g.read(ctx, tc.name)
			if err != nil || string(got) != tc.want {
				t.Fatal("registered file did not reopen exact bytes")
			}
		}
		if g.complete(ctx) != nil {
			t.Fatal("complete unsealed tree refused")
		}
		generationTestClosed(t, o)
		if len(o.objects) != 4 {
			t.Fatal("successful tree was removed")
		}
		for _, name := range []string{"root/nested/first", "root/second"} {
			want := []string{"create:" + name, "created:" + name, "check:" + name, "check:" + name, "write:" + name, "check:" + name, "sync:" + name}
			var actual []string
			for _, event := range o.events {
				if event == "sync:"+name {
					actual = append(actual, event)
					break
				}
				if strings.HasSuffix(event, ":"+name) {
					actual = append(actual, event)
				}
			}
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("create/write/sync ordering differs: %v", actual)
			}
		}
		before := len(o.events)
		if g.complete(ctx) != nil || g.abort() != nil {
			t.Fatal("successful terminal calls not idempotent")
		}
		if len(o.events) != before {
			t.Fatal("terminal operation touched successful tree")
		}
	})

	t.Run("sync-failure", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		o.fail = "sync:root/file"
		if err := g.write(ctx, "file", 3, strings.NewReader("abc")); err != ReasonStateIO {
			t.Fatal("failed sync was not a fixed refusal")
		}
		before := len(o.events)
		if g.complete(ctx) != ReasonStateIO || g.abort() != ReasonStateIO {
			t.Fatal("failed sync became successful completion")
		}
		generationTestNoEffects(t, o, before, "create:", "write:", "sync:", "reopen:")
	})
	t.Run("repeated-read-releases-bookkeeping", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		generationTestWrite(t, g, "file", "abc")
		for i := 0; i < 20; i++ {
			got, err := g.read(ctx, "file")
			if err != nil || string(got) != "abc" || len(g.readers) != 0 {
				t.Fatal("successful read retained reader bookkeeping or changed bytes")
			}
		}
		if g.complete(ctx) != nil {
			t.Fatal("repeated reads prevented successful completion")
		}
		generationTestClosed(t, o)
	})

	for _, operation := range []string{"create", "write", "reopen"} {
		t.Run("replacement-before-"+operation, func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			name := "root"
			if operation == "reopen" {
				generationTestWrite(t, g, "file", "abc")
				name = "root/file"
			}
			foreign := &generationTestObject{name: name, private: true, directory: operation != "reopen", data: []byte("sentinel")}
			if operation != "write" {
				o.objects[name] = foreign
			}
			before := len(o.events)
			var err error
			switch operation {
			case "create":
				err = g.mkdir(ctx, "child")
			case "write":
				err = g.write(ctx, "file", 3, generationTestReader{read: func(p []byte) (int, error) {
					o.objects["root"] = foreign
					return copy(p, "abc"), io.EOF
				}})
			case "reopen":
				_, err = g.read(ctx, "file")
			}
			if err != ReasonStateIO {
				t.Fatal("observed replacement did not refuse")
			}
			generationTestNoEffects(t, o, before, operation+":")
			if o.objects[name] != foreign || !bytes.Equal(foreign.data, []byte("sentinel")) || o.foreignDelete {
				t.Fatal("replacement sentinel was touched")
			}
		})
	}

	t.Run("existing-root", func(t *testing.T) {
		o := newGenerationTestOps()
		foreign := &generationTestObject{name: "root", private: true, directory: true, data: []byte("sentinel")}
		o.objects["root"] = foreign
		g, err := newPrivateGeneration(ctx, o, "root")
		if g != nil || err != ReasonStateIO || o.objects["root"] != foreign || len(o.handles) != 0 {
			t.Fatal("existing root adopted or changed")
		}
		generationTestNoEffects(t, o, 0, "write:", "remove:")
	})

	for _, stage := range []string{"initial", "mkdir", "write", "read", "complete"} {
		t.Run("cancel-"+stage, func(t *testing.T) {
			o := newGenerationTestOps()
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if stage == "initial" {
				g, err := newPrivateGeneration(cancelled, o, "root")
				if g != nil || err != context.Canceled {
					t.Fatal("initial cancellation not propagated")
				}
				generationTestNoEffects(t, o, 0, "create:")
				return
			}
			g := generationTestStart(t, o)
			generationTestWrite(t, g, "file", "abc")
			before := len(o.events)
			var err error
			switch stage {
			case "mkdir":
				err = g.mkdir(cancelled, "nested")
			case "write":
				err = g.write(cancelled, "second", 3, strings.NewReader("abc"))
			case "read":
				_, err = g.read(cancelled, "file")
			case "complete":
				err = g.complete(cancelled)
			}
			if err != context.Canceled || g.complete(ctx) != context.Canceled {
				t.Fatal("cancellation not terminal or not propagated")
			}
			generationTestNoEffects(t, o, before, "create:", "write:", "sync:", "reopen:")
		})
	}
	t.Run("cancel-during-short-read", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		active, cancel := context.WithCancel(ctx)
		defer cancel()
		reader := generationTestReader{read: func(p []byte) (int, error) {
			cancel()
			p[0] = 'a'
			return 1, nil
		}}
		if g.write(active, "file", 3, reader) != context.Canceled {
			t.Fatal("cancellation between reader calls not propagated")
		}
		generationTestNoEffects(t, o, 0, "write:", "sync:")
	})
	t.Run("cancel-during-sync", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		active, cancel := context.WithCancel(ctx)
		defer cancel()
		o.before = func(stage string) {
			if stage == "sync:root/file" {
				cancel()
			}
		}
		if g.write(active, "file", 3, strings.NewReader("abc")) != context.Canceled || g.complete(ctx) != context.Canceled {
			t.Fatal("sync-boundary cancellation became success")
		}
	})
}

type generationTestReader struct{ read func([]byte) (int, error) }

func (r generationTestReader) Read(p []byte) (int, error) { return r.read(p) }

func TestPrivateGenerationOwnedCleanup(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"parent", "create:root", "created:root", "check:root"} {
		t.Run("construction-failure-"+stage, func(t *testing.T) {
			o := newGenerationTestOps()
			o.fail = stage
			g, err := newPrivateGeneration(ctx, o, "root")
			if g != nil || err != ReasonStateIO || len(o.objects) != 0 {
				t.Fatal("failed construction adopted or leaked an owned root")
			}
			generationTestClosed(t, o)
		})
	}
	t.Run("post-create-privacy", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		o.before = func(stage string) {
			if stage == "created:root/file" {
				o.objects["root/file"].private = false
			}
		}
		if g.write(ctx, "file", 3, strings.NewReader("abc")) != ReasonStateIO {
			t.Fatal("post-create privacy refusal bypassed")
		}
		generationTestNoEffects(t, o, 0, "write:", "sync:")
		if len(o.objects) != 0 {
			t.Fatal("owned object with invalid privacy was not safely cleaned")
		}
		generationTestClosed(t, o)
	})
	t.Run("reverse-and-terminal", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		if g.mkdir(ctx, "nested") != nil {
			t.Fatal("nested directory refused")
		}
		generationTestWrite(t, g, "nested/first", "abc")
		generationTestWrite(t, g, "second", "def")
		before := len(o.events)
		if g.abort() != ReasonStateIO {
			t.Fatal("abort reported success")
		}
		var removed []string
		var effects []string
		for _, event := range o.events[before:] {
			if strings.HasPrefix(event, "remove:") {
				removed = append(removed, event)
			}
			if strings.HasPrefix(event, "remove:") || strings.HasPrefix(event, "close:") {
				effects = append(effects, event)
			}
		}
		want := []string{"remove:root/second", "remove:root/nested/first", "remove:root/nested", "remove:root"}
		wantEffects := []string{"remove:root/second", "close:root/second", "remove:root/nested/first", "close:root/nested/first", "remove:root/nested", "close:root/nested", "remove:root", "close:root"}
		if !reflect.DeepEqual(removed, want) || !reflect.DeepEqual(effects, wantEffects) || len(o.objects) != 0 {
			t.Fatal("cleanup did not remove reverse owned order before each close")
		}
		generationTestClosed(t, o)
		before = len(o.events)
		if g.abort() != ReasonStateIO || g.complete(ctx) != ReasonStateIO || len(o.events) != before {
			t.Fatal("failed terminal calls retried effects")
		}
	})

	for _, stage := range []string{"create:root/file", "created:root/file", "check:root/file", "write:root/file", "sync:root/file", "reopen:root/file", "reopened:root/file", "read:root/file"} {
		t.Run("failure-"+strings.ReplaceAll(stage, "/", "-"), func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			o.fail = stage
			err := g.write(ctx, "file", 3, strings.NewReader("abc"))
			if err == nil {
				_, err = g.read(ctx, "file")
			}
			if err != ReasonStateIO || g.complete(ctx) != ReasonStateIO {
				t.Fatal("injected failure did not stay failed")
			}
			if len(o.objects) != 0 {
				t.Fatal("unambiguous failed operation left owned objects")
			}
			generationTestClosed(t, o)
		})
	}

	t.Run("foreign-name-preserved", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		generationTestWrite(t, g, "file", "owned")
		foreign := &generationTestObject{name: "root/file", data: []byte("foreign sentinel"), private: true}
		o.objects["root/file"] = foreign
		before := len(o.events)
		if g.abort() != ReasonStateIO {
			t.Fatal("ambiguous rollback reported success")
		}
		generationTestNoEffects(t, o, before, "remove:root/file")
		if o.objects["root/file"] != foreign || string(foreign.data) != "foreign sentinel" || o.foreignDelete {
			t.Fatal("cleanup deleted a foreign registered name")
		}
	})

	t.Run("cleanup-failure-preserves-residue", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		generationTestWrite(t, g, "file", "abc")
		o.fail = "remove:root/file"
		if g.abort() != ReasonStateIO || len(o.objects) != 2 {
			t.Fatal("cleanup failure hid ambiguous residue")
		}
		generationTestClosed(t, o)
	})
	t.Run("parent-close-failure", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		generationTestWrite(t, g, "file", "abc")
		o.fail = "close-parent"
		if g.complete(ctx) != ReasonStateIO || g.completed || len(o.objects) != 2 {
			t.Fatal("ambiguous parent close reported a successful or removed tree")
		}
		before := len(o.events)
		if g.abort() != ReasonStateIO || g.complete(ctx) != ReasonStateIO || len(o.events) != before || o.closeAttempts != 1 {
			t.Fatal("ambiguous parent close retried")
		}
	})
	for _, stage := range []string{"reopened:root/file", "read:root/file"} {
		t.Run("replacement-during-"+stage, func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			generationTestWrite(t, g, "file", "abc")
			foreign := &generationTestObject{name: "root/file", data: []byte("sentinel"), private: true}
			o.before = func(event string) {
				if event == stage {
					o.objects["root/file"] = foreign
				}
			}
			got, err := g.read(ctx, "file")
			if err != ReasonStateIO || got != nil || o.objects["root/file"] != foreign || o.foreignDelete {
				t.Fatal("readback silently accepted or removed a replacement")
			}
			if stage == "reopened:root/file" && o.seen["read:root/file"] != 0 {
				t.Fatal("payload read after observed reopen replacement")
			}
		})
	}

	for _, beforeClosure := range []bool{false, true} {
		for _, reader := range []bool{false, true} {
			t.Run(fmt.Sprintf("close-before-%t-reader-%t", beforeClosure, reader), func(t *testing.T) {
				o := newGenerationTestOps()
				g := generationTestStart(t, o)
				generationTestWrite(t, g, "file", "abc")
				o.closeBefore = beforeClosure
				o.fail = "close:root/file"
				var err error
				if reader {
					o.fail = "close-reader:root/file"
					_, err = g.read(ctx, "file")
				} else {
					err = g.complete(ctx)
				}
				if err != ReasonStateIO {
					t.Fatal("ambiguous close became success")
				}
				last := len(o.events)
				if g.complete(ctx) != ReasonStateIO || g.abort() != ReasonStateIO || len(o.events) != last {
					t.Fatal("ambiguous terminal close retried")
				}
				matched := 0
				for _, h := range o.handles {
					if h.object.name == "root/file" && h.reader == reader {
						matched++
						if h.closes != 1 || h.closed == beforeClosure {
							t.Fatal("close uncertainty or actual closure not preserved")
						}
					}
				}
				if matched != 1 {
					t.Fatal("intended close boundary not exercised")
				}
			})
		}
	}
}

func TestPrivateGenerationNamesAndBounds(t *testing.T) {
	ctx := context.Background()
	invalid := []string{"", ".", "..", "/absolute", `C:\drive`, `\\server\share`, `\\?\device`, "a:b", "a//b", "a/./b", "a/../b", "a/", "a\\b", "a ", "a.", "a~1", "CON", "prn.txt", "AUX", "nul", "COM1", "lpt9.txt", "CLOCK$", "é", "a\x00b", strings.Repeat("a", 65)}
	for index, name := range invalid {
		t.Run(fmt.Sprintf("invalid-%d", index), func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			before := len(o.events)
			if err := g.mkdir(ctx, name); err != ReasonStateInvalid {
				t.Fatal("unsafe name did not receive fixed invalid classification")
			}
			generationTestNoEffects(t, o, before, "create:")
		})
	}
	for _, name := range []string{"Case", "case", "case/child", "Case/../child"} {
		t.Run("collision-"+strings.ReplaceAll(name, "/", "-"), func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			if g.mkdir(ctx, "Case") != nil {
				t.Fatal("safe mixed case name refused")
			}
			if g.mkdir(ctx, name) != ReasonStateInvalid {
				t.Fatal("duplicate or alias parent accepted")
			}
		})
	}
	t.Run("ascii-and-component-boundary", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		for _, name := range []string{"ABC_xyz-09.txt", strings.Repeat("a", 64), "COM10", "LPT0"} {
			if g.mkdir(ctx, name) != nil {
				t.Fatal("exact safe component boundary refused")
			}
		}
		if g.complete(ctx) != nil {
			t.Fatal("bounded tree completion refused")
		}
	})
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("depth-extra-%t", extra), func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			path := ""
			for i := 0; i < 15; i++ { // Root plus fifteen descendants: sixteen.
				if path != "" {
					path += "/"
				}
				path += "d"
				if g.mkdir(ctx, path) != nil {
					t.Fatal("exact depth boundary refused")
				}
			}
			if extra {
				if g.mkdir(ctx, path+"/d") != ReasonStateInvalid {
					t.Fatal("excess depth accepted")
				}
			} else if g.complete(ctx) != nil {
				t.Fatal("maximum depth did not complete")
			}
		})
	}
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("path-extra-%t", extra), func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			path := ""
			for _, size := range []int{64, 64, 64} {
				if path != "" {
					path += "/"
				}
				path += strings.Repeat("a", size)
				if g.mkdir(ctx, path) != nil {
					t.Fatal("bounded path ancestor refused")
				}
			}
			leafSize := 45 // Three separators plus 64+64+64+45 equals 240.
			if extra {
				leafSize++
			}
			err := g.mkdir(ctx, path+"/"+strings.Repeat("b", leafSize))
			if extra {
				if err != ReasonStateInvalid {
					t.Fatal("241-byte relative path accepted")
				}
			} else if err != nil || g.complete(ctx) != nil {
				t.Fatal("240-byte relative path refused")
			}
		})
	}
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("objects-extra-%t", extra), func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			for i := 1; i < 4096; i++ { // The inventory cap includes the root.
				if g.mkdir(ctx, fmt.Sprintf("d%d", i)) != nil {
					t.Fatal("exact object boundary refused")
				}
			}
			if extra {
				if g.mkdir(ctx, "overflow") != ReasonStateInvalid {
					t.Fatal("object overflow accepted")
				}
			} else if g.complete(ctx) != nil {
				t.Fatal("maximum object tree did not complete")
			}
		})
	}
	for _, tc := range []struct {
		name     string
		expected int64
		data     string
		want     error
	}{
		{"empty", 0, "", nil}, {"exact", 3, "abc", nil}, {"truncated", 4, "abc", ReasonStateIO},
		{"overflow", 2, "abc", ReasonStateIO}, {"negative", -1, "", ReasonStateInvalid},
		{"over-limit", (2 << 30) + 1, "", ReasonStateInvalid},
	} {
		t.Run("bytes-"+tc.name, func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			err := g.write(ctx, "file", tc.expected, strings.NewReader(tc.data))
			if err != tc.want {
				t.Fatalf("byte contract classification differs: %v", err)
			}
			if tc.want == nil {
				if g.complete(ctx) != nil {
					t.Fatal("exact byte count failed completion")
				}
			} else if o.seen["sync:root/file"] != 0 || g.complete(ctx) != tc.want {
				t.Fatal("invalid byte count synced or completed")
			}
		})
	}
	t.Run("short-write", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		o.shortWrite = true
		if g.write(ctx, "file", 3, strings.NewReader("abc")) != ReasonStateIO || o.seen["sync:root/file"] != 0 {
			t.Fatal("short write continued to sync")
		}
	})
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("aggregate-byte-boundary-extra-%t", extra), func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			// Seed only the accounting counter: testing the last three bytes
			// avoids allocating or streaming a two-gigabyte fixture per run.
			g.bytes = (2 << 30) - 3
			generationTestWrite(t, g, "last", "abc")
			if g.bytes != 2<<30 {
				t.Fatal("exact aggregate byte boundary miscounted")
			}
			if extra {
				before := len(o.events)
				if g.write(ctx, "overflow", 1, strings.NewReader("x")) != ReasonStateInvalid {
					t.Fatal("aggregate byte overflow accepted")
				}
				generationTestNoEffects(t, o, before, "create:", "write:", "sync:")
			} else {
				generationTestWrite(t, g, "empty", "")
				if g.complete(ctx) != nil {
					t.Fatal("maximum aggregate byte budget refused")
				}
			}
		})
	}
	t.Run("bounded-chunks", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		payload := strings.Repeat("a", (32<<10)+3)
		generationTestWrite(t, g, "file", payload)
		if o.seen["write:root/file"] != 2 {
			t.Fatal("file writes did not use bounded chunks")
		}
		got, err := g.read(ctx, "file")
		if err != nil || string(got) != payload || g.complete(ctx) != nil {
			t.Fatal("bounded chunks changed exact readback")
		}
	})
	for _, tc := range []struct {
		name string
		read func([]byte) (int, error)
	}{
		{"no-progress", func([]byte) (int, error) { return 0, nil }},
		{"reader-error", func([]byte) (int, error) { return 0, errors.New("synthetic read failure") }},
		{"invalid-count", func(p []byte) (int, error) { return len(p) + 1, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newGenerationTestOps()
			g := generationTestStart(t, o)
			if g.write(ctx, "file", 3, generationTestReader{read: tc.read}) != ReasonStateIO {
				t.Fatal("invalid reader outcome not refused")
			}
			generationTestNoEffects(t, o, 0, "write:", "sync:")
		})
	}
	t.Run("unregistered-read", func(t *testing.T) {
		o := newGenerationTestOps()
		g := generationTestStart(t, o)
		if _, err := g.read(ctx, "unknown"); err != ReasonStateInvalid {
			t.Fatal("unregistered file reopened")
		}
		generationTestNoEffects(t, o, 0, "reopen:")
	})
}

func TestPrivateGenerationObservationCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		action      string
		stage       string
		visit       int
		prefix      []string
		objects     int
		readers     int
		storedBytes int
	}{
		{
			name: "constructor-parent-check", action: "construct", stage: "parent", visit: 1,
			prefix: []string{"parent"},
		},
		{
			name: "constructor-root-check", action: "construct", stage: "check:root", visit: 1,
			prefix: []string{"parent", "create:root", "created:root", "check:root"}, objects: 1,
		},
		{
			name: "mkdir-parent-check", action: "mkdir", stage: "check:root/branch", visit: 1,
			prefix: []string{"check:root/branch"}, objects: 2,
		},
		{
			name: "write-created-file-check", action: "write", stage: "check:root/file", visit: 1,
			prefix: []string{"check:root", "create:root/file", "created:root/file", "check:root/file"}, objects: 2,
		},
		{
			name: "write-prewrite-check", action: "write", stage: "check:root/file", visit: 2,
			prefix: []string{"check:root", "create:root/file", "created:root/file", "check:root/file", "check:root/file"}, objects: 2,
		},
		{
			name: "write-presync-check", action: "write", stage: "check:root/file", visit: 3,
			prefix:  []string{"check:root", "create:root/file", "created:root/file", "check:root/file", "check:root/file", "write:root/file", "check:root/file"},
			objects: 2, storedBytes: 3,
		},
		{
			name: "read-prereopen-check", action: "read", stage: "check:root/file", visit: 1,
			prefix: []string{"check:root/file"}, objects: 2, storedBytes: 3,
		},
		{
			name: "read-reopened-check", action: "read", stage: "check:root/file", visit: 2,
			prefix:  []string{"check:root/file", "reopen:root/file", "reopened:root/file", "check:root/file"},
			objects: 2, readers: 1, storedBytes: 3,
		},
		{
			name: "read-postread-check", action: "read", stage: "check:root/file", visit: 3,
			prefix:  []string{"check:root/file", "reopen:root/file", "reopened:root/file", "check:root/file", "read:root/file", "read:root/file", "check:root/file"},
			objects: 2, readers: 1, storedBytes: 3,
		},
		{
			name: "complete-validation-check", action: "complete", stage: "check:root", visit: 1,
			prefix: []string{"check:root"}, objects: 2, storedBytes: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newGenerationTestOps()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var g *privateGeneration
			if tc.action != "construct" {
				g = generationTestStart(t, o)
			}
			switch tc.action {
			case "mkdir":
				if g.mkdir(ctx, "branch") != nil {
					t.Fatal("observation-setup-refused")
				}
			case "read", "complete":
				generationTestWrite(t, g, "file", "abc")
			}

			// Arm only after setup. Count visits independently of the fake's seen
			// map, then pin the observation with an independently specified trace.
			before := len(o.events)
			trigger, visits, fired := -1, 0, 0
			active := false
			var acquired []*generationTestHandle
			objects, readers, storedBytes := 0, 0, 0
			o.before = func(stage string) {
				if stage != tc.stage {
					return
				}
				visits++
				if visits != tc.visit {
					return
				}
				fired++
				active = ctx.Err() == nil
				trigger = len(o.events) - 1
				acquired = append(acquired, o.handles...)
				objects = len(o.objects)
				for _, h := range acquired {
					if h.closed || h.closes != 0 {
						t.Fatal("observation-premature-close")
					}
					if h.reader {
						readers++
					}
				}
				for _, object := range o.objects {
					storedBytes += len(object.data)
				}
				cancel()
			}
			var err error
			var data []byte
			switch tc.action {
			case "construct":
				g, err = newPrivateGeneration(ctx, o, "root")
			case "mkdir":
				err = g.mkdir(ctx, "branch/leaf")
			case "write":
				err = g.write(ctx, "file", 3, strings.NewReader("abc"))
			case "read":
				data, err = g.read(ctx, "file")
			case "complete":
				err = g.complete(ctx)
			}
			after := len(o.events)
			if fired != 1 || !active || ctx.Err() != context.Canceled || trigger < before || trigger >= after {
				t.Fatal("observation-trigger-mismatch")
			}
			if !reflect.DeepEqual(o.events[before:trigger+1], tc.prefix) {
				t.Fatal("observation-prefix-mismatch")
			}
			// Check effects before the returned error: a later ready check can
			// return cancellation while hiding an earlier forbidden effect.
			for _, event := range o.events[trigger+1 : after] {
				for _, prefix := range []string{"create:", "write:", "sync:", "reopen:", "read:"} {
					if strings.HasPrefix(event, prefix) {
						t.Fatal("observation-forbidden-effect")
					}
				}
			}
			if objects != tc.objects || readers != tc.readers || len(acquired) != tc.objects+tc.readers || storedBytes != tc.storedBytes {
				t.Fatal("observation-owned-state-mismatch")
			}
			if err != context.Canceled || data != nil {
				t.Fatal("observation-cancellation-result-mismatch")
			}
			if tc.action == "construct" {
				if g != nil {
					t.Fatal("observation-constructor-returned-builder")
				}
			} else if g == nil || !g.terminal || g.completed || g.result != context.Canceled {
				t.Fatal("observation-terminal-state-mismatch")
			}

			// Derive cleanup from acquired fake handles, not builder bookkeeping.
			// Readers close first; each owned object is checked, removed and closed
			// in reverse creation order. Postread's necessary reader close is allowed.
			var cleanup []string
			for i := len(acquired) - 1; i >= 0; i-- {
				if acquired[i].reader {
					cleanup = append(cleanup, "close-reader:"+acquired[i].object.name)
				}
			}
			for i := len(acquired) - 1; i >= 0; i-- {
				if !acquired[i].reader {
					name := acquired[i].object.name
					cleanup = append(cleanup, "owned:"+name, "remove:"+name, "close:"+name)
				}
			}
			cleanup = append(cleanup, "close-parent")
			if !reflect.DeepEqual(o.events[trigger+1:after], cleanup) || len(o.handles) != len(acquired) || len(o.objects) != 0 || o.foreignDelete {
				t.Fatal("observation-cleanup-mismatch")
			}
			generationTestClosed(t, o)
			if g != nil {
				fresh := context.Background()
				if g.abort() != context.Canceled || g.complete(fresh) != context.Canceled ||
					g.mkdir(fresh, "later") != context.Canceled ||
					g.write(fresh, "later", 3, strings.NewReader("abc")) != context.Canceled {
					t.Fatal("observation-terminal-call-mismatch")
				}
				if got, readErr := g.read(fresh, "file"); got != nil || readErr != context.Canceled {
					t.Fatal("observation-terminal-read-mismatch")
				}
				if len(o.events) != after || g.completed || len(o.objects) != 0 || o.foreignDelete {
					t.Fatal("observation-terminal-effect")
				}
				generationTestClosed(t, o)
			}
		})
	}
}

func TestPrivateGenerationConstructorRoots(t *testing.T) {
	// Four independently counted sixteen-byte ASCII groups: this constructor
	// boundary is separate from B1's limit on a complete native absolute path.
	exactRoot := "rrrrrrrrrrrrrrrr" + "rrrrrrrrrrrrrrrr" + "rrrrrrrrrrrrrrrr" + "rrrrrrrrrrrrrrrr"
	if len(exactRoot) != 64 || len(exactRoot+"r") != 65 {
		t.Fatal("constructor-boundary-fixture-mismatch")
	}
	for _, tc := range []struct {
		name  string
		root  string
		valid bool
	}{
		{name: "empty", root: ""},
		{name: "dot", root: "."},
		{name: "dot-dot", root: ".."},
		{name: "absolute", root: "/root"},
		{name: "rooted-backslash", root: `\root`},
		{name: "drive-absolute", root: `C:\root`},
		{name: "drive-relative", root: "C:root"},
		{name: "unc", root: `\\server\share\root`},
		{name: "device-path", root: `\\.\root`},
		{name: "extended-path", root: `\\?\C:\root`},
		{name: "alternate-stream", root: "root:stream"},
		{name: "reserved-console", root: "CON"},
		{name: "reserved-printer", root: "PRN"},
		{name: "reserved-auxiliary", root: "AUX"},
		{name: "reserved-null-extension", root: "nul.txt"},
		{name: "reserved-serial", root: "COM1"},
		{name: "reserved-parallel", root: "LPT9"},
		{name: "reserved-console-input", root: "CONIN$"},
		{name: "reserved-console-output", root: "CONOUT$"},
		{name: "reserved-clock", root: "CLOCK$"},
		{name: "tilde-alias", root: "ROOT~1"},
		{name: "trailing-dot", root: "root."},
		{name: "trailing-space", root: "root "},
		{name: "embedded-space", root: "two roots"},
		{name: "unsupported-character", root: "root?"},
		{name: "non-ascii", root: "røot"},
		{name: "overlong-component", root: exactRoot + "r"},
		{name: "nested-root", root: "outer/inner"},
		{name: "exact-64-byte", root: exactRoot, valid: true},
		{name: "ordinary-root", root: "root", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newGenerationTestOps()
			g, err := newPrivateGeneration(context.Background(), o, tc.root)
			if !tc.valid {
				if g != nil || err != ReasonStateInvalid {
					t.Error("constructor-refusal-mismatch")
				}
				if !reflect.DeepEqual(o.events, []string{"close-parent"}) || len(o.objects) != 0 || len(o.handles) != 0 || o.foreignDelete {
					t.Error("constructor-refusal-effect")
				}
				generationTestClosed(t, o)
				return
			}
			if err != nil || g == nil {
				t.Fatal("constructor-control-refused")
			}
			want := []string{"parent", "create:" + tc.root, "created:" + tc.root, "check:" + tc.root}
			if !reflect.DeepEqual(o.events, want) || len(o.objects) != 1 || len(o.handles) != 1 || o.closeAttempts != 0 {
				t.Fatal("constructor-control-acquisition-mismatch")
			}
			h := o.handles[0]
			if o.objects[tc.root] != h.object || !h.object.directory || !h.object.private || h.object.parent != nil || h.reader || h.closed || h.closes != 0 {
				t.Fatal("constructor-control-ownership-mismatch")
			}
			if g.complete(context.Background()) != nil || !g.terminal || !g.completed || g.result != nil {
				t.Fatal("constructor-control-completion-mismatch")
			}
			want = append(want, "check:"+tc.root, "close:"+tc.root, "close-parent")
			if !reflect.DeepEqual(o.events, want) || len(o.objects) != 1 || o.objects[tc.root] != h.object || o.foreignDelete {
				t.Fatal("constructor-control-tree-mismatch")
			}
			generationTestClosed(t, o)
			if g.abort() != nil || g.complete(context.Background()) != nil || !g.completed ||
				!reflect.DeepEqual(o.events, want) || len(o.objects) != 1 || o.objects[tc.root] != h.object || o.foreignDelete {
				t.Fatal("constructor-control-terminal-mismatch")
			}
			generationTestClosed(t, o)
		})
	}
}
