package featurestatus

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Later runbook slices append their public document here so every shared guard
// applies to each published procedure.
var runbookDocsFiles = []string{
	"runbook-professional-to-team.md",
	"runbook-control-plane-rollback.md",
	"runbook-serving-cell-lost.md",
}

// runbookDocsVerified records, for each listed runbook, the main commit (its
// first 12 hex digits) and the date its facts were checked against; the
// not-rehearsed banner must name both.
var runbookDocsVerified = map[string][2]string{
	"runbook-professional-to-team.md":   {"1209a8c0ecf9", "2026-10-18"},
	"runbook-control-plane-rollback.md": {"8f5545dc6548", "2026-10-19"},
	"runbook-serving-cell-lost.md":      {"bc2d5a5bb990", "2026-10-21"},
}

func TestRunbookDocsBanner(t *testing.T) {
	runbookDocsRequireFiles(t)
	commitPattern := regexp.MustCompile(`^[0-9a-f]{12}$`)
	datePattern := regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	listed := make(map[string]bool, len(runbookDocsFiles))
	for _, file := range runbookDocsFiles {
		listed[file] = true
		t.Run(file, func(t *testing.T) {
			verified, ok := runbookDocsVerified[file]
			if !ok {
				t.Fatal("missing runbook verification entry")
			}
			if !commitPattern.MatchString(verified[0]) {
				t.Fatal("runbook verification commit must be exactly 12 lowercase hex digits")
			}
			if !datePattern.MatchString(verified[1]) {
				t.Fatal("runbook verification date must be YYYY-MM-DD")
			}
			want := "Status: not rehearsed. No step below has been run as a drill or timed end to end. Verified against `main` at `" + verified[0] + "` on " + verified[1] + "; re-check every command against the current tree before relying on it."
			lines := strings.Split(runbookDocsRead(t, "docs", file), "\n")
			if len(lines) < 3 || lines[2] != want {
				t.Error("paragraph immediately after the H1 must equal the required not-rehearsed banner")
			}
		})
	}
	for file := range runbookDocsVerified {
		if !listed[file] {
			t.Errorf("runbook verification entry %s is not listed in runbookDocsFiles", file)
		}
	}
}

func TestRunbookDocsListComplete(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "docs"))
	if err != nil {
		t.Fatal("docs:1 runbook-read-dir")
	}
	listed := make(map[string]bool, len(runbookDocsFiles))
	for _, file := range runbookDocsFiles {
		listed[file] = true
	}
	matched := make(map[string]bool)
	for _, entry := range entries {
		match, err := filepath.Match("runbook-*.md", entry.Name())
		if err != nil {
			t.Fatal("docs:1 runbook-match")
		}
		if entry.IsDir() || !match {
			continue
		}
		matched[entry.Name()] = true
		if !listed[entry.Name()] {
			t.Errorf("docs/%s:1 runbook-list-complete", entry.Name())
		}
	}
	for _, file := range runbookDocsFiles {
		if !matched[file] {
			t.Errorf("docs/%s:1 runbook-list-complete", file)
		}
	}
}

func TestRunbookDocsStructure(t *testing.T) {
	runbookDocsRequireFiles(t)
	reference := regexp.MustCompile(`\bG([0-9]+)\b`)
	definition := regexp.MustCompile(`^\s*(?:[-*]|[0-9]+[.)])\s+\*\*(?:\[GAP\] )?G([0-9]+)\.?\*\*`)
	unbracketed := regexp.MustCompile(`FOUNDER CHECKPOINT|GAP`)
	for _, file := range runbookDocsFiles {
		body := runbookDocsRead(t, "docs", file)
		lines := runbookDocsUnfencedLines(body)
		h1Count, h1Line := 0, 1
		lastH2 := runbookDocsLine{number: 1}
		definitions := make(map[string]bool)
		inGaps := false
		for _, line := range lines {
			if strings.HasPrefix(line.text, "# ") {
				h1Count++
				h1Line = line.number
			}
			if strings.HasPrefix(line.text, "## ") {
				lastH2 = line
				inGaps = line.text == "## Gaps"
			}
			if match := definition.FindStringSubmatch(line.text); inGaps && match != nil {
				id := match[1]
				if definitions[id] {
					t.Errorf("docs/%s:%d gap-definition-duplicate", file, line.number)
				} else if id != strconv.Itoa(len(definitions)+1) {
					t.Errorf("docs/%s:%d gap-definition-sequence", file, line.number)
				}
				definitions[id] = true
			}
		}
		if h1Count != 1 {
			t.Errorf("docs/%s:%d single-h1", file, h1Line)
		}
		if lastH2.text != "## Gaps" {
			t.Errorf("docs/%s:%d last-h2-gaps", file, lastH2.number)
		}
		for _, line := range lines {
			for _, match := range reference.FindAllStringSubmatch(line.text, -1) {
				if !definitions[match[1]] {
					t.Errorf("docs/%s:%d gap-reference-defined", file, line.number)
				}
			}
		}
		masked, unnumbered := runbookDocsMaskMarkers(lines)
		for _, line := range unnumbered {
			t.Errorf("docs/%s:%d gap-marker-numbered", file, line)
		}
		for _, line := range masked {
			if unbracketed.MatchString(line.text) {
				t.Errorf("docs/%s:%d bracketed-marker", file, line.number)
			}
		}
		for _, line := range runbookDocsJoinedMarkerResidue(body, masked, unbracketed) {
			t.Errorf("docs/%s:%d bracketed-marker", file, line)
		}
	}
}

