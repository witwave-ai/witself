package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// Synthetic grammar fixtures model the owner's macOS 26.6.2 print-disabled
// observation and its reviewed legacy boolean spelling; they are not native CI
// acceptance evidence. Diagnostics deliberately never print these values.
func launchdDisabledOutput(entries ...string) []byte {
	return []byte("\n\tdisabled services = {\n" + strings.Join(entries, "") + "\t}\n")
}

func launchdDisabledEntry(label, state string) string {
	return "\t\t\"" + label + "\" => " + state + "\n"
}

func TestLaunchdDisabledParser(t *testing.T) {
	const target = "test.target"
	entry := func(label, state string) string { return launchdDisabledEntry(label, state) }
	type parserCase struct {
		name string
		raw  []byte
		want launchdDisabledState
		bad  bool
	}
	cases := []parserCase{
		{name: "absent", raw: launchdDisabledOutput(), want: launchdOverrideAbsent},
		{name: "false", raw: launchdDisabledOutput(entry(target, "false")), want: launchdOverrideEnabled},
		{name: "true", raw: launchdDisabledOutput(entry(target, "true")), want: launchdOverrideDisabled},
		{name: "enabled", raw: launchdDisabledOutput(entry(target, "enabled")), want: launchdOverrideEnabled},
		{name: "disabled", raw: launchdDisabledOutput(entry(target, "disabled")), want: launchdOverrideDisabled},
		{name: "sibling_disabled_target_absent", raw: launchdDisabledOutput(entry("test.sibling", "true")), want: launchdOverrideAbsent},
		{name: "sibling_disabled_target_false", raw: launchdDisabledOutput(entry(target, "false"), entry("test.sibling", "true")), want: launchdOverrideEnabled},
		{name: "prefix_collision", raw: launchdDisabledOutput(entry("prefix."+target, "true")), want: launchdOverrideAbsent},
		{name: "suffix_collision", raw: launchdDisabledOutput(entry(target+".suffix", "true")), want: launchdOverrideAbsent},
		{name: "other_domain", raw: launchdDisabledOutput(entry("gui/999/"+target, "true")), want: launchdOverrideAbsent},
		{name: "duplicate", raw: launchdDisabledOutput(entry(target, "false"), entry(target, "false")), bad: true},
		{name: "contradictory_duplicate", raw: launchdDisabledOutput(entry(target, "false"), entry(target, "true")), bad: true},
		{name: "duplicate_sibling", raw: launchdDisabledOutput(entry("test.sibling", "true"), entry("test.sibling", "true")), bad: true},
		{name: "non_boolean", raw: launchdDisabledOutput(entry(target, "1")), bad: true},
		{name: "unknown_token", raw: launchdDisabledOutput(entry(target, "unknown")), bad: true},
		{name: "quoted_token", raw: launchdDisabledOutput(entry(target, "\"true\"")), bad: true},
		{name: "token_suffix", raw: launchdDisabledOutput(entry(target, "true trailing")), bad: true},
		{name: "token_case", raw: launchdDisabledOutput(entry(target, "TRUE")), bad: true},
		{name: "invalid_text", raw: []byte("unrecognized manager response"), bad: true},
		{name: "empty", bad: true},
		{name: "truncated", raw: []byte("\n\tdisabled services = {\n" + entry(target, "true")), bad: true},
		{name: "trailing_material", raw: append(launchdDisabledOutput(), []byte("trailing")...), bad: true},
		{name: "extra_newline", raw: append(launchdDisabledOutput(), '\n'), bad: true},
		{name: "no_leading_blank", raw: launchdDisabledOutput()[1:], bad: true},
		{name: "crlf_refused", raw: bytes.ReplaceAll(launchdDisabledOutput(), []byte("\n"), []byte("\r\n")), bad: true},
		{name: "invalid_utf8", raw: launchdDisabledOutput(entry(string([]byte{0xff}), "true")), bad: true},
		{name: "nul", raw: launchdDisabledOutput(entry("bad\x00label", "true")), bad: true},
		{name: "label_control", raw: launchdDisabledOutput(entry("bad\tlabel", "true")), bad: true},
		{name: "empty_label", raw: launchdDisabledOutput(entry("", "true")), bad: true},
		{name: "quote_in_label", raw: launchdDisabledOutput(entry("bad\"label", "true")), bad: true},
		{name: "escaped_quote", raw: launchdDisabledOutput(entry("bad\\\"label", "true")), bad: true},
		{name: "backslash", raw: launchdDisabledOutput(entry("bad\\label", "true")), bad: true},
		{name: "brace_label", raw: launchdDisabledOutput(entry("bad{label", "true")), bad: true},
		{name: "unquoted_label", raw: launchdDisabledOutput("\t\t" + target + " => true\n"), bad: true},
		{name: "single_quoted_label", raw: launchdDisabledOutput("\t\t'" + target + "' => true\n"), bad: true},
		{name: "nested", raw: launchdDisabledOutput("\t\tnested = {\n" + entry(target, "true") + "\t\t}\n"), bad: true},
		{name: "wrong_indent", raw: launchdDisabledOutput("\t" + entry(target, "true")), bad: true},
		{name: "blank_entry", raw: launchdDisabledOutput("\n"), bad: true},
		{name: "unicode_label", raw: launchdDisabledOutput(entry("service.été", "false")), want: launchdOverrideAbsent},
		{name: "no_final_newline", raw: bytes.TrimSuffix(launchdDisabledOutput(), []byte("\n")), want: launchdOverrideAbsent},
	}
	base := launchdDisabledOutput(entry("", "true"))
	atBytes := launchdDisabledOutput(entry(strings.Repeat("x", launchdDisabledMaxBytes-len(base)), "true"))
	cases = append(cases,
		parserCase{name: "byte_bound_exact", raw: atBytes, want: launchdOverrideAbsent},
		parserCase{name: "byte_bound_exceeded", raw: launchdDisabledOutput(entry(strings.Repeat("x", launchdDisabledMaxBytes-len(base)+1), "true")), bad: true},
	)
	entries := make([]string, launchdDisabledMaxEntries+1)
	for i := range entries {
		entries[i] = entry(fmt.Sprintf("test.entry.%d", i), "false")
	}
	cases = append(cases,
		parserCase{name: "entry_bound_exact", raw: launchdDisabledOutput(entries[:launchdDisabledMaxEntries]...), want: launchdOverrideAbsent},
		parserCase{name: "entry_bound_exceeded", raw: launchdDisabledOutput(entries...), bad: true},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLaunchdDisabled(tc.raw, target)
			if tc.bad {
				if !errors.Is(err, ErrServiceRegistrationFailed) || !strings.Contains(err.Error(), "inspect_disabled:parse") || !strings.Contains(err.Error(), "unsupported_or_incomplete_print_disabled") {
					t.Fatal("invalid disabled response did not produce the fixed refusal")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatal("disabled parser did not preserve the exact target state")
			}
		})
	}
}

