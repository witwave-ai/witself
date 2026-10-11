package main

import (
	"os"
	"testing"
)

// Re-execute this test binary so the matrix exercises the production serve
// wiring, including conditional route registration and race-instrumented code.
func TestMain(m *testing.M) {
	if os.Getenv("WITSELF_TEST_TENANT_MATRIX_CHILD") == "1" {
		os.Exit(run([]string{"serve"}))
	}
	os.Exit(m.Run())
}