// Mask only exact markers, retaining byte lengths for paragraph source offsets.
// A gap number may follow closing bold markup or wrap onto the next prose line.
func runbookDocsMaskMarkers(lines []runbookDocsLine) ([]runbookDocsLine, []int) {
	marker := regexp.MustCompile(`\[FOUNDER CHECKPOINT\]|\[GAP\]`)
	number := regexp.MustCompile(`^(?:\*\*)?[ \t\r]+G[0-9]+\b`)
	wrap := regexp.MustCompile(`^(?:\*\*)?[ \t\r]*$`)
	wrappedNumber := regexp.MustCompile(`^[ \t\r]*G[0-9]+\b`)
	masked := make([]runbookDocsLine, len(lines))
	var unnumbered []int
	definitions := 0
	for i, line := range lines {
		text := []byte(line.text)
		for _, match := range marker.FindAllStringIndex(line.text, -1) {
			if line.text[match[0]:match[1]] == "[GAP]" {
				tail := line.text[match[1]:]
				switch {
				case strings.HasPrefix(tail, "`"):
					// The literal inline-code token is exempt from numbering.
				case match[0] == 0 && strings.HasPrefix(tail, " Gn:") && definitions == 0:
					// Only the one legend line, which starts with "[GAP] Gn:", may use the placeholder.
					definitions++
					match[1] += len(" Gn")
				case number.MatchString(tail):
					match[1] += number.FindStringIndex(tail)[1]
				case wrap.MatchString(tail) && i+1 < len(lines) && lines[i+1].number == line.number+1 && wrappedNumber.MatchString(lines[i+1].text):
					// Contiguity prevents looking through a fence or blank line.
				default:
					unnumbered = append(unnumbered, line.number)
					continue
				}
			}
			for offset := match[0]; offset < match[1]; offset++ {
				text[offset] = ' '
			}
		}
		masked[i] = runbookDocsLine{number: line.number, text: string(text)}
	}
	return masked, unnumbered
}

// runbookDocsJoinedMarkerResidue returns the source line of every marker residue
// left in a joined unfenced paragraph, so a marker wrapped across lines is caught.
func runbookDocsJoinedMarkerResidue(body string, masked []runbookDocsLine, unbracketed *regexp.Regexp) []int {
	maskedText := make(map[int]string, len(masked))
	for _, line := range masked {
		maskedText[line.number] = line.text
	}
	var residue []int
	for _, paragraph := range runbookDocsParagraphs(body) {
		if _, unfenced := maskedText[paragraph[0].number]; !unfenced {
			continue
		}
		for i := range paragraph {
			paragraph[i].text = maskedText[paragraph[i].number]
		}
		joined, sourceLines := runbookDocsJoinParagraph(paragraph)
		// A bracketed token that hard-wraps at its inner space is still exact once joined;
		// blank it at the same length so offsets keep mapping to source lines.
		const founder = "[FOUNDER CHECKPOINT]"
		joined = strings.ReplaceAll(joined, founder, strings.Repeat(" ", len(founder)))
		for _, match := range unbracketed.FindAllStringIndex(joined, -1) {
			residue = append(residue, sourceLines[match[0]])
		}
	}
	return residue
}