func launchdDisabledRequireSequence(t *testing.T, m *launchdFixtureManager, want [][]string) {
	t.Helper()
	var got [][]string
	for _, call := range m.allCalls {
		if len(call) < 2 {
			t.Fatal("manager call lacked a command")
		}
		if call[1] != "print" {
			got = append(got, call)
		}
	}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatal("disabled inspection and complete mutation sequence differed")
	}
}

func launchdDisabledRequireError(t *testing.T, err error, step string) {
	t.Helper()
	if !errors.Is(err, ErrServiceRegistrationFailed) || !strings.Contains(err.Error(), step) {
		t.Fatal("disabled inspection lost its fixed registration refusal")
	}
}

func launchdDisabledPlist(t *testing.T, a *launchdAgentAdapter, d ServiceDescriptor) (os.FileInfo, []byte) {
	t.Helper()
	info, err := os.Lstat(a.plistPath(d))
	if err != nil {
		t.Fatal("private plist identity unavailable")
	}
	data, err := os.ReadFile(a.plistPath(d))
	if err != nil {
		t.Fatal("private plist bytes unavailable")
	}
	return info, data
}

func launchdDisabledSeedPlist(t *testing.T, a *launchdAgentAdapter, d ServiceDescriptor) {
	t.Helper()
	if err := a.inspectDirectories(true); err != nil {
		t.Fatal("private fixture directories unavailable")
	}
	data, err := RenderLaunchdAgentPlist(d)
	if err != nil {
		t.Fatal("private fixture rendering failed")
	}
	if err := os.WriteFile(a.plistPath(d), data, 0o600); err != nil {
		t.Fatal("private fixture write failed")
	}
}

