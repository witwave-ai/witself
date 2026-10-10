package export

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestReadPeakMemoryGateIsWired(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	check := regexp.MustCompile(`(?m)^check:[^\n]*\n((?:\t[^\n]*\n|\n)*)`).FindString(string(makefile))
	const importPeak = "\tWITSELF_TEST_REQUIRE_PEAK_MEMORY=1 go test ./internal/store -run '^TestImportPeakMemory$$' -count=1 -v -timeout=15m\n"
	const readPeak = "\tWITSELF_TEST_REQUIRE_PEAK_MEMORY=1 go test ./internal/export -run '^TestReadPeakMemory$$' -count=1 -v -timeout=15m\n"
	if !strings.Contains(check, importPeak+readPeak) {
		t.Fatal("Makefile check must run the required no-race reader peak test immediately after the import peak test")
	}
	const importStep = "      - name: test import peak memory (no race)\n" +
		"        env:\n" +
		"          WITSELF_TEST_REQUIRE_PEAK_MEMORY: \"1\"\n" +
		"        run: go test ./internal/store -run '^TestImportPeakMemory$' -count=1 -v -timeout=15m\n"
	const readStep = "      - name: test archive reader peak memory (no race)\n" +
		"        env:\n" +
		"          WITSELF_TEST_REQUIRE_PEAK_MEMORY: \"1\"\n" +
		"        run: go test ./internal/export -run '^TestReadPeakMemory$' -count=1 -v -timeout=15m\n"
	for _, name := range []string{"ci.yml", "release.yml"} {
		t.Run(name, func(t *testing.T) {
			workflow, err := os.ReadFile("../../.github/workflows/" + name)
			if err != nil {
				t.Fatal(err)
			}
			job := regexp.MustCompile(`(?m)^  go-store:\n((?:    [^\n]*\n|\n)*)`).FindString(string(workflow))
			if !strings.Contains(job, importStep+"\n"+readStep) {
				t.Fatal("go-store must run the exact required no-race reader peak step immediately after the import peak step")
			}
		})
	}
}
