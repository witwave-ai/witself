package localcell

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
)

// reader2Row is one more reader that must never wait on what it opens. setup
// builds a fixture whose file at the returned name is valid and regular, and a
// read that calls plant(name) where the reader's own pathname check, if it
// keeps one, gives way to its open; the read is nil only when the content
// arrived intact. fifo, link and empty are the refusals (substrings) for a FIFO
// or a link planted there and for an empty regular file already there.
type reader2Row struct {
	name, fifo, link, empty string
	setup                   func(t *testing.T, plant func(string)) (string, func() error)
}

const reader2Unavailable = "local cell password file is unavailable or not private"

// reader2Rows are the readers whose fixtures every platform builds; the linux
// and darwin file appends the PostgreSQL and service manager readers.
var reader2Rows = []reader2Row{
	{"password", reader2Unavailable, reader2Unavailable, "local cell password file is invalid", func(t *testing.T, plant func(string)) (string, func() error) {
		c, _ := unitCell(t)
		if c.makePassword() != nil {
			t.Fatal("cannot prepare the reader password")
		}
		path := c.path("run", "postgres.password")
		return path, func() error { plant(path); return reader2Value(c.password(context.Background())) }
	}},
	{"serviceControlSecret", "service_record_not_private", "service_record_not_private", "service_control_secret_invalid", func(t *testing.T, plant func(string)) (string, func() error) {
		c, _ := unitCell(t)
		path := c.path("service", "control.secret")
		if privateDir(c.path("service"), true) != nil || writeAtomicBytes(path, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), nil) != nil {
			t.Fatal("cannot prepare the reader control secret")
		}
		return path, func() error { plant(path); return reader2Value(c.serviceControlSecret()) }
	}},
	// The credential keeps its pinned check; the existing open seam plants.
	{"credentialPassword", ErrLocalPrincipalCredentialsRefused.Error(), ErrLocalPrincipalCredentialsRefused.Error(), ErrLocalPrincipalCredentialsRefused.Error(), func(t *testing.T, plant func(string)) (string, func() error) {
		c, m, _ := selectedCredentialFixture(t)
		path, err := c.credentialPath(credentialServer)
		if err != nil {
			t.Fatal("cannot locate the reader credential")
		}
		realOpen := openPrincipalCredentialFile
		t.Cleanup(func() { openPrincipalCredentialFile = realOpen })
		openPrincipalCredentialFile = func(ctx context.Context, name string, flags int, mode os.FileMode) (*os.File, error) {
			plant(name)
			return realOpen(ctx, name, flags, mode)
		}
		return path, func() error { return reader2Value(c.credentialPassword(context.Background(), m, credentialServer)) }
	}},
	// The fence keeps its observing Lstat; the existing read parameter plants.
	// An empty fence stays active: it is never read as absent.
	{"maintenanceFence", errMaintenanceFenceActive.Error(), errMaintenanceFenceActive.Error(), errMaintenanceFenceActive.Error(), func(t *testing.T, plant func(string)) (string, func() error) {
		c, _ := unitCell(t)
		want := updateTestFence(t, c)
		return c.path("journal", "maintenance.json"), func() error {
			fence, err := c.readMaintenanceFenceWith(func(name string, dst any) error {
				plant(name)
				return readMaintenanceFenceJSON(context.Background(), name, dst)
			})
			if err == nil && (fence == nil || fence.Generation != want.Generation || fence.OperationID != want.OperationID) {
				return errors.New("fence content lost")
			}
			return err
		}
	}},
	{"serviceControlEndpoint", "endpoint:record", "endpoint:record", "endpoint:record", func(t *testing.T, plant func(string)) (string, func() error) {
		c, _ := unitCell(t)
		path := c.path("service", "control-endpoint.json")
		if privateDir(c.path("service"), true) != nil || writeAtomic(path, serviceControlEndpointRecord{Version: 1}, nil) != nil {
			t.Fatal("cannot prepare the reader endpoint record")
		}
		return path, func() error {
			plant(path)
			record, err := c.readServiceControlEndpointRecord()
			if err == nil && record.Version != 1 {
				return errors.New("endpoint content lost")
			}
			return err
		}
	}},
	// An empty update record is present and partial, never absent.
	{"derivedUpdateRecord", errUpdatePartialState.Error(), errUpdatePartialState.Error(), errUpdatePartialState.Error(), func(t *testing.T, plant func(string)) (string, func() error) {
		c, record := updateRecordFixture(t)
		path := c.updateRecordPath(record.OperationID)
		if writeAtomic(path, record, nil) != nil {
			t.Fatal("cannot prepare the reader update record")
		}
		return path, func() error {
			plant(path)
			got, absent, err := c.readDerivedUpdateRecord(record.OperationID)
			if absent || err == nil && (got == nil || *got != *record) {
				return errors.New("update record content lost")
			}
			return err
		}
	}},
}

// reader2Value accepts a secret only with its whole value, never printed.
func reader2Value(value string, err error) error {
	if err == nil && len(value) < 24 {
		return errors.New("reader value lost")
	}
	return err
}

func TestReaders2StillReadRegularFiles(t *testing.T) {
	for _, row := range reader2Rows {
		t.Run(row.name, func(t *testing.T) {
			_, read := row.setup(t, func(string) {})
			if err := readerWatchdog(t, read, func() {}); err != nil {
				t.Fatalf("reader refused its regular file: %v", err)
			}
		})
	}
}

// An empty regular file keeps the outcome its caller acts on today.
func TestReaders2EmptyRegularFileOutcomes(t *testing.T) {
	for _, row := range reader2Rows {
		t.Run(row.name, func(t *testing.T) {
			path, read := row.setup(t, func(string) {})
			if os.Truncate(path, 0) != nil {
				t.Fatal("cannot empty the reader file")
			}
			if err := readerWatchdog(t, read, func() {}); err == nil || !strings.Contains(err.Error(), row.empty) {
				t.Fatalf("empty regular file changed its outcome: %v", err)
			}
		})
	}
}
