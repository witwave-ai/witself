package syncmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTypedIdentityWireAndEntropy(t *testing.T) {
	op := OperationID{1, 2, 3}
	encoded := op.String()
	if parsed, err := ParseOperationID(encoded); err != nil || parsed != op {
		t.Fatal("operation ID round trip failed")
	}
	for _, bad := range []string{"", encoded + "=", "revision_" + strings.TrimPrefix(encoded, "op_"), "op_abc", strings.ToUpper(encoded), "op_AAAAAAAAAAAAAAAAAAAAAB"} {
		if _, err := ParseOperationID(bad); err == nil {
			t.Fatal("noncanonical identity accepted")
		}
	}
	for _, wire := range []string{"null", "12", "[]", `"op_bad"`} {
		var id OperationID
		if json.Unmarshal([]byte(wire), &id) == nil {
			t.Fatal("invalid identity JSON accepted")
		}
	}
	constructors := []func() ([]byte, error){
		func() ([]byte, error) { v, e := NewOperationID(); return v[:], e }, func() ([]byte, error) { v, e := NewTransferID(); return v[:], e }, func() ([]byte, error) { v, e := NewRestoreMarker(); return v[:], e }, func() ([]byte, error) { v, e := NewRevisionID(); return v[:], e }, func() ([]byte, error) { v, e := NewGenerationID(); return v[:], e },
	}
	for _, newID := range constructors {
		first, err := newID()
		if err != nil || len(first) != 16 {
			t.Fatal("random identity creation failed")
		}
		second, err := newID()
		if err != nil || bytes.Equal(first, second) {
			t.Fatal("random identity constructor reused bytes")
		}
	}
	for _, v := range []any{TransferID{4}, RestoreMarker{5}, RevisionID{6}, GenerationID{7}} {
		data, err := json.Marshal(v)
		if err != nil || len(data) < 24 {
			t.Fatal("typed identity encoding failed")
		}
		switch v.(type) {
		case TransferID:
			var x TransferID
			err = json.Unmarshal(data, &x)
		case RestoreMarker:
			var x RestoreMarker
			err = json.Unmarshal(data, &x)
		case RevisionID:
			var x RevisionID
			err = json.Unmarshal(data, &x)
		case GenerationID:
			var x GenerationID
			err = json.Unmarshal(data, &x)
		}
		if err != nil {
			t.Fatal("typed identity decoding failed")
		}
	}
}
func TestCounterBoundsAndExactWire(t *testing.T) {
	for _, wire := range []string{`0`, `null`, `""`, `"+1"`, `"01"`, `"-1"`, `"1.0"`, `"1e2"`, `"9223372036854775808"`} {
		var n Ordinal
		if json.Unmarshal([]byte(wire), &n) == nil {
			t.Fatal("invalid exact counter accepted")
		}
	}
	for _, n := range []Ordinal{0, 1, 9007199254740993, Ordinal(MaxCounter)} {
		wire, err := json.Marshal(n)
		var next Ordinal
		if err != nil || json.Unmarshal(wire, &next) != nil || next != n {
			t.Fatal("counter precision lost")
		}
	}
	for _, n := range []uint64{MaxLifecycleEpoch - 1, MaxLifecycleEpoch, MaxLifecycleEpoch + 1} {
		err := LifecycleEpoch(n).Validate()
		if (n <= MaxLifecycleEpoch) != (err == nil) {
			t.Fatal("lifecycle bound mismatch")
		}
	}
	if CaptureEpoch(0).Validate() == nil || GraphEpoch(0).Validate() == nil || GraphRevision(0).Validate() != nil || Ordinal(0).Validate() != nil {
		t.Fatal("counter zero policy mismatch")
	}
	for _, n := range []uint64{MaxCounter - 1, MaxCounter, MaxCounter + 1} {
		for _, v := range []interface{ Validate() error }{CaptureEpoch(n), GraphEpoch(n), GraphRevision(n), Ordinal(n)} {
			if (n <= MaxCounter) != (v.Validate() == nil) {
				t.Fatal("counter storage bound mismatch")
			}
		}
	}
}
func TestCommitmentFormattingIsValueFree(t *testing.T) {
	value := Commitment{1, 2, 3}
	for _, format := range []string{"%s", "%v", "%#v", "%x", "%X", "%q"} {
		if fmt.Sprintf(format, value) != "[commitment redacted]" {
			t.Fatal("commitment leaked in formatting")
		}
	}
	wire, err := json.Marshal(value)
	var next Commitment
	if err != nil || json.Unmarshal(wire, &next) != nil || next != value {
		t.Fatal("explicit commitment wire round trip failed")
	}
	if _, ok := any(value).(fmt.Stringer); ok {
		t.Fatal("commitment has a value-bearing string interface")
	}
}
