package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestImportPeakMemoryGateIsWired(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	check := regexp.MustCompile(`(?m)^check:[^\n]*\n((?:\t[^\n]*\n|\n)*)`).FindString(string(makefile))
	const race = "\tgo test ./... -race -shuffle=on -timeout=$(STORE_TEST_TIMEOUT)\n"
	const peak = "\tWITSELF_TEST_REQUIRE_PEAK_MEMORY=1 go test ./internal/store -run '^TestImportPeakMemory$$' -count=1 -v -timeout=15m\n"
	if !strings.Contains(check, race+peak) {
		t.Fatal("Makefile check must run the required no-race peak test immediately after its race suite")
	}
	const storeStep = "      - name: test store\n"
	const peakStep = "      - name: test import peak memory (no race)\n" +
		"        env:\n" +
		"          WITSELF_TEST_REQUIRE_PEAK_MEMORY: \"1\"\n" +
		"        run: go test ./internal/store -run '^TestImportPeakMemory$' -count=1 -v -timeout=15m\n"
	for _, name := range []string{"ci.yml", "release.yml"} {
		t.Run(name, func(t *testing.T) {
			workflow, err := os.ReadFile("../../.github/workflows/" + name)
			if err != nil {
				t.Fatal(err)
			}
			job := regexp.MustCompile(`(?m)^  go-store:\n((?:    [^\n]*\n|\n)*)`).FindString(string(workflow))
			before, after := strings.Index(job, storeStep), strings.Index(job, peakStep)
			if before < 0 || after <= before {
				t.Fatal("go-store must run the required no-race peak step after test store")
			}
		})
	}
}