func TestRunbookDocsJoinedMarkers(t *testing.T) {
	unbracketed := regexp.MustCompile(`FOUNDER CHECKPOINT|GAP`)
	for i, check := range []struct {
		body    string
		residue []int
	}{
		{"The founder signs at the **[FOUNDER\nCHECKPOINT]** below.", nil},
		{"This step is a FOUNDER\nCHECKPOINT: do it.", []int{1}},
		{"- item\n  a FOUNDER\n  CHECKPOINT", []int{2}},
		{"x [FOUNDER\nCHECKPOINT run it", []int{1}},
		{"```\nFOUNDER\nCHECKPOINT\n```", nil},
	} {
		masked, _ := runbookDocsMaskMarkers(runbookDocsUnfencedLines(check.body))
		got := runbookDocsJoinedMarkerResidue(check.body, masked, unbracketed)
		same := len(got) == len(check.residue)
		for j := 0; same && j < len(got); j++ {
			same = got[j] == check.residue[j]
		}
		if !same {
			t.Errorf("fixture %d joined marker residue differs", i)
		}
	}
}

func TestRunbookDocsMarkers(t *testing.T) {
	for i, check := range []struct {
		body       string
		unnumbered int
		residue    bool
	}{
		{"**[FOUNDER CHECKPOINT]** Proceed.", 0, false},
		{"**[GAP] G1**", 0, false},
		{"**[GAP]** G1", 0, false},
		{"[GAP] Gn: definition.", 0, false},
		{"Use `[GAP]` in the legend.", 0, false},
		{"**[GAP]\nG1**", 0, false},
		{"**[GAP]**\r\n  G1", 0, false},
		{"**[GAP]** No funnel.", 1, true},
		{"[GAP] G1x", 1, true},
		{"[GAP] Gnonsense", 1, true},
		{"[GAP]\n\nG1", 1, true},
		{"[GAP]\n```\nexample\n```\nG1", 1, true},
		{"[GAP] Gn: definition with GAP", 0, true},
		{"Text **[GAP] Gn** here.", 1, true},
		{"[GAP] Gn: one.\n\n[GAP] Gn: two.", 1, true},
		{"  [GAP] Gn: indented.", 1, true},
		{"**[GAP: no rule]**", 0, true},
		{"**[GAP G3**", 0, true},
		{"[FOUNDER CHECKPOINT run it", 0, true},
		{"```\n[GAP]\nFOUNDER CHECKPOINT\n```", 0, false},
	} {
		lines := runbookDocsUnfencedLines(check.body)
		masked, unnumbered := runbookDocsMaskMarkers(lines)
		if len(unnumbered) != check.unnumbered || len(masked) != len(lines) {
			t.Fatalf("fixture %d marker count differs", i)
		}
		residue := false
		for j, line := range masked {
			if line.number != lines[j].number || len(line.text) != len(lines[j].text) {
				t.Errorf("fixture %d marker offsets differ", i)
			}
			residue = residue || strings.Contains(line.text, "GAP") || strings.Contains(line.text, "FOUNDER CHECKPOINT")
		}
		if residue != check.residue {
			t.Errorf("fixture %d marker residue differs", i)
		}
	}
}

