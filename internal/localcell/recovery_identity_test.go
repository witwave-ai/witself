package localcell_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestPostgresRecoveryRejectsReusedPIDIdentity(t *testing.T) {
	const pid, epoch = 4242, 1700000000
	for _, tc := range []struct {
		name   string
		second string
		accept bool
	}{
		{name: "IdentityChanges", second: "identity-B"},
		{name: "IdentityStable", second: "identity-A", accept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell, root := localcell.NewPostgresRecoveryCellForTest(t)
			manifestPath := filepath.Join(root, "manifest.json")
			manifestBefore, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal("cannot read recovery identity fixture manifest")
			}
			manifestInfoBefore, err := os.Stat(manifestPath)
			if err != nil {
				t.Fatal("cannot inspect recovery identity fixture manifest")
			}
			data := fmt.Sprintf("%d\n%s\n%d\n", pid, filepath.Join(root, "pg"), epoch)
			if err := lifecycleWriteFile(filepath.Join(root, "pg", "postmaster.pid"), []byte(data)); err != nil {
				t.Fatal("cannot stage recovery identity fixture postmaster")
			}
			var observations []string
			identityCalls := 0
			localcell.SetPostgresRecoveryObserversForTest(cell, func(observedPID int) (string, bool, error) {
				if observedPID != pid {
					t.Fatal("recovery identity observer received another PID")
				}
				observations = append(observations, "identity")
				identityCalls++
				if identityCalls == 1 {
					return "identity-A", true, nil
				}
				return tc.second, true, nil
			}, func(observedPID int) (time.Time, error) {
				if observedPID != pid {
					t.Fatal("recovery creation observer received another PID")
				}
				observations = append(observations, "creation")
				return time.Unix(epoch-5, 0), nil
			})
			err = localcell.RecoverPostgresRecordForTest(cell)
			if !reflect.DeepEqual(observations, []string{"identity", "creation", "identity"}) {
				t.Error("recovery identity observations did not bracket creation time")
			}
			if tc.accept {
				if err != nil {
					t.Error("recovery stable identity control was refused")
				}
				ownership, readErr := os.ReadFile(filepath.Join(root, "run", "postgres.json"))
				var record localcell.ProcessRecord
				if readErr != nil || json.Unmarshal(ownership, &record) != nil || record.PID != pid || record.StartTime != "identity-A" || record.Root != root || record.Component != "postgres" {
					t.Error("recovery stable identity control did not adopt its exact record")
				}
			} else {
				if err == nil || err.Error() != "cannot verify PostgreSQL recovery process" {
					t.Error("recovery identity change omitted the exact fixed refusal")
				}
				entries, readErr := os.ReadDir(filepath.Join(root, "run"))
				if readErr != nil || len(entries) != 0 {
					t.Error("recovery identity change published an ownership record")
				}
			}
			manifestAfter, readErr := os.ReadFile(manifestPath)
			manifestInfoAfter, statErr := os.Stat(manifestPath)
			if readErr != nil || statErr != nil || !bytes.Equal(manifestBefore, manifestAfter) || !os.SameFile(manifestInfoBefore, manifestInfoAfter) || !manifestInfoBefore.ModTime().Equal(manifestInfoAfter.ModTime()) {
				t.Error("recovery identity observation rewrote the installation manifest")
			}
		})
	}
}
