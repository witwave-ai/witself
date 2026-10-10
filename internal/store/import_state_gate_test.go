package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestImportStateMemoryGateIsWired(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	check := regexp.MustCompile(`(?m)^check:[^\n]*\n((?:\t[^\n]*\n|\n)*)`).FindString(string(makefile))
	const peak = "\tWITSELF_TEST_REQUIRE_PEAK_MEMORY=1 go test ./internal/store -run '^TestImportPeakMemory$$' -count=1 -v -timeout=15m\n"
	const state = "\tWITSELF_TEST_REQUIRE_PEAK_MEMORY=1 go test ./internal/store -run '^(TestImportEntryIndexBytes|TestImportStateMemoryPostgres)$$' -count=1 -v -timeout=20m\n"
	before, after := strings.Index(check, peak), strings.Index(check, state)
	if before < 0 || after <= before {
		t.Fatal("Makefile check must run the required no-race import state tests after the peak test")
	}
	const peakStep = "      - name: test import peak memory (no race)\n"
	const stateStep = "      - name: test import state memory (no race)\n" +
		"        env:\n" +
		"          WITSELF_TEST_REQUIRE_PEAK_MEMORY: \"1\"\n" +
		"          WITSELF_TEST_REQUIRE_DATABASE: \"1\"\n" +
		"        run: go test ./internal/store -run '^(TestImportEntryIndexBytes|TestImportStateMemoryPostgres)$' -count=1 -v -timeout=20m\n"
	for _, name := range []string{"ci.yml", "release.yml"} {
		t.Run(name, func(t *testing.T) {
			workflow, err := os.ReadFile("../../.github/workflows/" + name)
			if err != nil {
				t.Fatal(err)
			}
			job := regexp.MustCompile(`(?m)^  go-store:\n((?:    [^\n]*\n|\n)*)`).FindString(string(workflow))
			before, after := strings.Index(job, peakStep), strings.Index(job, stateStep)
			if before < 0 || after <= before {
				t.Fatal("go-store must run the required no-race import state step after the peak step")
			}
		})
	}
}