func TestRunbookDocsPublicSafety(t *testing.T) {
	runbookDocsRequireFiles(t)
	patterns := []struct {
		name string
		re   string
	}{
		{"private-handoff-path", `\.witself/handoff`},
		{"private-backup-path", `\.witself/pre-migration-backups`},
		{"private-age-path", `\.witself/backup-age`},
		{"private-witself-path", `~/\.witself/`},
		{"private-temporary-path", `/private/tmp`},
		{"private-user-home-path", `/Users/`},
		{"private-home-environment-path", `\$\{HOME\}`},
		{"private-home-witself-path", `\$HOME/\.witself`},
		{"dispatcher-directory", `dispatcher-scripts`},
		{"private-pr-ledger", `PR-LEDGER`},
		{"private-readiness-document", `READINESS`},
		{"private-founder-decisions", `SCOTT-DECISIONS`},
		{"private-migration-draft", `migration-runbook-draft`},
		{"private-memory-document", `MEMORY\.md`},
		{"private-packet", `\bPACKET\b`},
		{"private-migration-document", `\bMIGRATION\b`},
		{"private-spec", `\bSPEC-[0-9]`},
		{"private-draft", `-DRAFT\b`},
		{"personal-name", `\bScott\b`},
		{"incident-memory", `(?i)\bincident memory\b`},
		{"private-decision-anchor", `\bDecisions:[0-9]`},
		{"vault-identifier", `\bsec_[a-z0-9]{12,}`},
		{"memory-identifier", `\bmem_[a-z0-9]{12,}`},
		{"ledger-reference", `(?i)\bledger:`},
		{"credential-posture", `(?i)\bleaked\b`},
		{"private-deploy-script", `cp-deploy\.sh`},
		{"private-control-plane-script", `\bcp-[0-9]+\.sh`},
		{"private-chain-script", `chain-[0-9]+\.sh`},
		{"private-drill-script", `drill-proof-v[0-9]`},
		{"private-detach-script", `detach\.sh`},
		{"private-scheduler-script", `cp-sched-watch\.sh`},
		{"private-serving-restore-script", `restore-use1-serving\.sh`},
		{"private-serving-register-script", `register-use1-serving\.sh`},
		{"ipv4-address", `\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`},
		{"source-line-anchor", `[A-Za-z0-9_./-]+\.(?:go|mjs|js|jsonc|json|ya?ml|sh|md|sql|toml|tf)(?::[0-9]+|#L[0-9]+)`},
		{"phx1-core-service-duration", `(?i)\b6 h 20\b`},
		{"phx1-full-service-duration", `(?i)about 21 h`},
		{"phx1-recovery-point-relative", `(?i)after 09-05`},
		{"phx1-recovery-point-date", `2026-09-05`},
		{"phx1-notice-length", `(?i)31 days of notice`},
		{"phx1-recovery-point-age", `(?i)\b14 days\b`},
	}
	matchers := make([]*regexp.Regexp, len(patterns))
	for i, pattern := range patterns {
		matchers[i] = regexp.MustCompile(pattern.re)
	}
	email := regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	for _, file := range runbookDocsFiles {
		body := runbookDocsRead(t, "docs", file)
		for i, line := range strings.Split(body, "\n") {
			for j, pattern := range patterns {
				if matchers[j].MatchString(line) {
					t.Errorf("docs/%s:%d %s", file, i+1, pattern.name)
				}
			}
			for _, address := range email.FindAllString(line, -1) {
				if !runbookDocsAllowedEmail(address) {
					t.Errorf("docs/%s:%d email-address", file, i+1)
				}
			}
		}
		for _, paragraph := range runbookDocsParagraphs(body) {
			joined, sourceLines := runbookDocsJoinParagraph(paragraph)
			for j, pattern := range patterns {
				for _, match := range matchers[j].FindAllStringIndex(joined, -1) {
					t.Errorf("docs/%s:%d %s", file, sourceLines[match[0]], pattern.name)
				}
			}
			for _, match := range email.FindAllStringIndex(joined, -1) {
				if !runbookDocsAllowedEmail(joined[match[0]:match[1]]) {
					t.Errorf("docs/%s:%d email-address", file, sourceLines[match[0]])
				}
			}
		}
	}
}

func TestRunbookDocsLinks(t *testing.T) {
	runbookDocsRequireFiles(t)
	root, err := filepath.EvalSymlinks(repoRoot(t))
	if err != nil {
		t.Fatal("repository root does not resolve")
	}
	for _, file := range runbookDocsFiles {
		for _, link := range runbookDocsLinks(runbookDocsRead(t, "docs", file)) {
			target, err := url.Parse(link.target)
			if err != nil {
				t.Errorf("docs/%s:%d: malformed link", file, link.line)
				continue
			}
			if target.Scheme == "http" || target.Scheme == "https" || target.Scheme == "mailto" {
				continue
			}
			path := target.Path
			if path == "" {
				path = file
			}
			resolved := filepath.Join(root, "docs", filepath.FromSlash(path))
			relative, err := filepath.Rel(root, resolved)
			if target.Scheme != "" || target.Host != "" || filepath.IsAbs(path) || err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Errorf("docs/%s:%d: link target is outside the repository", file, link.line)
				continue
			}
			resolved, err = filepath.EvalSymlinks(resolved)
			if err != nil {
				t.Errorf("docs/%s:%d: link target does not resolve", file, link.line)
				continue
			}
			relative, err = filepath.Rel(root, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Errorf("docs/%s:%d: link target is outside the repository", file, link.line)
				continue
			}
			raw, err := os.ReadFile(resolved)
			if err != nil {
				t.Errorf("docs/%s:%d: link target does not resolve", file, link.line)
				continue
			}
			if target.Fragment != "" && !runbookDocsHeadingSlugs(string(raw))[target.Fragment] {
				t.Errorf("docs/%s:%d: link fragment does not resolve", file, link.line)
			}
		}
	}
}

