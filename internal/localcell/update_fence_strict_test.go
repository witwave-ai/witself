package localcell

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestUpdateFenceStrictDecoding(t *testing.T) {
	canonical := []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`)
	want := maintenanceFence{Generation: 1, OperationID: "fixture", Since: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	fixtures := []struct {
		name        string
		data        []byte
		shape       bool
		decode      bool
		reader      bool
		skipShape   bool
		generations []int
		want        maintenanceFence
	}{
		{"order_generation_operation_id_since", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), true, true, true, false, nil, want},
		{"order_generation_since_operation_id", []byte(`{"generation":1,"since":"2026-11-10T00:00:00Z","operation_id":"fixture"}`), true, true, true, false, nil, want},
		{"order_operation_id_generation_since", []byte(`{"operation_id":"fixture","generation":1,"since":"2026-11-10T00:00:00Z"}`), true, true, true, false, nil, want},
		{"order_operation_id_since_generation", []byte(`{"operation_id":"fixture","since":"2026-11-10T00:00:00Z","generation":1}`), true, true, true, false, nil, want},
		{"order_since_generation_operation_id", []byte(`{"since":"2026-11-10T00:00:00Z","generation":1,"operation_id":"fixture"}`), true, true, true, false, nil, want},
		{"order_since_operation_id_generation", []byte(`{"since":"2026-11-10T00:00:00Z","operation_id":"fixture","generation":1}`), true, true, true, false, nil, want},
		{"generation_escaped_canonical", []byte(`{"generati\u006fn":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), true, true, true, false, nil, want},
		{"operation_id_escaped_canonical", []byte(`{"generation":1,"operation_\u0069d":"fixture","since":"2026-11-10T00:00:00Z"}`), true, true, true, false, nil, want},
		{"since_escaped_canonical", []byte(`{"generation":1,"operation_id":"fixture","sinc\u0065":"2026-11-10T00:00:00Z"}`), true, true, true, false, nil, want},
		{"leading_whitespace", append([]byte{' ', '\t', '\r', '\n'}, canonical...), true, true, true, false, nil, want},
		{"trailing_whitespace", append(append([]byte{}, canonical...), ' ', '\t'), true, true, true, false, nil, want},
		{"trailing_newline", append(append([]byte{}, canonical...), '\n'), true, true, true, false, nil, want},
		{"trailing_CRLF", append(append([]byte{}, canonical...), '\r', '\n'), true, true, true, false, nil, want},
		{"between_token_whitespace", []byte(`{ "generation" : 1 , "operation_id" : "fixture" , "since" : "2026-11-10T00:00:00Z" }`), true, true, true, false, nil, want},
		{"exact_bound", append(append([]byte{}, canonical...), bytes.Repeat([]byte(` `), 4096-len(canonical))...), true, true, true, false, nil, want},
		{"generation_duplicate_equal", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","generation":1}`), false, false, false, false, nil, want},
		{"generation_duplicate_different", []byte(`{"generation":7,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","generation":9}`), false, false, false, false, []int{7, 9}, want},
		{"generation_duplicate_escaped", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","generati\u006fn":1}`), false, false, false, false, nil, want},
		{"generation_upper", []byte(`{"GENERATION":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"generation_title", []byte(`{"Generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"generation_mixed", []byte(`{"gEneration":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"generation_canonical_then_folded", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","Generation":1}`), false, false, false, false, nil, want},
		{"generation_folded_then_canonical", []byte(`{"Generation":1,"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"generation_null", []byte(`{"generation":null,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"generation_missing", []byte(`{"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_duplicate_equal", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","operation_id":"fixture"}`), false, false, false, false, nil, want},
		{"operation_id_duplicate_different", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","operation_id":"different"}`), false, false, false, false, nil, want},
		{"operation_id_duplicate_escaped", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","operation_\u0069d":"fixture"}`), false, false, false, false, nil, want},
		{"operation_id_upper", []byte(`{"generation":1,"OPERATION_ID":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_title", []byte(`{"generation":1,"Operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_mixed", []byte(`{"generation":1,"oPeration_ID":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_canonical_then_folded", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","Operation_id":"fixture"}`), false, false, false, false, nil, want},
		{"operation_id_folded_then_canonical", []byte(`{"Operation_id":"fixture","generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_null", []byte(`{"generation":1,"operation_id":null,"since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_missing", []byte(`{"generation":1,"since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_duplicate_equal", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_duplicate_different", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","since":"2026-11-11T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_duplicate_escaped", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","sinc\u0065":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_upper", []byte(`{"generation":1,"operation_id":"fixture","SINCE":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_title", []byte(`{"generation":1,"operation_id":"fixture","Since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_mixed", []byte(`{"generation":1,"operation_id":"fixture","sInce":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_canonical_then_folded", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","Since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_folded_then_canonical", []byte(`{"Since":"2026-11-10T00:00:00Z","generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_null", []byte(`{"generation":1,"operation_id":"fixture","since":null}`), false, false, false, false, nil, want},
		{"since_missing", []byte(`{"generation":1,"operation_id":"fixture"}`), false, false, false, false, nil, want},
		{"unknown_fourth_key", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","unknown":true}`), false, false, false, false, nil, want},
		{"unknown_replacement_key", []byte(`{"generation":1,"operation_id":"fixture","unknown":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"top_null", []byte(`null`), false, false, false, false, nil, want},
		{"top_array", []byte(`[]`), false, false, false, false, nil, want},
		{"top_string", []byte(`"fixture"`), false, false, false, false, nil, want},
		{"top_number", []byte(`1`), false, false, false, false, nil, want},
		{"top_true", []byte(`true`), false, false, false, false, nil, want},
		{"empty", []byte(``), false, false, false, false, nil, want},
		{"whitespace_only", []byte(`   `), false, false, false, false, nil, want},
		{"byte_order_mark", append([]byte{0xef, 0xbb, 0xbf}, canonical...), false, false, false, false, nil, want},
		{"nul_inside_operation_id", append(append([]byte(`{"generation":1,"operation_id":"fi`), byte(0)), []byte(`xture","since":"2026-11-10T00:00:00Z"}`)...), false, false, false, false, nil, want},
		{"nul_after_document", append(append([]byte{}, canonical...), byte(0)), false, false, false, false, nil, want},
		{"second_value", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"} {}`), false, false, false, false, nil, want},
		{"trailing_garbage", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"} x`), false, false, false, false, nil, want},
		{"truncated_before_closing_brace", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"`), false, false, false, false, nil, want},
		{"truncated_inside_string", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10`), false, false, false, false, nil, want},
		{"generation_object", []byte(`{"generation":{},"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"generation_array", []byte(`{"generation":[],"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_object", []byte(`{"generation":1,"operation_id":{},"since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"operation_id_array", []byte(`{"generation":1,"operation_id":[],"since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"since_object", []byte(`{"generation":1,"operation_id":"fixture","since":{}}`), false, false, false, false, nil, want},
		{"since_array", []byte(`{"generation":1,"operation_id":"fixture","since":[]}`), false, false, false, false, nil, want},
		// These scalar shapes leave type and time validation to the typed pass.
		{"typed_generation_string", []byte(`{"generation":"1","operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), true, false, false, false, nil, want},
		{"typed_generation_fraction", []byte(`{"generation":1.5,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), true, false, false, false, nil, want},
		{"typed_generation_boolean", []byte(`{"generation":true,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), true, false, false, false, nil, want},
		{"typed_operation_id_number", []byte(`{"generation":1,"operation_id":1,"since":"2026-11-10T00:00:00Z"}`), true, false, false, false, nil, want},
		{"typed_since_number", []byte(`{"generation":1,"operation_id":"fixture","since":1}`), true, false, false, false, nil, want},
		{"typed_since_not_RFC3339", []byte(`{"generation":1,"operation_id":"fixture","since":"not-a-time"}`), true, false, false, false, nil, want},
		// These decoded records leave semantic validity to valid().
		{"valid_generation_zero", []byte(`{"generation":0,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), true, true, false, false, nil, want},
		{"valid_generation_negative", []byte(`{"generation":-1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), true, true, false, false, nil, want},
		{"valid_operation_id_empty", []byte(`{"generation":1,"operation_id":"","since":"2026-11-10T00:00:00Z"}`), true, true, false, false, nil, want},
		{"valid_operation_id_unsafe", []byte(`{"generation":1,"operation_id":"fixture!","since":"2026-11-10T00:00:00Z"}`), true, true, false, false, nil, want},
		{"valid_operation_id_long", []byte(`{"generation":1,"operation_id":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz","since":"2026-11-10T00:00:00Z"}`), true, true, false, false, nil, want},
		{"valid_since_zero", []byte(`{"generation":1,"operation_id":"fixture","since":"0001-01-01T00:00:00Z"}`), true, true, false, false, nil, want},
		// Overflow is checked before shape and is never passed directly to the helper.
		{"one_over_bound", append(append([]byte{}, canonical...), bytes.Repeat([]byte(` `), 4097-len(canonical))...), false, false, false, true, nil, want},
		// Token rejects each malformed punctuation sequence as well as the typed pass.
		{"punctuation_doubled_comma", []byte(`{"generation":1,,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"punctuation_missing_colon", []byte(`{"generation" 1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z"}`), false, false, false, false, nil, want},
		{"punctuation_trailing_comma", []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z",}`), false, false, false, false, nil, want},
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures {
		if names[fixture.name] {
			t.Fatal("Duplicate strict fixture name")
		}
		names[fixture.name] = true
		for _, char := range fixture.name {
			switch {
			case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '_', char == '-':
			default:
				t.Fatal("Invalid strict fixture name")
			}
		}
		t.Run(fixture.name, func(t *testing.T) {
			checkError := func(err error, accepted bool, shape bool) {
				t.Helper()
				if accepted {
					if err != nil {
						if shape {
							t.Error("Shape level refused an accepted fixture")
						} else {
							t.Error("Decode level refused an accepted fixture")
						}
					}
					return
				}
				if err == nil {
					if shape {
						t.Error("Shape level accepted a refused fixture")
					} else {
						t.Error("Decode level accepted a refused fixture")
					}
					return
				}
				switch err.Error() {
				case "invalid local cell record", "invalid trailing local cell record", "local cell record exceeds size limit":
				default:
					if shape {
						t.Error("Shape level returned an unexpected error text")
					} else {
						t.Error("Decode level returned an unexpected error text")
					}
				}
			}
			if !fixture.skipShape {
				checkError(checkMaintenanceFenceShape(fixture.data), fixture.shape, true)
			}
			var decoded maintenanceFence
			checkError(decodeMaintenanceFence(bytes.NewReader(fixture.data), &decoded), fixture.decode, false)
			c, _ := unitCell(t)
			if testWriteFile(c.path("journal", "maintenance.json"), fixture.data, 0o600) != nil {
				t.Fatal("Cannot prepare strict fixture fence")
			}
			// The root snapshot includes the leaf bytes and every journal entry.
			before := serviceMaintenanceSnapshot(t, c)
			got, err := c.readMaintenanceFence()
			if fixture.reader {
				if err != nil || got == nil || *got != fixture.want {
					t.Error("Reader level did not return the exact expected fields")
				}
				if c.maintenanceFenceAllows(0) != errMaintenanceFenceActive {
					t.Error("Reader level admitted the ordinary generation")
				}
				if c.maintenanceFenceAllows(fixture.want.Generation) != nil {
					t.Error("Reader level refused the matching generation")
				}
				if c.maintenanceFenceAllows(fixture.want.Generation+1) != errMaintenanceFenceActive {
					t.Error("Reader level admitted a different generation")
				}
			} else {
				if got != nil || err != errMaintenanceFenceActive {
					t.Error("Reader level did not return the exact maintenance refusal")
				}
				generations := append([]int{0, 1, 2}, fixture.generations...)
				for _, generation := range generations {
					if c.maintenanceFenceAllows(generation) != errMaintenanceFenceActive {
						t.Error("Reader level admitted a refused fixture generation")
					}
				}
			}
			serviceMaintenanceUnchanged(t, c, before)
		})
	}
}

func TestUpdateFenceStrictAbsentParity(t *testing.T) {
	for _, name := range []string{"absent_parent", "absent_leaf"} {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			if name == "absent_parent" {
				if os.Remove(c.path("journal")) != nil {
					t.Fatal("Cannot prepare absent journal fixture")
				}
			}
			before := serviceMaintenanceSnapshot(t, c)
			called := false
			got, err := c.readMaintenanceFenceWith(func(string, any) error {
				called = true
				return errors.New("Fixture reader must remain unused")
			})
			if got != nil || err != nil || called {
				t.Error("Absent fence reached the reader or changed its result")
			}
			if c.maintenanceFenceAllows(0) != nil {
				t.Error("Absent fence refused the ordinary generation")
			}
			if c.maintenanceFenceAllows(1) != errMaintenanceFenceActive {
				t.Error("Absent fence admitted a positive generation")
			}
			serviceMaintenanceUnchanged(t, c, before)
		})
	}
}

func TestUpdateFenceStrictFoundationGuard(t *testing.T) {
	if len(updateFoundationViolations(admissionReadSources(t))) != 0 {
		t.Fatal("Foundation guard rejected the unchanged source control")
	}
	for _, helper := range []string{"checkMaintenanceFenceShape"} {
		t.Run(helper, func(t *testing.T) {
			sources := admissionReadSources(t)
			sources["fence_strict_mutant.go"] = "package localcell\nfunc fenceStrictMutant() { _ = " + helper + " }\n"
			want := "fence_strict_mutant.go: fenceStrictMutant references update helper " + helper
			if got := updateFoundationViolations(sources); !reflect.DeepEqual(got, []string{want}) {
				t.Fatal("Foundation guard did not identify the injected helper reference")
			}
		})
	}
}
