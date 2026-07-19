package sealed

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestCanonicalValueAADExactOrderAndDeterminism(t *testing.T) {
	binding := testValueBinding()
	first, err := CanonicalValueAAD(binding)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalValueAAD(binding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical value AAD is not deterministic")
	}

	want := append([]byte(nil), []byte(FieldValueDomain)...)
	for _, value := range []string{
		binding.AccountID, binding.RealmID, binding.OwnerAgentID,
		binding.SecretID, binding.FieldID, binding.DEKID,
	} {
		want = testAppendString(want, value)
	}
	want = binary.BigEndian.AppendUint64(want, binding.ValueVersion)
	want = binary.BigEndian.AppendUint64(want, binding.DEKGeneration)
	want = testAppendString(want, binding.ValueEncoding)
	want = testAppendString(want, binding.AEADAlgorithm)
	if !bytes.Equal(first, want) {
		t.Fatalf("canonical value AAD bytes changed")
	}
}

func TestCanonicalDEKWrapAADExactOrderAndSeparateDomain(t *testing.T) {
	binding := testWrapBinding()
	got, err := CanonicalDEKWrapAAD(binding)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), []byte(dekWrapDomain)...)
	for _, value := range []string{
		binding.AccountID, binding.RealmID, binding.OwnerAgentID,
		binding.SecretID, binding.FieldID, binding.DEKID,
	} {
		want = testAppendString(want, value)
	}
	want = binary.BigEndian.AppendUint64(want, binding.DEKGeneration)
	want = testAppendString(want, string(binding.Domain))
	want = testAppendString(want, binding.WrappingKeyID)
	want = binary.BigEndian.AppendUint64(want, binding.WrappingKeyVersion)
	want = binary.BigEndian.AppendUint64(want, binding.WrapRevision)
	want = testAppendString(want, binding.WrapAlgorithm)
	if !bytes.Equal(got, want) {
		t.Fatal("canonical DEK-wrap AAD bytes changed")
	}
	valueAAD, err := CanonicalValueAAD(testValueBinding())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, valueAAD) || bytes.HasPrefix(got, []byte(FieldValueDomain)) {
		t.Fatal("value and DEK-wrap domains are not separated")
	}
}

func TestCanonicalAADRejectsInvalidComponents(t *testing.T) {
	valueCases := []ValueAADBinding{
		func() ValueAADBinding { b := testValueBinding(); b.Domain = "other"; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.AccountID = "acc_1"; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.RealmID = "realm_AAAAAAAAAAAAAAAA"; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.OwnerAgentID = ""; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.SecretID = "sec_aaaaaaaaaaaaaaa1"; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.FieldID = "fld_aaaaaaaaaaaaaaa_"; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.DEKID = "avk_aaaaaaaaaaaaaaaa"; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.ValueVersion = 0; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.DEKGeneration = 0; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.ValueEncoding = "base64"; return b }(),
		func() ValueAADBinding { b := testValueBinding(); b.AEADAlgorithm = "other"; return b }(),
	}
	for i, binding := range valueCases {
		if aad, err := CanonicalValueAAD(binding); aad != nil || !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("value case %d = %x, %v; want ErrInvalidBinding", i, aad, err)
		}
	}

	wrapCases := []DEKWrapAADBinding{
		func() DEKWrapAADBinding { b := testWrapBinding(); b.Domain = "other"; return b }(),
		func() DEKWrapAADBinding { b := testWrapBinding(); b.WrappingKeyID = "avk_1"; return b }(),
		func() DEKWrapAADBinding { b := testWrapBinding(); b.WrappingKeyVersion = 0; return b }(),
		func() DEKWrapAADBinding { b := testWrapBinding(); b.WrapRevision = 0; return b }(),
		func() DEKWrapAADBinding { b := testWrapBinding(); b.WrapAlgorithm = "other"; return b }(),
	}
	for i, binding := range wrapCases {
		if aad, err := CanonicalDEKWrapAAD(binding); aad != nil || !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("wrap case %d = %x, %v; want ErrInvalidBinding", i, aad, err)
		}
	}
}

func testFieldScope() FieldScope {
	return FieldScope{
		Domain:       FieldValueDomain,
		AccountID:    "acc_aaaaaaaaaaaaaaaa",
		RealmID:      "realm_bbbbbbbbbbbbbbbb",
		OwnerAgentID: "agent_cccccccccccccccc",
		SecretID:     "sec_dddddddddddddddd",
		FieldID:      "fld_eeeeeeeeeeeeeeee",
	}
}

func testValueBinding() ValueAADBinding {
	return ValueAADBinding{
		FieldScope:    testFieldScope(),
		DEKID:         "dek_ffffffffffffffff",
		ValueVersion:  7,
		DEKGeneration: 3,
		ValueEncoding: ValueEncodingUTF8,
		AEADAlgorithm: AES256GCMAlgorithm,
	}
}

func testWrapBinding() DEKWrapAADBinding {
	return DEKWrapAADBinding{
		FieldScope:         testFieldScope(),
		DEKID:              "dek_ffffffffffffffff",
		DEKGeneration:      3,
		WrappingKeyID:      "avk_gggggggggggggggg",
		WrappingKeyVersion: 2,
		WrapRevision:       4,
		WrapAlgorithm:      AES256GCMAlgorithm,
	}
}

func testAppendString(dst []byte, value string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(value)))
	return append(dst, value...)
}
