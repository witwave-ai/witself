package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

func TestDataSubjectRunbookCommandsExist(t *testing.T) {
	const file = "../../docs/runbooks.md"
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	section := dsrRunbookSection(t, file, string(raw), "## Handle a data-subject request")
	var commands []string
	inShellFence := false
	for _, line := range strings.Split(section, "\n") {
		switch strings.TrimSpace(line) {
		case "```sh":
			inShellFence = true
		case "```":
			inShellFence = false
		default:
			if inShellFence && strings.HasPrefix(line, "witself ") {
				commands = append(commands, line)
			}
		}
	}
	expected := []string{
		"witself account status",
		"witself export --out FILE",
		"witself account events",
		"witself account change-email --new-email new@example.com",
		`witself account change-display-name "New Display Name"`,
		"witself fact delete --dry-run PREDICATE",
		"witself memory delete MEM_ID --dry-run",
		"witself account close --yes",
		"witself account recover",
	}
	if !slices.Equal(commands, expected) {
		t.Fatalf("%s: data-subject sh fence commands = %q, want %q", file, commands, expected)
	}
	// Pin complete table notes so valid flag names cannot conceal incorrect
	// instructions about how those flags are used. In particular, memory.go's
	// memoryDelete (lines 238-240) distinguishes preview concurrency fields from
	// the caller's fresh retry key.
	var tableNotes []string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Request |") || strings.HasPrefix(line, "| --- |") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 5 {
			t.Fatalf("%s: data-subject table row must contain three cells", file)
		}
		tableNotes = append(tableNotes, strings.TrimSpace(cells[3]))
	}
	expectedNotes := []string{
		"The export is the whole account and is verified before it is written. Sealed-plane values export only as ciphertext. `account events` is the owner's audit trail.",
		"`change-email` sends a code to the new address; run it again with `--code` to commit. Agents correct their own facts and memories with their usual write commands. Delete a wrong item as in the next row.",
		"Preview with `--dry-run`. `fact delete` applies with `--yes`. `memory delete` applies with `--yes --expected-version N --scrub-set-revision SHA256 --idempotency-key KEY`. Take `--expected-version` and `--scrub-set-revision` from the preview. Choose a fresh `--idempotency-key`; reuse it only to retry the same deletion. Deletion is permanent.",
		"See the timing below.",
	}
	if !slices.Equal(tableNotes, expectedNotes) {
		t.Errorf("%s: data-subject table notes = %q, want %q", file, tableNotes, expectedNotes)
	}
	normalized := strings.Join(strings.Fields(section), " ")
	for _, fragment := range []string{
		"--expected-version", "--scrub-set-revision", "--idempotency-key",
		"--code", "--account", "--realm", "--agent", "--token-file",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Errorf("%s: data-subject section is missing %q", file, fragment)
		}
	}
	// main.go:3165-3171 factConnectionFlags supplies both deletion commands'
	// account/realm/agent selectors; factDelete and memoryDelete use it before
	// parsing their own flags (main.go:3515 and memory.go:235).
	extraFlags := map[string][]string{
		"account change-email": {"code"},
		"fact delete":          {"yes", "account", "realm", "agent", "token-file"},
		"memory delete":        {"yes", "expected-version", "scrub-set-revision", "idempotency-key", "account", "realm", "agent", "token-file"},
		"account recover":      {"code"},
	}
	dsrRunbookCheckCommandHelp(t, file, commands, extraFlags)

	const securityFile = "../../docs/security-policy.md"
	securityRaw, err := os.ReadFile(securityFile)
	if err != nil {
		t.Fatal(err)
	}
	securitySection := dsrRunbookSection(t, securityFile, string(securityRaw), "## Account email is the root of trust")
	// main.go:792-818 dispatches operator list; operatorDelete registers --yes
	// and takes OPERATOR as a positional argument (main.go:903-915).
	// tokenRevoke registers --token and --yes (main.go:1149-1164). Each command
	// parses -h before connecting, so checking help cannot use a credential.
	compromiseCommands := []string{
		"witself operator list",
		"witself operator delete --yes OPERATOR",
		"witself token revoke --token TOKEN_ID --yes",
	}
	securityNormalized := strings.Join(strings.Fields(securitySection), " ")
	for _, command := range compromiseCommands {
		if !strings.Contains(securityNormalized, "`"+command+"`") {
			t.Errorf("%s: root-of-trust section is missing exact command %q", securityFile, command)
		}
	}
	if !strings.Contains(securitySection, "(runbooks.md#recover-a-lost-owner-token)") {
		t.Errorf("%s: root-of-trust section is missing the owner-token recovery runbook link", securityFile)
	}
	dsrRunbookCheckCommandHelp(t, securityFile, compromiseCommands, nil)
}

