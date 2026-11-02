package localcell

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func launchdRenderFixture(t *testing.T) ServiceDescriptor {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, `cell's 日本 & < " space`)
	return ServiceDescriptor{
		Version: 1, InstallationID: "fixture-install_1", UID: "1001", Root: root,
		Adapter: "launchd-agent", Scope: "user", Runtime: "local",
		Payload: ServicePayload{
			Generation: "generation-1", Host: filepath.Join(home, `host's 日本 & < " space`),
			Args: []string{"run", "--root", root, `argument's & < > " 日本`, "", " padded "},
		},
	}
}

func TestServiceLaunchdPlistGolden(t *testing.T) {
	d := launchdRenderFixture(t)
	got, err := RenderLaunchdAgentPlist(d)
	if err != nil {
		t.Fatal(err)
	}
	// The literal template pins the complete key set and ordering. Path escaping
	// is independent of the encoding/xml writer used by the renderer.
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&#39;", `"`, "&#34;")
	want := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
<key>Label</key>
<string>ai.witwave.witself.cell.fixture-install_1</string>
<key>ProgramArguments</key>
<array>
<string>%s</string>
<string>run</string>
<string>--root</string>
<string>%s</string>
<string>argument&#39;s &amp; &lt; &gt; &#34; 日本</string>
<string></string>
<string> padded </string>
</array>
<key>RunAtLoad</key>
<true/>
<key>KeepAlive</key>
<false/>
<key>ProcessType</key>
<string>Standard</string>
<key>ExitTimeOut</key>
<integer>90</integer>
<key>EnvironmentVariables</key>
<dict>
<key>WITSELF_RUNTIME</key>
<string>local</string>
</dict>
<key>StandardErrorPath</key>
<string>%s</string>
<key>StandardOutPath</key>
<string>%s</string>
</dict>
</plist>
`, escape.Replace(d.Payload.Host), escape.Replace(d.Root),
		escape.Replace(filepath.Join(d.Root, "service", "host-stderr.log")),
		escape.Replace(filepath.Join(d.Root, "service", "host-stdout.log")))
	if !bytes.Equal(got, []byte(want)) {
		t.Fatal("rendered plist differs from the exact golden template")
	}
}

func TestServiceLaunchdPlistArgumentRoundTrip(t *testing.T) {
	d := launchdRenderFixture(t)
	data, err := RenderLaunchdAgentPlist(d)
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var got []string
	found := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal("rendered plist is not valid XML")
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			t.Fatal("cannot decode plist key")
		}
		if key != "ProgramArguments" {
			continue
		}
		if found {
			t.Fatal("duplicate ProgramArguments key")
		}
		found = true
		var array struct {
			XMLName xml.Name `xml:"array"`
			Strings []string `xml:"string"`
		}
		if err := decoder.Decode(&array); err != nil {
			t.Fatal("ProgramArguments is not a string array")
		}
		got = array.Strings
	}
	if !found || !slices.Equal(got, append([]string{d.Payload.Host}, d.Payload.Args...)) {
		t.Fatal("ProgramArguments failed byte-exact XML round trip")
	}
}

func TestServiceLaunchdPlistValidation(t *testing.T) {
	cases := []struct {
		name, step string
		mutate     func(*ServiceDescriptor)
	}{
		{"uid-empty", "spec:uid", func(d *ServiceDescriptor) { d.UID = "" }},
		{"uid-root", "spec:uid", func(d *ServiceDescriptor) { d.UID = "0" }},
		{"uid-negative", "spec:uid", func(d *ServiceDescriptor) { d.UID = "-1" }},
		{"uid-nonnumeric", "spec:uid", func(d *ServiceDescriptor) { d.UID = "owner" }},
		{"uid-leading-zero", "spec:uid", func(d *ServiceDescriptor) { d.UID = "01001" }},
		{"uid-overflow", "spec:uid", func(d *ServiceDescriptor) { d.UID = "4294967296" }},
		{"runtime-empty", "spec:runtime", func(d *ServiceDescriptor) { d.Runtime = "" }},
		{"runtime-hosted", "spec:runtime", func(d *ServiceDescriptor) { d.Runtime = "hosted" }},
		{"scope-system", "spec:scope", func(d *ServiceDescriptor) { d.Scope = "system" }},
		{"adapter-systemd", "spec:adapter", func(d *ServiceDescriptor) { d.Adapter = "systemd-user" }},
		{"adapter-foreground", "spec:adapter", func(d *ServiceDescriptor) { d.Adapter = "foreground" }},
		{"root-relative", "spec:root", func(d *ServiceDescriptor) { d.Root = "relative" }},
		{"root-unclean", "spec:root", func(d *ServiceDescriptor) { d.Root += string(filepath.Separator) + "." }},
		{"host-relative", "spec:host", func(d *ServiceDescriptor) { d.Payload.Host = "relative" }},
		{"host-unclean", "spec:host", func(d *ServiceDescriptor) { d.Payload.Host += string(filepath.Separator) + "." }},
		{"generation-empty", "spec:generation", func(d *ServiceDescriptor) { d.Payload.Generation = "" }},
		{"installation-empty", "spec:installation_id", func(d *ServiceDescriptor) { d.InstallationID = "" }},
		{"installation-separator", "spec:installation_id", func(d *ServiceDescriptor) { d.InstallationID = "bad/name" }},
		{"installation-dot", "spec:installation_id", func(d *ServiceDescriptor) { d.InstallationID = "bad.name" }},
		{"installation-unicode", "spec:installation_id", func(d *ServiceDescriptor) { d.InstallationID = "日本" }},
		{"installation-long", "spec:installation_id", func(d *ServiceDescriptor) { d.InstallationID = strings.Repeat("a", 129) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := launchdRenderFixture(t)
			tc.mutate(&d)
			data, err := RenderLaunchdAgentPlist(d)
			if !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), tc.step) || len(data) != 0 {
				t.Fatal("invalid descriptor was not refused with the expected sentinel and step")
			}
		})
	}
	t.Run("installation-maximum", func(t *testing.T) {
		d := launchdRenderFixture(t)
		d.InstallationID = strings.Repeat("a", 125) + "A_9"
		if _, err := RenderLaunchdAgentPlist(d); err != nil {
			t.Fatal("valid maximum-length installation ID refused")
		}
	})
}

func TestServiceLaunchdPlistRejectsControlAndInvalidXML(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"nul", "\x00"}, {"newline", "\n"}, {"carriage-return", "\r"},
		{"tab", "\t"}, {"delete", "\x7f"}, {"unicode-control", "\u0085"},
		{"invalid-utf8", "\xff"}, {"xml-noncharacter-fffe", "\ufffe"}, {"xml-noncharacter-ffff", "\uffff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, field := range []string{"argument", "root", "host"} {
				t.Run(field, func(t *testing.T) {
					d := launchdRenderFixture(t)
					switch field {
					case "argument":
						d.Payload.Args = []string{"before" + tc.value + "after"}
					case "root":
						d.Root += tc.value
					case "host":
						d.Payload.Host += tc.value
					}
					data, err := RenderLaunchdAgentPlist(d)
					if !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), "render:argument") || len(data) != 0 {
						t.Fatal("control or invalid XML value was not refused before rendering")
					}
				})
			}
		})
	}
}

func launchdPrintFixture(target string) string {
	// Minimal synthetic fixture for argument-byte and malformed-shape tests.
	// The separate observed fixture pins the owner's captured output; launchctl(1),
	// print/CAVEATS, still denies a stable output API.
	return target + " = {\n" +
		"\tpath = /fixture/owned.plist\n" +
		"\tprogram = /fixture/host's 日本 & < > \"\n" +
		"\targuments = {\n" +
		"\t\t/fixture/host's 日本 & < > \"\n" +
		"\t\trun\n" +
		"\t\t padded \n" +
		"\t\t\n" +
		"\t\tbrace { and } = value\n" +
		"\t}\n" +
		"\n\tstate = running\n" +
		"\tenvironment = {\n" +
		"\t\tpath = /foreign/nested.plist\n" +
		"\t\tprogram = foreign\n" +
		"\t\tnested = {\n\t\t\tfield = value\n\t\t}\n" +
		"\t}\n}\n"
}

func loadLaunchdObservedPrint(t *testing.T) string {
	t.Helper()
	// Verbatim supplied LC-S7C1-observed-print.txt; reports/LC-S7C1-owner-premise-evidence.md,
	// opening section, real-print-shape and parser-probe bullets. Substitute only
	// the capture's placeholders, preserving every tab and blank line.
	raw, err := os.ReadFile("testdata/launchd-print-observed.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer("<home>", "/fixture/home", "<uid>", "1001", "<asid>", "42", "<n>", "7").Replace(string(raw))
}

func TestServiceLaunchdPrintParserObserved(t *testing.T) {
	raw := loadLaunchdObservedPrint(t)
	got, err := parseLaunchdPrint([]byte(raw), "gui/1001/ai.openclaw.gateway")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/sh",
		"/fixture/home/.openclaw/service-env/ai.openclaw.gateway-env-wrapper.sh",
		"/fixture/home/.openclaw/service-env/ai.openclaw.gateway.env",
		"/opt/homebrew/opt/node/bin/node",
		"/opt/homebrew/lib/node_modules/openclaw/dist/index.js",
		"gateway", "--port", "18789",
	}
	if got.path != "/fixture/home/Library/LaunchAgents/ai.openclaw.gateway.plist" || got.program != "/bin/sh" || !slices.Equal(got.arguments, want) {
		t.Fatal("observed print did not preserve the exact path, program and eight arguments")
	}
}

func TestServiceLaunchdObservedPrintTruncationRefused(t *testing.T) {
	raw := loadLaunchdObservedPrint(t)
	const argumentsOpener = "\n\targuments = {\n"
	argumentStart := strings.Index(raw, argumentsOpener)
	argumentEnd := strings.Index(raw, "\n\t}\n")
	if argumentStart < 0 || argumentEnd < argumentStart+len(argumentsOpener) || !strings.HasSuffix(raw, "\n}\n") {
		t.Fatal("observed fixture no longer has the expected argument block and final close")
	}
	arguments := strings.Split(raw[argumentStart+len(argumentsOpener):argumentEnd], "\n")
	if len(arguments) != 8 {
		t.Fatal("observed truncation boundary is not after the eighth argument")
	}
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "\t\t") {
			t.Fatal("observed argument line no longer has the expected indentation")
		}
	}

	for _, tc := range []struct{ name, raw string }{
		{"eof_after_eighth_argument", raw[:argumentEnd]},
		{"eof_after_argument_block", raw[:argumentEnd+len("\n\t}")]},
		{"indented_final_close", strings.TrimSuffix(raw, "}\n") + "\t}\n"},
		{"missing_argument_block_opener", strings.Replace(raw, argumentsOpener, "\n", 1)},
		{"extra_final_close", raw + "}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLaunchdPrint([]byte(tc.raw), "gui/1001/ai.openclaw.gateway")
			if !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), "inspect:parse") {
				t.Fatal("incomplete or malformed observed print did not refuse with ownership conflict and parse step")
			}
			if got.path != "" || got.program != "" || len(got.arguments) != 0 {
				t.Fatal("failed observed parse exposed a partial ownership result")
			}
		})
	}
}

func TestServiceLaunchdPrintParserPreservesArgumentBytes(t *testing.T) {
	target := "gui/1001/ai.witwave.witself.cell.fixture-install_1"
	for _, ending := range []string{"newline", "no-newline"} {
		t.Run(ending, func(t *testing.T) {
			raw := launchdPrintFixture(target)
			if ending == "no-newline" {
				raw = strings.TrimSuffix(raw, "\n")
			}
			got, err := parseLaunchdPrint([]byte(raw), target)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{`/fixture/host's 日本 & < > "`, "run", " padded ", "", "brace { and } = value"}
			if got.path != "/fixture/owned.plist" || got.program != want[0] || !slices.Equal(got.arguments, want) {
				t.Fatal("print parser changed argument bytes or trusted nested ownership fields")
			}
		})
	}
}

