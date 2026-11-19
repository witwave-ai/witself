package localcell

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

const leaseLayerID = "ckpt_abcdefghijklmnop"

// LC-LEASE-2B R5: with its context already done, each threaded declaration
// still makes its first attempt and gives its normal result for an unleased
// file on every system; one check per expectation, each with its own text
// (Windows and macOS run it in CI only).
func TestLeaseWaitLayerUnleased(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	refused, opened := errors.New("unleased layer row refused"), func(f *os.File, err error) (bool, error) { return f != nil && f.Close() == nil, err }
	for _, row := range []struct {
		name string
		call func(c *Cell, want maintenanceFence, dir string) (bool, error)
	}{
		{"openRegular", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			return opened(openRegular(done, c.path("manifest.json"), refused, refused, refused))
		}},
		{"openRegularWriter", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			return opened(openRegularWriter(done, c.path("tmp", "unleased"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, refused, refused))
		}},
		{"openServiceRecord", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			return opened(openServiceRecord(done, c.path("journal", "maintenance.json"), refused))
		}},
		{"checkpointComplete", func(_ *Cell, _ maintenanceFence, dir string) (bool, error) {
			_, complete, err := checkpointComplete(done, dir, leaseLayerID)
			return complete, err
		}},
		{"readMaintenanceFenceJSON", func(c *Cell, want maintenanceFence, _ string) (bool, error) {
			var fence maintenanceFence
			err := readMaintenanceFenceJSON(done, c.path("journal", "maintenance.json"), &fence)
			return fence.Generation == want.Generation && fence.OperationID == want.OperationID, err
		}},
		{"readMaintenanceFence", func(c *Cell, want maintenanceFence, _ string) (bool, error) {
			fence, err := c.readMaintenanceFence(done)
			return fence != nil && fence.Generation == want.Generation && fence.OperationID == want.OperationID, err
		}},
		{"acquireLock", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			lock, err := acquireLock(done, c.cfg.Root, true)
			return lock != nil && lock.Close() == nil, err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := unitCell(t)
			normal, err := row.call(c, updateTestFence(t, c), checkpointPlant(t, c, leaseLayerID, leaseLayerID))
			if err != nil {
				t.Fatal("a threaded declaration refused an unleased file under a done context")
			}
			if !normal {
				t.Fatal("a threaded declaration gave another result for an unleased file under a done context")
			}
		})
	}
}

// The LC-LEASE-2B rows of the lease wait contract, with LF and CRLF endings:
// each mutation reports its key with both, and a method of another receiver
// that shares a threaded name is no call into the layer (no key at all).
func TestLeaseWaitLayerSourceContract(t *testing.T) {
	sources := map[string]string{}
	for name, source := range admissionReadSources(t) {
		sources[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	const writer, load = "\tf, err := openPrivateWriter(ctx, path, flags, mode)\n", "func (s *supervisor) load(d ServiceDescriptor) (result error) {\n"
	const start, regular = "func (c *Cell) Start(ctx context.Context) error {\n", "openPrivateReader, nil, path, inspect, irregular, open)\n"
	for _, row := range []struct{ name, file, before, after, want string }{
		{"writer_cut_dropped", "files.go", writer + "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn nil, cut\n\t}\n", writer, "lease_wait_cut:files.go"},
		{"fence_json_cut_refused", "update_fence.go", "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn cut\n\t} else if err != nil {\n\t\treturn refused\n\t}\n",
			"\tif err != nil {\n\t\treturn refused\n\t}\n", "lease_wait_cut:update_fence.go"},
		{"fence_cut_active", "update_fence.go", "\tif err := read(path, &fence); errors.As(err, new(*leaseWaitCut)) {\n\t\treturn nil, err\n\t} else if err != nil || !fence.valid() {\n",
			"\tif err := read(path, &fence); err != nil || !fence.valid() {\n", "lease_wait_cut:update_fence.go"},
		{"layer_context_dropped", "files.go", "\treturn openRegularBy(ctx, " + regular, "\treturn openRegularBy(context.Background(), " + regular, "lease_wait_placeholder:files.go:openRegular"},
		{"placeholder_added", "lifecycle.go", start, start + "\t_ = readMaintenanceFenceJSON(context.Background(), c.path(\"journal\", \"maintenance.json\"), new(maintenanceFence))\n",
			"lease_wait_placeholder:lifecycle.go:(*Cell).Start"},
		{"field_call_counted", "service_supervisor.go", load, load + "\t_, _ = s.cell.readMaintenanceFence(context.Background())\n", "lease_wait_placeholder:service_supervisor.go:(*supervisor).load"},
		{"other_receiver_ignored", "service_supervisor.go", load, "func (s *supervisor) readMaintenanceFence(context.Context) {}\n\n" + load + "\ts.readMaintenanceFence(context.Background())\n", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(sources[row.file], row.before) != 1 {
				t.Fatal("lease wait layer mutation anchor is not unique")
			}
			mutated := maps.Clone(sources)
			mutated[row.file] = strings.Replace(sources[row.file], row.before, row.after, 1)
			for _, ending := range []string{"\n", "\r\n"} {
				if failures := leaseWaitContract(referencesEnding(mutated, ending)); row.want == "" && len(failures) != 0 || row.want != "" && !slices.Contains(failures, row.want) {
					t.Fatalf("lease wait layer contract reported keys %v against the row", failures)
				}
			}
		})
	}
}
