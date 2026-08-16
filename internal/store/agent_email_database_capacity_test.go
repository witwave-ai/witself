package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestValidateAgentEmailCellStorageLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                          string
		admissionBytes, admissionRows int64
		hardBytes, hardRows           int64
		wantError                     string
	}{
		{name: "valid", admissionBytes: 3, admissionRows: 5, hardBytes: 4, hardRows: 10},
		{name: "equal bytes", admissionBytes: 4, admissionRows: 5, hardBytes: 4, hardRows: 10, wantError: "smaller than"},
		{name: "equal rows", admissionBytes: 3, admissionRows: 10, hardBytes: 4, hardRows: 10, wantError: "smaller than"},
		{name: "zero admission bytes", admissionRows: 1, hardBytes: 1, hardRows: 1, wantError: "admission bytes"},
		{name: "zero admission rows", admissionBytes: 1, hardBytes: 1, hardRows: 1, wantError: "admission rows"},
		{name: "zero hard bytes", admissionBytes: 1, admissionRows: 1, hardRows: 1, wantError: "hard bytes"},
		{name: "zero hard rows", admissionBytes: 1, admissionRows: 1, hardBytes: 1, wantError: "hard rows"},
		{name: "byte order", admissionBytes: 2, admissionRows: 1, hardBytes: 1, hardRows: 2, wantError: "smaller than"},
		{name: "row order", admissionBytes: 1, admissionRows: 2, hardBytes: 2, hardRows: 1, wantError: "smaller than"},
		{name: "representation bound", admissionBytes: maximumAgentEmailCellStorageLimit + 1, admissionRows: 1, hardBytes: maximumAgentEmailCellStorageLimit + 1, hardRows: 1, wantError: "between 1"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateAgentEmailCellStorageLimits(
				test.admissionBytes, test.admissionRows,
				test.hardBytes, test.hardRows,
			)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validation = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validation = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestIsAgentEmailDatabaseCapacityError(t *testing.T) {
	t.Parallel()
	if !IsAgentEmailDatabaseCapacityError(ErrAgentEmailDatabaseCapacity) ||
		!IsAgentEmailDatabaseCapacityError(fmt.Errorf(
			"wrapped: %w", ErrAgentEmailDatabaseCapacity,
		)) {
		t.Fatal("sentinel capacity error was not recognized")
	}
	databaseError := &pgconn.PgError{
		Code:    agentEmailCellStorageCapacitySQLState,
		Message: "value-free capacity refusal",
	}
	if !IsAgentEmailDatabaseCapacityError(databaseError) ||
		!IsAgentEmailDatabaseCapacityError(fmt.Errorf("wrapped: %w", databaseError)) {
		t.Fatal("schema capacity SQLSTATE was not recognized")
	}
	if IsAgentEmailDatabaseCapacityError(errors.New("other")) ||
		IsAgentEmailDatabaseCapacityError(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("unrelated error was recognized as capacity")
	}
}