func launchdDisabledRequirePlist(t *testing.T, a *launchdAgentAdapter, d ServiceDescriptor, before os.FileInfo, data []byte) {
	t.Helper()
	after, got := launchdDisabledPlist(t, a, d)
	if !os.SameFile(before, after) || !bytes.Equal(data, got) {
		t.Fatal("private plist identity or bytes changed")
	}
}

func TestLaunchdApplyDisabledState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  launchdDisabledState
		change bool
	}{
		{name: "absent", state: launchdOverrideAbsent},
		{name: "false", state: launchdOverrideEnabled},
		{name: "true", state: launchdOverrideDisabled},
		{name: "plan_to_apply_disabled", state: launchdOverrideDisabled, change: true},
		{name: "plan_to_apply_enabled", state: launchdOverrideEnabled, change: true},
		{name: "plan_to_apply_absent", state: launchdOverrideAbsent, change: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			target := a.target(d)
			sibling := target + ".sibling"
			if tc.change {
				m.enabled[target] = tc.state == launchdOverrideDisabled
			}
			plan, err := a.Plan(t.Context(), d)
			if err != nil {
				t.Fatal("disabled state fixture plan failed")
			}
			delete(m.enabled, target)
			if tc.state != launchdOverrideAbsent {
				m.enabled[target] = tc.state == launchdOverrideEnabled
			}
			m.enabled[sibling] = tc.state == launchdOverrideDisabled
			m.enabled["gui/999/"+serviceLaunchdLabel(d)] = false
			wantOverrides := maps.Clone(m.enabled)
			want := [][]string{{a.launchctl, "print-disabled", a.domain()}}
			if tc.state == launchdOverrideDisabled {
				want = append(want, []string{a.launchctl, "enable", target})
				wantOverrides[target] = true
			}
			want = append(want, []string{a.launchctl, "bootstrap", a.domain(), a.plistPath(d)})
			m.allCalls = nil
			applyErr := a.Apply(t.Context(), plan)
			launchdDisabledRequireSequence(t, m, want)
			if !maps.Equal(m.enabled, wantOverrides) {
				t.Fatal("disabled apply changed an unauthorized override")
			}
			if applyErr != nil || !m.loaded {
				t.Fatal("disabled state apply failed to load the registration")
			}
			obs, err := a.Inspect(t.Context(), d)
			if err != nil || !obs.Owned || !obs.Registered {
				t.Fatal("disabled state apply did not finish owned and loaded")
			}
			m.allCalls = nil
			if err := a.Apply(t.Context(), plan); err != nil {
				t.Fatal("owned loaded reapply failed")
			}
			launchdDisabledRequireSequence(t, m, nil)
			if !maps.Equal(m.enabled, wantOverrides) {
				t.Fatal("owned loaded reapply changed overrides")
			}
		})
	}
}