func dsrRunbookCheckCommandHelp(t *testing.T, file string, commands []string, extraFlags map[string][]string) {
	t.Helper()
	placeholder := regexp.MustCompile(`^[A-Z0-9_]+$`)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			words, err := dsrRunbookSplitWords(command)
			if err != nil {
				t.Fatalf("%s: split command %q: %v", file, command, err)
			}
			var path, flags []string
			for _, word := range words[1:] {
				if strings.HasPrefix(word.value, "-") || word.quoted || placeholder.MatchString(word.value) {
					break
				}
				path = append(path, word.value)
			}
			for _, word := range words[1:] {
				if strings.HasPrefix(word.value, "--") {
					name, _, _ := strings.Cut(strings.TrimPrefix(word.value, "--"), "=")
					flags = append(flags, name)
				}
			}
			commandPath := strings.Join(path, " ")
			flags = append(flags, extraFlags[commandPath]...)
			stdout, stderr, code := captureIntegrationsCLI(t, func() int {
				return run(append(append([]string{}, path...), "-h"))
			})
			out := stdout + stderr
			if code != 0 && code != 2 {
				t.Errorf("%s: command %q help exit = %d, want 0 or 2", file, commandPath, code)
			}
			for _, forbidden := range []string{"unknown command", "unknown subcommand"} {
				if strings.Contains(out, forbidden) {
					t.Errorf("%s: command %q help contains %q", file, commandPath, forbidden)
				}
			}
			heading := "Usage of " + commandPath + ":"
			hasUsage := strings.Contains(out, heading)
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "usage: witself "+commandPath) {
					hasUsage = true
				}
			}
			if !hasUsage {
				t.Errorf("%s: command %q help is missing its own usage heading %q", file, commandPath, heading)
			}
			for _, name := range flags {
				entry := regexp.MustCompile("(?m)^\\s+-" + regexp.QuoteMeta(name) + "(\\s|$)")
				if !entry.MatchString(out) {
					t.Errorf("%s: command %q help is missing whole flag entry %q", file, commandPath, "-"+name)
				}
			}
		})
	}
}

func dsrRunbookSection(t *testing.T, file, text, heading string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	for start, line := range lines {
		if line != heading {
			continue
		}
		end := start + 1
		for end < len(lines) && !strings.HasPrefix(lines[end], "## ") {
			end++
		}
		return strings.Join(lines[start:end], "\n")
	}
	t.Fatalf("%s: missing heading %q", file, heading)
	return ""
}

type dsrRunbookWord struct {
	value  string
	quoted bool
}

// The runbook uses double quotes for positional arguments containing spaces.
// Retain that boundary so a quoted argument cannot become part of the path.
func dsrRunbookSplitWords(line string) ([]dsrRunbookWord, error) {
	var words []dsrRunbookWord
	var word strings.Builder
	inQuote, quoted, started := false, false, false
	for _, char := range line {
		switch {
		case char == '"':
			inQuote = !inQuote
			quoted, started = true, true
		case unicode.IsSpace(char) && !inQuote:
			if started {
				words = append(words, dsrRunbookWord{value: word.String(), quoted: quoted})
				word.Reset()
				quoted, started = false, false
			}
		default:
			word.WriteRune(char)
			started = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated double quote")
	}
	if started {
		words = append(words, dsrRunbookWord{value: word.String(), quoted: quoted})
	}
	return words, nil
}
