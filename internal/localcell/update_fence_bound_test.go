package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUpdateFenceBound(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		size    int
		garbage bool
		allowed bool
	}{
		{"unpadded", 0, false, true},
		{"exact_bound_B1", 4096, false, true},
		{"one_over_B2", 4097, false, false},
		{"review_fixture_B3", (1 << 20) + 4096, true, false},
		{"oversized_whitespace_B4", (1 << 20) + 4096, false, false},
		{"trailing_garbage_B5", 0, true, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			c, _ := unitCell(t)
			want := updateTestFence(t, c)
			data, err := json.Marshal(want)
			if err != nil {
				t.Fatal("cannot encode fixture fence")
			}
			if len(data) < fixture.size {
				data = append(data, bytes.Repeat([]byte(" "), fixture.size-len(data))...)
				if fixture.size <= 4097 {
					data[len(data)-1] = '\n'
				}
			}
			if fixture.garbage {
				data = append(data, ' ', 'x')
			}
			path := c.path("journal", "maintenance.json")
			if testWriteFile(path, data, 0o600) != nil {
				t.Fatal("cannot prepare bounded fence fixture")
			}
			before := serviceMaintenanceSnapshot(t, c)
			got, err := c.readMaintenanceFence()
			serviceMaintenanceUnchanged(t, c, before)
			if fixture.allowed {
				if err != nil || got == nil || !got.valid() || *got != want {
					t.Fatal("complete in-bound fence was not returned unchanged")
				}
			} else if got != nil || err != errMaintenanceFenceActive {
				t.Error("refused fence did not return nil and exact maintenance cause")
			}
			for _, generation := range []int{0, want.Generation, want.Generation + 1} {
				err := c.maintenanceFenceAllows(generation)
				if fixture.allowed && generation == want.Generation {
					if err != nil {
						t.Error("complete in-bound fence refused matching generation")
					}
				} else if err != errMaintenanceFenceActive {
					t.Error("fence admitted an unauthorized generation")
				}
				// B7: compare the whole root after each call, including directory
				// entries, so even a leftover sibling temporary file is caught.
				serviceMaintenanceUnchanged(t, c, before)
			}
		})
	}
}

// The reader has no natural EOF. Its independent safety budget makes a broken
// production limit fail locally instead of allocating or reading forever.
type endlessFenceReader struct {
	prefix    []byte
	requested int
	delivered int
	over      bool
}

func (r *endlessFenceReader) Read(p []byte) (int, error) {
	r.requested += len(p)
	if r.requested > 4097 {
		r.over = true
		return 0, errors.New("fixture reader budget exceeded")
	}
	n := copy(p, r.prefix)
	r.prefix = r.prefix[n:]
	for i := n; i < len(p); i++ {
		p[i] = ' '
	}
	r.delivered += len(p)
	return len(p), nil
}

func TestUpdateFenceBoundNeverEndingInput(t *testing.T) {
	r := &endlessFenceReader{prefix: []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`)}
	start := time.Now()
	var fence maintenanceFence
	err := decodeMaintenanceFence(r, &fence)
	if err == nil || r.over || r.requested > 4097 || r.delivered != 4097 {
		t.Fatal("never-ending fence was not refused within the independent byte budget")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("bounded in-memory fence refusal did not return promptly")
	}
}

type failedFenceReader struct{}

func (failedFenceReader) Read([]byte) (int, error) {
	return 0, errors.New("fixture detail must not escape")
}

func TestUpdateFenceBoundValueFreeErrors(t *testing.T) {
	var fence maintenanceFence
	if err := decodeMaintenanceFence(failedFenceReader{}, &fence); err == nil || err.Error() != "cannot read local cell record" {
		t.Fatal("reader failure exposed variable error detail")
	}
	c, _ := unitCell(t)
	if err := readMaintenanceFenceJSON(c.path("journal", "missing.json"), &fence); err == nil || err.Error() != "cannot open local cell record" {
		t.Fatal("open failure exposed variable error detail")
	}
}

func TestUpdateFenceBoundLifecycleAdmissions(t *testing.T) {
	// Reuse the existing admission table's fixtures without adding launchers or
	// changing the existing parity tests in a different test package.
	for _, name := range admissionNames {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, name)
			updateTestFence(t, f.cell)
			path := f.cell.path("journal", "maintenance.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("cannot read fixture fence")
			}
			data = append(data, bytes.Repeat([]byte(" "), (1<<20)+1)...)
			data = append(data, 'x')
			if testWriteFile(path, data, 0o600) != nil {
				t.Fatal("cannot prepare oversized admission fence")
			}
			before := serviceMaintenanceSnapshot(t, f.cell)
			if err := f.run(t.Context()); err != errMaintenanceFenceActive {
				t.Fatal("oversized fence owner did not return exact maintenance cause")
			}
			if len(*f.trace) != 0 {
				t.Fatal("oversized fence owner reached a guarded effect")
			}
			serviceMaintenanceUnchanged(t, f.cell, before)
		})
	}
}

func TestUpdateFenceBoundFoundationGuard(t *testing.T) {
	for _, helper := range []string{"maintenanceFenceMaxBytes", "readMaintenanceFenceJSON", "decodeMaintenanceFence"} {
		t.Run(helper, func(t *testing.T) {
			sources := admissionReadSources(t)
			// M9: inject a real production source body into the guard's input,
			// without writing a forbidden production path to disk.
			sources["fence_bound_mutant.go"] = "package localcell\nfunc fenceBoundMutant() { _ = " + helper + " }\n"
			want := "fence_bound_mutant.go: fenceBoundMutant references update helper " + helper
			if got := updateFoundationViolations(sources); !reflect.DeepEqual(got, []string{want}) {
				t.Fatal("foundation guard did not identify the injected helper reference")
			}
		})
	}
}

func TestUpdateFenceBoundRejectsSecondValue(t *testing.T) {
	var fence maintenanceFence
	data := `{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"} {}`
	if err := decodeMaintenanceFence(strings.NewReader(data), &fence); err == nil {
		t.Fatal("fence decoder accepted a second JSON value")
	}
}