func TestServiceLaunchdPrintParserNestedArrowsDoNotGrantOwnership(t *testing.T) {
	target := "gui/1001/ai.witwave.witself.cell.fixture-install_1"
	raw := strings.NewReplacer(
		"\t\tpath = /foreign/nested.plist", "\t\tpath => /foreign/nested.plist",
		"\t\tprogram = foreign", "\t\tprogram => foreign",
		"\t\tnested = {", "\t\targuments => {",
		"\t\t\tfield = value", "\t\t\t0 => /foreign/argument",
		"brace { and } = value", "brace { and } => value",
	).Replace(launchdPrintFixture(target))
	t.Run("nested-arrows-ignored-argument-bytes-preserved", func(t *testing.T) {
		got, err := parseLaunchdPrint([]byte(raw), target)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{`/fixture/host's 日本 & < > "`, "run", " padded ", "", "brace { and } => value"}
		if got.path != "/fixture/owned.plist" || got.program != want[0] || !slices.Equal(got.arguments, want) {
			t.Fatal("nested arrow fields changed ownership or argument bytes")
		}
	})
	argumentStart := strings.Index(raw, "\targuments = {\n")
	argumentEnd := argumentStart + strings.Index(raw[argumentStart:], "\t}\n") + len("\t}\n")
	for _, tc := range []struct{ name, remove string }{
		{"missing-top-level-path", "\tpath = /fixture/owned.plist\n"},
		{"missing-top-level-program", "\tprogram = /fixture/host's 日本 & < > \"\n"},
		{"missing-top-level-arguments", raw[argumentStart:argumentEnd]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLaunchdPrint([]byte(strings.Replace(raw, tc.remove, "", 1)), target)
			if !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), "inspect:parse") {
				t.Fatal("nested arrow field substituted for missing top-level ownership field")
			}
		})
	}
}

