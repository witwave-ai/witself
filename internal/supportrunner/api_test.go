package supportrunner

import (
	"testing"
)

func TestValidateTicketListCompleteness(t *testing.T) {
	tests := []struct {
		name            string
		cellsPresent    bool
		ticketsPresent  bool
		aggregateCapped bool
		cellStatuses    []string
		wantError       bool
	}{
		{name: "complete", cellsPresent: true, ticketsPresent: true, cellStatuses: []string{"ok", "ok"}},
		{name: "empty fleet", cellsPresent: true, ticketsPresent: true},
		{name: "missing cells", ticketsPresent: true, wantError: true},
		{name: "missing tickets", cellsPresent: true, wantError: true},
		{name: "aggregate cap", cellsPresent: true, ticketsPresent: true, aggregateCapped: true, wantError: true},
		{name: "cell error", cellsPresent: true, ticketsPresent: true, cellStatuses: []string{"error"}, wantError: true},
		{name: "cell timeout", cellsPresent: true, ticketsPresent: true, cellStatuses: []string{"timeout"}, wantError: true},
		{name: "unknown cell status", cellsPresent: true, ticketsPresent: true, cellStatuses: []string{"partial"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTicketListCompleteness(
				test.cellsPresent,
				test.ticketsPresent,
				test.aggregateCapped,
				test.cellStatuses,
			)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %t", err, test.wantError)
			}
		})
	}
}
