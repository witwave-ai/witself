package localcell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrivateTreeRecoveryAlias(t *testing.T) {
	for _, row := range []string{"alias-only", "both", "file-alias"} {
		t.Run(row, func(t *testing.T) {
			root := t.TempDir()
			boundary := filepath.Join(root, "recovery")
			alias := filepath.Join(root, "Recovery")
			if row == "both" {
				privateTreeFixture(t, root, "recovery/file")
				if _, err := os.Lstat(alias); err == nil {
					t.Skip("case-insensitive temporary filesystem cannot hold both spellings")
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("cannot check temporary filesystem case sensitivity")
				}
			}
			switch row {
			case "file-alias":
				alias = filepath.Join(root, "RECOVERY")
				privateTreeFixture(t, root, "RECOVERY")
			case "both":
				privateTreeFixture(t, root, "Recovery/file")
			default:
				privateTreeFixture(t, root, "Recovery/dir/file")
			}
			var callbacks, validated []string
			run := func(start string) error {
				callbacks, validated = nil, nil
				return validatePrivateTreeSnapshot(start, func(start string, visit fs.WalkDirFunc) error {
					return filepath.WalkDir(start, func(path string, entry fs.DirEntry, err error) error {
						callbacks = append(callbacks, path)
						return visit(path, entry, err)
					})
				}, func(path string, directory bool) error {
					return privateTreeEntry(root, path, directory, func() error {
						validated = append(validated, path)
						return nil
					}, func(string) error {
						t.Fatal("alias walk unexpectedly selected the PGDATA validator")
						return nil
					})
				}, os.Lstat)
			}
			if row == "both" {
				// WalkDir sorts the uppercase alias first and stops there. With
				// both objects present, separately drive the exact boundary through
				// the same snapshot and entry decision to prove its one-object skip.
				if run(boundary) != nil || !reflect.DeepEqual(validated, []string{boundary}) ||
					!reflect.DeepEqual(callbacks, []string{boundary}) {
					t.Fatal("exact boundary was not validated once and skipped")
				}
			}
			err := run(root)
			for _, path := range callbacks {
				if strings.HasPrefix(path, alias+string(filepath.Separator)) || strings.HasPrefix(path, boundary+string(filepath.Separator)) {
					t.Fatal("walk callback entered below a recovery spelling")
				}
			}
			if !reflect.DeepEqual(validated, []string{root}) {
				t.Fatal("alias walk called a validator for the refused alias")
			}
			if err == nil || err.Error() != "cannot inspect private local cell layout" || errors.Is(err, fs.SkipDir) {
				t.Fatal("alias walk did not return the fixed refusal")
			}
			if !reflect.DeepEqual(callbacks, []string{root, alias}) {
				t.Fatal("alias walk did not stop at the alias entry")
			}
		})
	}
}