func TestServiceLaunchdPrintParserRejectsUnknownShape(t *testing.T) {
	target := "gui/1001/ai.witwave.witself.cell.fixture-install_1"
	valid := launchdPrintFixture(target)
	replace := func(old, replacement string) string { return strings.Replace(valid, old, replacement, 1) }
	cases := []struct{ name, raw string }{
		{"empty", ""},
		{"foreign-target", strings.Replace(valid, target, "gui/1002/foreign", 1)},
		{"missing-header", strings.TrimPrefix(valid, target+" = {\n")},
		{"header-spacing", replace(" = {\n", "={\n")},
		{"missing-footer", strings.TrimSuffix(valid, "}\n")},
		{"extra-footer", valid + "}\n"},
		{"trailing-output", valid + "unexpected\n"},
		{"missing-path", replace("\tpath = /fixture/owned.plist\n", "")},
		{"empty-path", replace("\tpath = /fixture/owned.plist", "\tpath = ")},
		{"missing-program", replace("\tprogram = /fixture/host's 日本 & < > \"\n", "")},
		{"missing-arguments", replace("\targuments = {", "\tother = {")},
		{"arguments-not-block", replace("\targuments = {", "\targuments = array")},
		{"empty-arguments", replace("\targuments = {", "\targuments = {\n\t}\n\tother = {")},
		{"duplicate-path", replace("\tpath = /fixture/owned.plist\n", "\tpath = /fixture/owned.plist\n\tpath = /foreign.plist\n")},
		{"duplicate-program", replace("\tstate = running", "\tprogram = /foreign/host")},
		{"duplicate-arguments", replace("\tstate = running", "\targuments = {\n\t\tforeign\n\t}")},
		{"duplicate-other-key", replace("\tstate = running", "\tstate = running\n\tstate = stopped")},
		{"top-level-space-indent", replace("\tpath = ", " path = ")},
		{"top-level-tab-indent", replace("\tpath = ", "\t\tpath = ")},
		{"top-level-path-arrow", replace("\tpath = ", "\tpath => ")},
		{"top-level-program-arrow", replace("\tprogram = ", "\tprogram => ")},
		{"top-level-arguments-arrow", replace("\targuments = ", "\targuments => ")},
		{"top-level-other-arrow", replace("\tstate = ", "\tstate => ")},
		{"unknown-line", replace("\tstate = running", "\tunsupported output")},
		{"empty-key", replace("\tstate = running", "\t = running")},
		{"key-trailing-space", replace("\tstate = running", "\tstate  = running")},
		{"argument-indent", replace("\t\trun\n", "\trun\n")},
		{"argument-control", replace("\t\trun\n", "\t\trun\tmore\n")},
		{"argument-unterminated", replace("\t}\n", "")},
		{"nested-invalid-line", replace("\t\tpath = /foreign/nested.plist", "\t\tnot a key-value line")},
		{"nested-unclosed", replace("\t\t}\n", "")},
		{"nested-wrong-close-indent", replace("\t\t}\n", "\t}\n")},
		{"carriage-return", replace("\tstate = running", "\tstate = running\r")},
		{"nul", replace("\tstate = running", "\tstate = run\x00ning")},
		{"invalid-utf8", replace("\tstate = running", "\tstate = \xff")},
		{"oversized", strings.Repeat("x", (1<<20)+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLaunchdPrint([]byte(tc.raw), target)
			if !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), "inspect:parse") {
				t.Fatal("unsupported print shape did not refuse with ownership conflict and parse step")
			}
			if got.path != "" || got.program != "" || len(got.arguments) != 0 {
				t.Fatal("failed parse exposed a partial ownership result")
			}
		})
	}
}

func TestServiceLaunchdPrintParserBoundsNesting(t *testing.T) {
	target := "gui/1001/ai.witwave.witself.cell.fixture-install_1"
	for _, levels := range []int{15, 16} {
		t.Run(fmt.Sprintf("nested-dictionaries-%d", levels), func(t *testing.T) {
			var raw strings.Builder
			raw.WriteString(target + " = {\n\tpath = /fixture/owned.plist\n\tprogram = /fixture/host\n\targuments = {\n\t\t/fixture/host\n\t}\n")
			for level := 1; level <= levels; level++ {
				raw.WriteString(strings.Repeat("\t", level) + "nested = {\n")
			}
			raw.WriteString(strings.Repeat("\t", levels+1) + "field = value\n")
			for level := levels; level > 0; level-- {
				raw.WriteString(strings.Repeat("\t", level) + "}\n")
			}
			raw.WriteString("}\n")
			_, err := parseLaunchdPrint([]byte(raw.String()), target)
			if levels == 15 && err != nil {
				t.Fatal("supported bounded nesting refused")
			}
			if levels == 16 && (!errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), "inspect:parse")) {
				t.Fatal("excessive nesting did not refuse with parse step")
			}
		})
	}
}