func TestRunbookDocsIndexes(t *testing.T) {
	runbookDocsRequireFiles(t)
	for _, index := range []string{"README.md", "runbooks.md"} {
		body := runbookDocsRead(t, "docs", index)
		if index == "runbooks.md" {
			lines := runbookDocsUnfencedLines(body)
			for _, line := range lines {
				if strings.HasPrefix(line.text, "## ") {
					body = strings.Join(strings.Split(body, "\n")[:line.number-1], "\n")
					break
				}
			}
		}
		links := runbookDocsLinks(body)
		for _, file := range runbookDocsFiles {
			found := false
			for _, link := range links {
				if link.target == file {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("docs/%s: missing runbook index link for %s", index, file)
			}
		}
	}
}

func TestRunbookDocsPreviewLabels(t *testing.T) {
	for _, check := range []struct {
		file string
		want string
	}{
		{"README.md", "- Grok Build preview:"},
		{"README.md", "- Cursor preview:"},
		{"docs/agent-memory-routing.md", "| Grok Build preview |"},
		{"docs/agent-memory-routing.md", "| Cursor preview |"},
		{"docs/memory-runtime-acceptance.md", "labeled preview"},
	} {
		if !strings.Contains(runbookDocsRead(t, check.file), check.want) {
			t.Errorf("%s: missing preview label %q", check.file, check.want)
		}
	}
}

func TestRunbookDocsProfessionalToTeamProcedure(t *testing.T) {
	const file = "runbook-professional-to-team.md"
	body := runbookDocsRead(t, "docs", file)
	for _, command := range []string{
		"witself-admin account plan-override set",
		"--plan team",
		"witself-admin account plan-override clear",
	} {
		if !strings.Contains(body, command) {
			t.Errorf("docs/%s: missing procedure command %q", file, command)
		}
	}
	numberedStep := regexp.MustCompile(`^\s*[0-9]+[.)]\s`)
	steps := map[string]int{"## Procedure": 0, "## Exit": 0}
	var section string
	for _, line := range runbookDocsUnfencedLines(body) {
		if strings.HasPrefix(line.text, "## ") {
			section = line.text
		}
		if _, procedure := steps[section]; procedure && numberedStep.MatchString(line.text) {
			steps[section]++
			if !strings.Contains(line.text, "[FOUNDER CHECKPOINT]") {
				t.Errorf("docs/%s:%d: numbered procedure step lacks founder checkpoint", file, line.number)
			}
		}
	}
	for section, count := range steps {
		if count == 0 {
			t.Errorf("docs/%s: %s lacks numbered procedure steps", file, section)
		}
	}
	for _, link := range runbookDocsLinks(runbookDocsRead(t, "docs", "billing-and-limits.md")) {
		if link.target == file {
			return
		}
	}
	t.Fatal("docs/billing-and-limits.md: missing Professional-to-Team procedure link")
}

func runbookDocsRequireFiles(t *testing.T) {
	t.Helper()
	if len(runbookDocsFiles) == 0 {
		t.Fatal("runbookDocsFiles must list at least one runbook")
	}
}

func runbookDocsRead(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func runbookDocsAllowedEmail(address string) bool {
	switch address {
	case "support@witwave.ai", "security@witwave.ai", "legal@witwave.ai":
		return true
	}
	_, domain, _ := strings.Cut(address, "@")
	switch domain {
	case "example.test", "example.com", "example.invalid":
		return true
	default:
		return false
	}
}

type runbookDocsLine struct {
	number int
	text   string
}

type runbookDocsFence struct {
	mark  byte
	width int
}

// Contains includes both fence delimiters and every line between them. After a
// closing delimiter, mark is zero so adjacent fenced blocks stay separate.
func (f *runbookDocsFence) contains(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
		mark := trimmed[0]
		n := 0
		for n < len(trimmed) && trimmed[n] == mark {
			n++
		}
		if n >= 3 && f.mark == 0 {
			f.mark, f.width = mark, n
			return true
		}
		if mark == f.mark && n >= f.width && strings.TrimSpace(trimmed[n:]) == "" {
			f.mark, f.width = 0, 0
			return true
		}
	}
	return f.mark != 0
}

func runbookDocsUnfencedLines(body string) []runbookDocsLine {
	var result []runbookDocsLine
	var fence runbookDocsFence
	for i, line := range strings.Split(body, "\n") {
		if !fence.contains(line) {
			result = append(result, runbookDocsLine{number: i + 1, text: line})
		}
	}
	return result
}

// Blank lines separate prose paragraphs; each fenced block is scanned on its
// own, including its delimiters, without joining it to surrounding prose.
func runbookDocsParagraphs(body string) [][]runbookDocsLine {
	var result [][]runbookDocsLine
	var paragraph []runbookDocsLine
	flush := func() {
		if len(paragraph) > 0 {
			result = append(result, paragraph)
			paragraph = nil
		}
	}
	var fence runbookDocsFence
	for i, text := range strings.Split(body, "\n") {
		wasFenced := fence.mark != 0
		fenced := fence.contains(text)
		if fenced && !wasFenced {
			flush()
		}
		if !fenced && strings.TrimSpace(text) == "" {
			flush()
			continue
		}
		paragraph = append(paragraph, runbookDocsLine{number: i + 1, text: text})
		if fenced && fence.mark == 0 {
			flush()
		}
	}
	flush()
	return result
}

// Keep a source line for every byte, because regexp match offsets are bytes.
// Replacement bytes inherit the first matched byte's source line.
func runbookDocsJoinParagraph(paragraph []runbookDocsLine) (string, []int) {
	var text strings.Builder
	var sourceLines []int
	for i, line := range paragraph {
		if i > 0 {
			text.WriteByte('\n')
			sourceLines = append(sourceLines, paragraph[i-1].number)
		}
		text.WriteString(line.text)
		for range len(line.text) {
			sourceLines = append(sourceLines, line.number)
		}
	}
	joined := text.String()
	for _, rewrite := range []struct {
		re          *regexp.Regexp
		replacement byte
	}{
		{regexp.MustCompile(`\b-[ \t\r]*\n[ \t\r]*`), '-'},
		{regexp.MustCompile(`\s+`), ' '},
	} {
		var normalized []byte
		var normalizedLines []int
		start := 0
		for _, match := range rewrite.re.FindAllStringIndex(joined, -1) {
			normalized = append(normalized, joined[start:match[0]]...)
			normalizedLines = append(normalizedLines, sourceLines[start:match[0]]...)
			normalized = append(normalized, rewrite.replacement)
			normalizedLines = append(normalizedLines, sourceLines[match[0]])
			start = match[1]
		}
		normalized = append(normalized, joined[start:]...)
		normalizedLines = append(normalizedLines, sourceLines[start:]...)
		joined, sourceLines = string(normalized), normalizedLines
	}
	return joined, sourceLines
}

func TestRunbookDocsParagraphs(t *testing.T) {
	body := "é\t prefix\n  continued\nPHX1 had 31 days of\nnotice and 2026- \n\t09-05\n\nabout\n\n21 h\n```text\n6 h\n20\n```\nafter\n~~~~\n31 days of\nnotice\n~~~~\n```\nseparate\n```\nlast\n\nrole address --\nlegal@witwave.ai\n\n2026-\r\n09-05\n\nPR-\nLEDGER\n\n```\nx-\n\ny\n```"
	paragraphs := runbookDocsParagraphs(body)
	want := []string{
		"é prefix continued PHX1 had 31 days of notice and 2026-09-05",
		"about", "21 h", "```text 6 h 20 ```", "after",
		"~~~~ 31 days of notice ~~~~", "``` separate ```", "last",
		"role address -- legal@witwave.ai", "2026-09-05", "PR-LEDGER",
		"``` x- y ```",
	}
	if len(paragraphs) != len(want) {
		t.Fatal("paragraph boundaries differ")
	}
	for i, paragraph := range paragraphs {
		joined, sourceLines := runbookDocsJoinParagraph(paragraph)
		if joined != want[i] || len(joined) != len(sourceLines) {
			t.Errorf("paragraph %d normalization differs", i)
		}
	}
	for _, check := range []struct {
		paragraph int
		match     string
		line      int
	}{
		{0, "continued", 2}, {0, "31 days of notice", 3},
		{0, "2026-09-05", 4}, {0, "09-05", 5},
		{3, "6 h 20", 11}, {5, "31 days of notice", 16},
		{6, "separate", 20}, {7, "last", 22},
		{8, "legal@witwave.ai", 25}, {9, "2026-09-05", 27},
		{9, "09-05", 28}, {10, "PR-LEDGER", 30}, {10, "LEDGER", 31},
	} {
		joined, sourceLines := runbookDocsJoinParagraph(paragraphs[check.paragraph])
		offset := strings.Index(joined, check.match)
		if offset < 0 || sourceLines[offset] != check.line {
			t.Errorf("paragraph %d source line differs", check.paragraph)
		}
	}
}

func runbookDocsHeadingSlugs(body string) map[string]bool {
	atx := regexp.MustCompile(`^ {0,3}#{1,6}[\t ]+(.+?)\s*#*\s*$`)
	setext := regexp.MustCompile(`^ {0,3}(?:=+|-+)[\t ]*$`)
	used := map[string]bool{}
	var paragraph []string
	previousLine := 0
	for _, line := range runbookDocsUnfencedLines(body) {
		if line.number != previousLine+1 {
			paragraph = nil
		}
		previousLine = line.number
		var heading string
		switch match := atx.FindStringSubmatch(line.text); {
		case match != nil:
			heading = match[1]
			paragraph = nil
		case setext.MatchString(line.text) && len(paragraph) > 0:
			heading = strings.Join(paragraph, " ")
			paragraph = nil
		case strings.TrimSpace(line.text) == "", strings.HasPrefix(line.text, "    "), setext.MatchString(line.text):
			paragraph = nil
			continue
		default:
			paragraph = append(paragraph, strings.TrimSpace(line.text))
			continue
		}
		var slug strings.Builder
		for _, r := range strings.ToLower(heading) {
			switch {
			case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
				slug.WriteRune(r)
			case r == ' ':
				slug.WriteByte('-')
			}
		}
		base := slug.String()
		candidate := base
		for suffix := 1; used[candidate]; suffix++ {
			candidate = base + "-" + strconv.Itoa(suffix)
		}
		used[candidate] = true
	}
	return used
}

type runbookDocsLink struct {
	line   int
	target string
}

// Parse inline and reference links, including angle-bracket destinations and
// balanced parentheses in paths. Code spans and fenced examples are not links.
func runbookDocsLinks(body string) []runbookDocsLink {
	// Keep every source newline, including those in omitted fences, definitions
	// and code spans, so a multiline link is reported at its opening bracket.
	visible := make([]string, len(strings.Split(body, "\n")))
	for _, line := range runbookDocsUnfencedLines(body) {
		visible[line.number-1] = line.text
	}
	text := strings.Join(visible, "\n")
	// A definition may have only a destination and optional quoted title.
	// In particular, the founder-marker explanation is prose, not a definition
	// pointing every checkpoint marker at a file named "the".
	definition := regexp.MustCompile(`(?m)^ {0,3}\[([^\]\[]+)\]:[\t ]*(?:\n[\t ]*)?(<[^>\n]+>|[^\s<>]+)(?:[\t ]+(?:"[^"\n]*"|'[^'\n]*'|\([^\)\n]*\)))?[\t ]*$`)
	codeSpan := regexp.MustCompile(`\x60+[^\x60]*\x60+`)
	text = codeSpan.ReplaceAllStringFunc(text, runbookDocsKeepNewlines)
	references := map[string]string{}
	for _, match := range definition.FindAllStringSubmatch(text, -1) {
		label := runbookDocsReferenceLabel(match[1])
		if _, exists := references[label]; !exists {
			references[label] = runbookDocsDestination(match[2])
		}
	}
	text = definition.ReplaceAllStringFunc(text, runbookDocsKeepNewlines)
	var links []runbookDocsLink
	for start := 0; start < len(text); start++ {
		if text[start] != '[' || (start > 0 && text[start-1] == '\\') {
			continue
		}
		end := runbookDocsClosingDelimiter(text, start, '[', ']')
		if end < 0 {
			continue
		}
		line := strings.Count(text[:start], "\n") + 1
		label := text[start+1 : end]
		next := end + 1
		var target string
		if next < len(text) && text[next] == '(' {
			closeAt := runbookDocsClosingDelimiter(text, next, '(', ')')
			if closeAt < 0 {
				continue
			}
			target = runbookDocsDestination(text[next+1 : closeAt])
			start = closeAt
		} else {
			// Full reference links may separate their two labels by a single
			// line break. A blank line begins another paragraph.
			referenceNext := next
			for referenceNext < len(text) && strings.ContainsRune(" \t\n", rune(text[referenceNext])) {
				referenceNext++
			}
			if strings.Count(text[next:referenceNext], "\n") <= 1 && referenceNext < len(text) && text[referenceNext] == '[' {
				closeAt := runbookDocsClosingDelimiter(text, referenceNext, '[', ']')
				if closeAt < 0 {
					continue
				}
				referenceLabel := label
				if closeAt > referenceNext+1 {
					referenceLabel = text[referenceNext+1 : closeAt]
				}
				if _, exists := references[runbookDocsReferenceLabel(referenceLabel)]; exists {
					label = referenceLabel
					start = closeAt
				}
			}
			target = references[runbookDocsReferenceLabel(label)]
		}
		if target != "" {
			links = append(links, runbookDocsLink{line: line, target: target})
		}
	}
	return links
}

func runbookDocsKeepNewlines(text string) string {
	return strings.Repeat("\n", strings.Count(text, "\n"))
}

func runbookDocsClosingDelimiter(text string, start int, open, closeByte byte) int {
	depth := 0
	for i := start; i < len(text); i++ {
		if text[i] == '\\' {
			i++
			continue
		}
		switch text[i] {
		case open:
			depth++
		case closeByte:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func runbookDocsDestination(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<") {
		if end := strings.IndexByte(text, '>'); end >= 0 {
			return text[1:end]
		}
	}
	if fields := strings.Fields(text); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func runbookDocsReferenceLabel(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func TestRunbookDocsMarkdownParsing(t *testing.T) {
	body := strings.Join([]string{
		"# Same!", "# Same!", "# Same-1", "# Keep_under score É",
		"~~~~markdown", "# Ignored", "[x](missing.md)", "~~~", "# Still ignored", "~~~~",
		`[inline](file(1).md#heading) and [angle](<other.md> "Title")`,
		"[Full][ref] [ref][] [ref] `[code](missing.md)`", "[ref]: target.md", "",
		"Setext heading", "========", "", "Second line", "continued heading", "--------", "",
		"[multi", "line](", "file.md#multi", `"Title"`, ")",
		"[Reference", "across lines][", "multi", "reference]", "",
		"[multi", "reference]:", "<multiref.md>", "",
		"[Separated]", "[ref]", "",
		"`[code", "span](missing.md)`", "[after](after.md)", "",
		"~~~~", "Fenced Setext", "======", "~~~~", "",
		"Setext heading", "-------", "",
		"`code starts", "[ghost]: missing.md", "code ends`", "[ghost]", "",
		"[ref][undefined]", "",
		"[FOUNDER CHECKPOINT]: the founder decides; he runs the step himself or says go first.",
		"**[FOUNDER CHECKPOINT]** Perform the step.", "",
	}, "\n")
	slugs := runbookDocsHeadingSlugs(body)
	for _, slug := range []string{
		"same", "same-1", "same-1-1", "keep_under-score-é", "setext-heading",
		"second-line-continued-heading", "setext-heading-1",
	} {
		if !slugs[slug] {
			t.Errorf("missing heading slug %q", slug)
		}
	}
	if len(slugs) != 7 {
		t.Errorf("got %d heading slugs; fenced headings must be ignored", len(slugs))
	}
	links := runbookDocsLinks(body)
	want := []runbookDocsLink{
		{line: 11, target: "file(1).md#heading"},
		{line: 11, target: "other.md"},
		{line: 12, target: "target.md"},
		{line: 12, target: "target.md"},
		{line: 12, target: "target.md"},
		{line: 22, target: "file.md#multi"},
		{line: 27, target: "multiref.md"},
		{line: 36, target: "target.md"},
		{line: 41, target: "after.md"},
		{line: 56, target: "target.md"},
	}
	if len(links) != len(want) {
		t.Fatalf("got %d links, want %d", len(links), len(want))
	}
	for i := range want {
		if links[i] != want[i] {
			t.Errorf("link %d differs: got %+v, want %+v", i, links[i], want[i])
		}
	}
}
