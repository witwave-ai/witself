package trust

import (
	"encoding/binary"
	"testing"
)

// Hand-built self-relative descriptors exercise the production byte parser on
// every host, independently of Windows ACL inheritance normalization.
func recordDescriptorTestBytes() (descriptor, user, system []byte) {
	user = []byte{1, 1, 0, 0, 0, 0, 0, 5, 21, 0, 0, 0}
	system = []byte{1, 1, 0, 0, 0, 0, 0, 5, 18, 0, 0, 0}
	descriptor = make([]byte, 80)
	descriptor[0] = 1
	binary.LittleEndian.PutUint16(descriptor[2:4], privateRecordSelfRelative|privateRecordDACLProtected|privateRecordDACLPresent)
	binary.LittleEndian.PutUint32(descriptor[4:8], 20)
	binary.LittleEndian.PutUint32(descriptor[16:20], 32)
	copy(descriptor[20:32], user)
	descriptor[32] = 2
	binary.LittleEndian.PutUint16(descriptor[34:36], 48)
	binary.LittleEndian.PutUint16(descriptor[36:38], 2)
	for i, sid := range [][]byte{user, system} {
		pos := 40 + i*20
		binary.LittleEndian.PutUint16(descriptor[pos+2:pos+4], 20)
		binary.LittleEndian.PutUint32(descriptor[pos+4:pos+8], privateRecordFullAccess)
		copy(descriptor[pos+8:pos+20], sid)
	}
	return descriptor, user, system
}

func TestTrustPrivateRecordDescriptor(t *testing.T) {
	cases := []struct {
		name   string
		change func([]byte, []byte, []byte) ([]byte, []byte, []byte)
		valid  bool
	}{
		{"owner-and-system", nil, true},
		{"reversed-trustees", func(b, u, s []byte) ([]byte, []byte, []byte) {
			copy(b[48:60], s)
			copy(b[68:80], u)
			return b, u, s
		}, true},
		{"user-is-system", func(b, _, s []byte) ([]byte, []byte, []byte) {
			copy(b[20:32], s)
			copy(b[48:60], s)
			binary.LittleEndian.PutUint16(b[34:36], 28)
			binary.LittleEndian.PutUint16(b[36:38], 1)
			return b[:60], s, s
		}, true},
		{"unprotected-dacl", func(b, u, s []byte) ([]byte, []byte, []byte) {
			binary.LittleEndian.PutUint16(b[2:4], privateRecordSelfRelative|privateRecordDACLPresent)
			return b, u, s
		}, false},
		{"absent-dacl", func(b, u, s []byte) ([]byte, []byte, []byte) {
			binary.LittleEndian.PutUint16(b[2:4], privateRecordSelfRelative|privateRecordDACLProtected)
			return b, u, s
		}, false},
		{"wrong-owner", func(b, u, s []byte) ([]byte, []byte, []byte) {
			copy(b[20:32], s)
			return b, u, s
		}, false},
		{"extra-trustee", func(b, u, s []byte) ([]byte, []byte, []byte) {
			b = append(b, b[40:60]...)
			b[96]++
			binary.LittleEndian.PutUint16(b[34:36], 68)
			binary.LittleEndian.PutUint16(b[36:38], 3)
			return b, u, s
		}, false},
		{"unknown-trustee", func(b, u, s []byte) ([]byte, []byte, []byte) {
			b[76]++
			return b, u, s
		}, false},
		{"duplicate-trustee", func(b, u, s []byte) ([]byte, []byte, []byte) {
			copy(b[68:80], u)
			return b, u, s
		}, false},
		{"invalid-rights", func(b, u, s []byte) ([]byte, []byte, []byte) {
			binary.LittleEndian.PutUint32(b[44:48], privateRecordFullAccess-1)
			return b, u, s
		}, false},
		{"inherited-ace", func(b, u, s []byte) ([]byte, []byte, []byte) {
			b[41] = 0x10 // INHERITED_ACE, even with SE_DACL_PROTECTED.
			return b, u, s
		}, false},
		{"inherit-only-ace", func(b, u, s []byte) ([]byte, []byte, []byte) {
			b[41] = 0x08 // INHERIT_ONLY_ACE.
			return b, u, s
		}, false},
		{"unknown-ace", func(b, u, s []byte) ([]byte, []byte, []byte) {
			b[40] = 0x7f
			return b, u, s
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, user, system := recordDescriptorTestBytes()
			if tc.change != nil {
				b, user, system = tc.change(b, user, system)
			}
			p, err := privateRecordPolicyFromSecurity(b, user, system)
			if err != nil {
				t.Fatal("well-formed descriptor projection refused")
			}
			if tc.name == "inherited-ace" && (len(p.aces) != 2 || p.aces[0].flags != 0x10) {
				t.Fatal("inherited ACE flag was not preserved")
			}
			err = checkPrivateRecordPolicy(p)
			if tc.valid {
				if err != nil {
					t.Fatal("valid descriptor policy refused")
				}
			} else if err != ReasonStateIO {
				t.Fatal("unsafe descriptor must receive fixed privacy refusal")
			}
		})
	}
}

func TestTrustPrivateRecordDescriptorMalformed(t *testing.T) {
	cases := []struct {
		name   string
		change func([]byte) []byte
	}{
		{"short-header", func(b []byte) []byte { return b[:19] }},
		{"revision", func(b []byte) []byte { b[0] = 2; return b }},
		{"absolute-descriptor", func(b []byte) []byte { b[3] &^= 0x80; return b }},
		{"missing-owner", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:8], 0); return b }},
		{"owner-in-header", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:8], 4); return b }},
		{"owner-unaligned", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:8], 21); return b }},
		{"owner-out-of-bounds", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:8], ^uint32(0)); return b }},
		{"sid-revision", func(b []byte) []byte { b[20] = 2; return b }},
		{"sid-count", func(b []byte) []byte { b[21] = 16; return b }},
		{"null-dacl", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[16:20], 0); return b }},
		{"acl-out-of-bounds", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[16:20], 80); return b }},
		{"acl-revision", func(b []byte) []byte { b[32] = 1; return b }},
		{"acl-short", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[34:36], 4); return b }},
		{"acl-overrun", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[34:36], 52); return b }},
		{"owner-acl-overlap", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:8], 48); return b }},
		{"unexpected-sacl", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:16], 32); return b }},
		{"group-in-header", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[8:12], 4); return b }},
		{"group-out-of-bounds", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[8:12], 80); return b }},
		{"excessive-aces", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[36:38], 1025); return b }},
		{"ace-count-overrun", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[36:38], 3); return b }},
		{"ace-short", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[42:44], 4); return b }},
		{"ace-unaligned", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[42:44], 19); return b }},
		{"ace-overrun", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[42:44], 44); return b }},
		{"ace-sid-truncated", func(b []byte) []byte { b[49] = 2; return b }},
		{"ace-sid-trailing-data", func(b []byte) []byte { b[49] = 0; return b }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, user, system := recordDescriptorTestBytes()
			if _, err := privateRecordPolicyFromSecurity(tc.change(b), user, system); err != ReasonStateIO {
				t.Fatal("malformed descriptor must receive fixed privacy refusal")
			}
		})
	}
}
