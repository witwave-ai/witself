package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The raw service opener and the service JSON precheck historically classified
// a missing child of a non-directory differently on Windows. Host exit uses
// errors.Is on the lease read to choose its public diagnostic.
func TestServicePrivateSnapshotQuietAbsence(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	root := testPrivateRoot(t)
	blocked := filepath.Join(root, "ordinary")
	if testWriteFile(blocked, nil, 0o600) != nil {
		t.Fatal("could not create private absence fixture")
	}
	for _, row := range []struct {
		name    string
		parent  string
		json    bool
		missing bool
	}{
		{"raw_absent", root, false, true},
		{"raw_blocked", blocked, false, false},
		{"json_absent", root, true, true},
		{"json_blocked", blocked, true, runtime.GOOS == "windows"},
	} {
		t.Run(row.name, func(t *testing.T) {
			path := filepath.Join(row.parent, "record")
			var err error
			if row.json {
				err = readServiceRecordJSON(t.Context(), path, new(serviceLeaseRecord))
			} else {
				var file *os.File
				file, err = openAdmissionRecord(t.Context(), path, errors.New("private record could not open"))
				if file != nil {
					_ = file.Close()
					t.Fatal("private absence returned a file")
				}
			}
			if row.missing {
				if !errors.Is(err, os.ErrNotExist) || err.Error() != os.ErrNotExist.Error() {
					t.Fatal("private absence lost its missing result")
				}
			} else if err == nil || errors.Is(err, os.ErrNotExist) || err.Error() != "service_record_not_private" {
				t.Fatal("blocked private name lost its policy refusal")
			}
		})
	}
}