func TestLaunchdDisabledInspectionFailure(t *testing.T) {
	for _, failure := range []string{"command", "cancellation", "malformed", "command_loaded", "malformed_loaded"} {
		for _, preexisting := range []bool{false, true} {
			name := "created"
			if preexisting {
				name = "preexisting"
			}
			t.Run(failure+"/"+name, func(t *testing.T) {
				a, d, m := launchdFixture(t)
				var before os.FileInfo
				var data []byte
				if preexisting {
					launchdDisabledSeedPlist(t, a, d)
					before, data = launchdDisabledPlist(t, a, d)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				private := strings.Repeat("q", 72)
				malformed := []byte("unrecognized disabled response " + private + " " + a.home)
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					raw, err := m.command(ctx, binary, args...)
					if args[0] != "print-disabled" {
						return raw, err
					}
					if !preexisting {
						before, data = launchdDisabledPlist(t, a, d)
					}
					if strings.HasSuffix(failure, "_loaded") {
						m.loaded = true
					}
					switch failure {
					case "command", "command_loaded":
						return nil, launchdFixtureError(t, 5, private+" "+a.home+" "+d.Root)
					case "cancellation":
						cancel()
						return nil, ctx.Err()
					default:
						return malformed, nil
					}
				}
				err := a.Apply(ctx, ServicePlan{Spec: d, RequiredPrivilege: "user"})
				launchdDisabledRequireError(t, err, "inspect_disabled")
				if failure == "cancellation" && !errors.Is(err, context.Canceled) {
					t.Fatal("disabled inspection cancellation sentinel was lost")
				}
				launchdDisabledRequireSequence(t, m, [][]string{{a.launchctl, "print-disabled", a.domain()}})
				if before == nil {
					t.Fatal("disabled read did not observe a private plist")
				}
				if preexisting || failure == "cancellation" || strings.HasSuffix(failure, "_loaded") {
					launchdDisabledRequirePlist(t, a, d, before, data)
				} else if _, statErr := os.Lstat(a.plistPath(d)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("failed disabled inspection retained its newly created plist")
				}
				for _, value := range []string{private, a.home, d.Root, string(malformed)} {
					if strings.Contains(err.Error(), value) {
						t.Fatal("disabled inspection diagnostic exposed fixture material")
					}
				}
			})
		}
	}
}

func TestLaunchdDisabledInspectionReplacement(t *testing.T) {
	for _, tc := range []struct {
		name             string
		disabled, loaded bool
	}{
		{name: "absent"},
		{name: "true", disabled: true},
		{name: "absent_loaded", loaded: true},
		{name: "true_loaded", disabled: true, loaded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			delete(m.enabled, a.target(d))
			if tc.disabled {
				m.enabled[a.target(d)] = false
			}
			var replacement os.FileInfo
			var data []byte
			a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
				raw, err := m.command(ctx, binary, args...)
				if args[0] == "print-disabled" {
					original, originalData := launchdDisabledPlist(t, a, d)
					if err := os.Rename(a.plistPath(d), a.plistPath(d)+".original"); err != nil {
						t.Fatal("disabled inspection replacement rename failed")
					}
					if err := os.WriteFile(a.plistPath(d), originalData, 0o600); err != nil {
						t.Fatal("disabled inspection replacement write failed")
					}
					replacement, data = launchdDisabledPlist(t, a, d)
					m.loaded = tc.loaded
					if os.SameFile(original, replacement) {
						t.Fatal("disabled inspection replacement retained original identity")
					}
				}
				return raw, err
			}
			err := a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
			launchdDisabledRequireSequence(t, m, [][]string{{a.launchctl, "print-disabled", a.domain()}})
			launchdDisabledRequireError(t, err, "post_disabled")
			if !errors.Is(err, ErrServiceOwnershipConflict) {
				t.Fatal("disabled inspection replacement lost ownership refusal")
			}
			if replacement == nil {
				t.Fatal("disabled inspection replacement callback did not run")
			}
			launchdDisabledRequirePlist(t, a, d, replacement, data)
		})
	}
}

func TestLaunchdDisabledEnableFailure(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "created"
		if preexisting {
			name = "preexisting"
		}
		t.Run(name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			m.enabled[a.target(d)] = false
			m.enabled[a.target(d)+".sibling"] = true
			beforeOverrides := maps.Clone(m.enabled)
			m.failEnable = true
			var before os.FileInfo
			var data []byte
			if preexisting {
				launchdDisabledSeedPlist(t, a, d)
				before, data = launchdDisabledPlist(t, a, d)
			}
			err := a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
			launchdDisabledRequireError(t, err, "enable")
			launchdDisabledRequireSequence(t, m, [][]string{{a.launchctl, "print-disabled", a.domain()}, {a.launchctl, "enable", a.target(d)}})
			if !maps.Equal(m.enabled, beforeOverrides) {
				t.Fatal("failed disabled enable changed an override")
			}
			if preexisting {
				launchdDisabledRequirePlist(t, a, d, before, data)
			} else if _, statErr := os.Lstat(a.plistPath(d)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("failed disabled enable retained its newly created plist")
			}
		})
	}
}
